package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	ErrNameTaken          = errors.New("account name taken")
	ErrAccountInUse       = errors.New("account has history")
	ErrAccountDisabled    = errors.New("account disabled")
	ErrLastCredential     = errors.New("last credential of the account")
	ErrCodeInvalid        = errors.New("enrollment code unknown, used or expired")
	ErrPasswordNotAllowed = errors.New("password not allowed for the account")
	ErrHandleMismatch     = errors.New("passkey user handle differs from the account's")
	ErrSignCount          = errors.New("passkey sign count did not advance")
	ErrSessionEnded       = errors.New("the requesting session has ended")
)

// CodeLifetime is how long an enrollment code stays usable after issue.
const CodeLifetime = 24 * time.Hour

const (
	Passkey  = "passkey"
	Password = "password"
)

// Account is a user row with what it holds for signing in over the network.
type Account struct {
	ID, Name                  string
	Disabled, PasswordAllowed bool
	Handle                    []byte // the WebAuthn user handle; nil until the first passkey
	Credentials, Sessions     int
	Created                   string
}

// Credential is a passkey (its WebAuthn record) or a password (its Argon2id PHC string).
type Credential struct {
	ID, User, Kind, Name    string
	CredentialID, PublicKey []byte
	AAGUID                  []byte
	SignCount               uint32
	Flags                   byte
	Transports              []string
	PasswordHash            string
	Created, LastUsed       string
}

// Session is a network sign-in; only the SHA-256 of its token is stored.
type Session struct {
	ID                                  string
	TokenHash                           []byte
	User, Credential, Created, LastUsed string
	Address                             string
}

type execQuerier interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// Live is the cutoff a session is counted and listed by: its last use after UsedAfter and its
// creation after CreatedAfter. The zero value counts every session.
type Live struct{ UsedAfter, CreatedAfter time.Time }

// liveSession is the SQL condition of Live on sessions s; its arguments are live.args().
const liveSession = "julianday(s.last_used) > julianday(?) AND julianday(s.created) > julianday(?)"

func (live Live) args() []any {
	return []any{SessionStamp(live.UsedAfter), SessionStamp(live.CreatedAfter)}
}

// SessionStamp is a session time as stored: whole milliseconds, the precision SQLite's date
// functions compare at, so the lifetime checks in Go and in SQL agree.
func SessionStamp(t time.Time) string { return stamp(t.Truncate(time.Millisecond)) }

const accountColumns = `u.id,u.name,u.disabled,u.password_allowed,u.webauthn_handle,
	(SELECT count(*) FROM credentials c WHERE c.user=u.id),(SELECT count(*) FROM sessions s WHERE s.user=u.id AND ` + liveSession + `),u.created`

func scanAccount(row interface{ Scan(...any) error }) (Account, error) {
	var a Account
	err := row.Scan(&a.ID, &a.Name, &a.Disabled, &a.PasswordAllowed, &a.Handle, &a.Credentials, &a.Sessions, &a.Created)
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrNotFound
	}
	return a, err
}

func account(ctx context.Context, q execQuerier, where string, arg any) (Account, error) {
	return liveAccount(ctx, q, Live{}, where, arg)
}

func liveAccount(ctx context.Context, q execQuerier, live Live, where string, arg any) (Account, error) {
	return scanAccount(q.QueryRowContext(ctx, "SELECT "+accountColumns+" FROM users u WHERE "+where, append(live.args(), arg)...))
}

// Accounts lists every account, oldest first, counting its sessions live at the cutoff.
func (s *Store) Accounts(ctx context.Context, live Live) ([]Account, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+accountColumns+" FROM users u ORDER BY u.created,u.id", live.args()...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	accounts := []Account{}
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		accounts = append(accounts, a)
	}
	return accounts, rows.Err()
}

func (s *Store) Account(ctx context.Context, id string) (Account, error) {
	return account(ctx, s.db, "u.id=?", id)
}

func (s *Store) AccountByName(ctx context.Context, name string) (Account, error) {
	return account(ctx, s.db, "u.name=?", name)
}

func (s *Store) AccountByHandle(ctx context.Context, handle []byte) (Account, error) {
	if len(handle) == 0 {
		return Account{}, ErrNotFound
	}
	return account(ctx, s.db, "u.webauthn_handle=?", handle)
}

// Account reads an account inside the transaction, counting its sessions live at the cutoff.
func (tx *Tx) Account(ctx context.Context, id string, live Live) (Account, error) {
	return liveAccount(ctx, tx.tx, live, "u.id=?", id)
}

// CreateAccount adds an account with no credential; ErrNameTaken when the name is in use.
func (tx *Tx) CreateAccount(ctx context.Context, id, name string, passwordAllowed bool, at time.Time) error {
	var taken bool
	if err := tx.tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM users WHERE name=?)", name).Scan(&taken); err != nil {
		return err
	}
	if taken {
		return ErrNameTaken
	}
	_, err := tx.tx.ExecContext(ctx, "INSERT INTO users(id,name,created,password_allowed) VALUES(?,?,?,?)", id, name, stamp(at), passwordAllowed)
	return err
}

// IssueEnrollmentCode stores a code's hash for the account, expiring a day after at, and
// deletes the account's unused codes.
func (tx *Tx) IssueEnrollmentCode(ctx context.Context, user string, codeHash []byte, at time.Time) error {
	if _, err := account(ctx, tx.tx, "u.id=?", user); err != nil {
		return err
	}
	if _, err := tx.tx.ExecContext(ctx, "DELETE FROM enrollment_codes WHERE user=? AND used IS NULL", user); err != nil {
		return err
	}
	_, err := tx.tx.ExecContext(ctx, "INSERT INTO enrollment_codes(code_hash,user,created,expires) VALUES(?,?,?,?)", codeHash, user, stamp(at), stamp(at.Add(CodeLifetime)))
	return err
}

// UpdateAccount sets the flags given and answers the account with its sessions live at the cutoff.
// Disabling ends every session of the account; their ids are returned.
func (tx *Tx) UpdateAccount(ctx context.Context, id string, disabled, passwordAllowed *bool, live Live) (Account, []string, error) {
	if _, err := account(ctx, tx.tx, "u.id=?", id); err != nil {
		return Account{}, nil, err
	}
	var ended []string
	if disabled != nil {
		if _, err := tx.tx.ExecContext(ctx, "UPDATE users SET disabled=? WHERE id=?", *disabled, id); err != nil {
			return Account{}, nil, err
		}
		if *disabled {
			var err error
			if ended, err = endSessions(ctx, tx.tx, id); err != nil {
				return Account{}, nil, err
			}
		}
	}
	if passwordAllowed != nil {
		if _, err := tx.tx.ExecContext(ctx, "UPDATE users SET password_allowed=? WHERE id=?", *passwordAllowed, id); err != nil {
			return Account{}, nil, err
		}
	}
	a, err := liveAccount(ctx, tx.tx, live, "u.id=?", id)
	return a, ended, err
}

// EndAccountSessions deletes every session of the account and returns their ids.
func (tx *Tx) EndAccountSessions(ctx context.Context, id string) ([]string, error) {
	if _, err := account(ctx, tx.tx, "u.id=?", id); err != nil {
		return nil, err
	}
	return endSessions(ctx, tx.tx, id)
}

// RemoveAccount deletes an account with no history, with its credentials, sessions and codes;
// ErrAccountInUse while anything else refers to it.
func (tx *Tx) RemoveAccount(ctx context.Context, id string) ([]string, error) {
	if _, err := account(ctx, tx.tx, "u.id=?", id); err != nil {
		return nil, err
	}
	var used bool
	if err := tx.tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM project_members WHERE user=?1)
		OR EXISTS(SELECT 1 FROM tasks WHERE created_by=?1) OR EXISTS(SELECT 1 FROM attempts WHERE actor=?1)
		OR EXISTS(SELECT 1 FROM findings WHERE author=?1) OR EXISTS(SELECT 1 FROM reviews WHERE reviewer=?1)
		OR EXISTS(SELECT 1 FROM source_snapshots WHERE created_by=?1) OR EXISTS(SELECT 1 FROM secret_allowlist WHERE author=?1)`, id).Scan(&used); err != nil {
		return nil, err
	}
	if used {
		return nil, ErrAccountInUse
	}
	ended, err := endSessions(ctx, tx.tx, id)
	if err != nil {
		return nil, err
	}
	for _, q := range []string{"DELETE FROM enrollment_codes WHERE user=?", "DELETE FROM credentials WHERE user=?", "DELETE FROM idempotency WHERE user=?", "DELETE FROM users WHERE id=?"} {
		if _, err := tx.tx.ExecContext(ctx, q, id); err != nil {
			return nil, err
		}
	}
	return ended, nil
}

// DeleteSession deletes any session by id.
func (tx *Tx) DeleteSession(ctx context.Context, id string) error {
	return deleteSession(ctx, tx.tx, id, "")
}

func endSessions(ctx context.Context, q execQuerier, user string) ([]string, error) {
	rows, err := q.QueryContext(ctx, "DELETE FROM sessions WHERE user=? RETURNING id", user)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ended := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ended = append(ended, id)
	}
	return ended, rows.Err()
}

func deleteSession(ctx context.Context, q execQuerier, id, user string) error {
	query, args := "DELETE FROM sessions WHERE id=?", []any{id}
	if user != "" {
		query, args = query+" AND user=?", append(args, user)
	}
	result, err := q.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return ErrNotFound
	}
	return nil
}

const credentialColumns = "id,user,kind,name,credential_id,public_key,COALESCE(sign_count,0),aaguid,COALESCE(flags,0),COALESCE(transports,''),COALESCE(password_hash,''),created,COALESCE(last_used,'')"

func scanCredential(row interface{ Scan(...any) error }) (Credential, error) {
	var c Credential
	var transports string
	var signCount int64
	var flags int64
	err := row.Scan(&c.ID, &c.User, &c.Kind, &c.Name, &c.CredentialID, &c.PublicKey, &signCount, &c.AAGUID, &flags, &transports, &c.PasswordHash, &c.Created, &c.LastUsed)
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrNotFound
	}
	c.SignCount, c.Flags = uint32(signCount), byte(flags)
	if transports != "" {
		c.Transports = strings.Split(transports, ",")
	}
	return c, err
}

// Credentials lists the account's credentials, oldest first.
func (s *Store) Credentials(ctx context.Context, user string) ([]Credential, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+credentialColumns+" FROM credentials WHERE user=? ORDER BY created,id", user)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	credentials := []Credential{}
	for rows.Next() {
		c, err := scanCredential(rows)
		if err != nil {
			return nil, err
		}
		credentials = append(credentials, c)
	}
	return credentials, rows.Err()
}

func (s *Store) PasswordCredential(ctx context.Context, user string) (Credential, error) {
	return scanCredential(s.db.QueryRowContext(ctx, "SELECT "+credentialColumns+" FROM credentials WHERE user=? AND kind='password'", user))
}

func (s *Store) PasskeyByCredentialID(ctx context.Context, credentialID []byte) (Credential, error) {
	return scanCredential(s.db.QueryRowContext(ctx, "SELECT "+credentialColumns+" FROM credentials WHERE credential_id=? AND kind='passkey'", credentialID))
}

// PasskeySignIn records a verified assertion of sess.Credential and creates sess in one
// transaction. The sign count must advance past the stored one at commit, zero staying zero, or
// ErrSignCount: two assertions verified against the same count cannot both sign in.
func (s *Store) PasskeySignIn(ctx context.Context, signCount uint32, flags byte, sess Session, at time.Time) error {
	credential := sess.Credential
	return s.write(ctx, sess.User, func(tx *Tx) error {
		if err := requireUsable(ctx, tx.tx, sess.User, credential, Passkey); err != nil {
			return err
		}
		result, err := tx.tx.ExecContext(ctx, `UPDATE credentials SET sign_count=?1,flags=?2,last_used=?3
			WHERE id=?4 AND kind='passkey' AND (COALESCE(sign_count,0) < ?1 OR (COALESCE(sign_count,0) = 0 AND ?1 = 0))`, int64(signCount), int64(flags), stamp(at), credential)
		if err != nil {
			return err
		}
		if n, err := result.RowsAffected(); err != nil {
			return err
		} else if n == 0 {
			return ErrSignCount
		}
		return insertSession(ctx, tx.tx, sess, at)
	})
}

// PasswordSignIn records a verified use of the password sess.Credential, storing rehash when it
// is not empty (the same password under the current parameters, not a credential change), and
// creates sess in one transaction that rechecks the account still allows it.
func (s *Store) PasswordSignIn(ctx context.Context, rehash string, sess Session, at time.Time) error {
	credential := sess.Credential
	return s.write(ctx, sess.User, func(tx *Tx) error {
		if err := requireUsable(ctx, tx.tx, sess.User, credential, Password); err != nil {
			return err
		}
		query, args := "UPDATE credentials SET last_used=? WHERE id=?", []any{stamp(at), credential}
		if rehash != "" {
			query, args = "UPDATE credentials SET password_hash=?,last_used=? WHERE id=?", []any{rehash, stamp(at), credential}
		}
		if _, err := tx.tx.ExecContext(ctx, query, args...); err != nil {
			return err
		}
		return insertSession(ctx, tx.tx, sess, at)
	})
}

// requireUsable checks that the credential is the account's, of the kind given (any for ""), and
// that the account, enabled, may sign in with it now: a password only while the account allows one.
func requireUsable(ctx context.Context, q execQuerier, user, credential, want string) error {
	var disabled, passwordAllowed bool
	var kind string
	err := q.QueryRowContext(ctx, "SELECT u.disabled,u.password_allowed,c.kind FROM users u JOIN credentials c ON c.user=u.id WHERE u.id=? AND c.id=?", user, credential).Scan(&disabled, &passwordAllowed, &kind)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if want != "" && kind != want {
		return ErrNotFound
	}
	if disabled {
		return ErrAccountDisabled
	}
	if kind == Password && !passwordAllowed {
		return ErrPasswordNotAllowed
	}
	return nil
}

// RemoveCredential deletes one of the account's credentials, for its session by, which must
// still be live, and every session of the account; ErrLastCredential when no credential the
// account may sign in with would remain.
func (s *Store) RemoveCredential(ctx context.Context, by Session, id string, live func() Live) ([]string, error) {
	user := by.User
	var ended []string
	err := s.write(ctx, user, func(tx *Tx) error {
		if err := requireSession(ctx, tx.tx, by, live); err != nil {
			return err
		}
		var owned bool
		if err := tx.tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM credentials WHERE id=? AND user=?)", id, user).Scan(&owned); err != nil {
			return err
		}
		if !owned {
			return ErrNotFound
		}
		var usable int
		if err := tx.tx.QueryRowContext(ctx, `SELECT count(*) FROM credentials c JOIN users u ON u.id=c.user
			WHERE c.user=? AND c.id<>? AND (c.kind='passkey' OR u.password_allowed=1)`, user, id).Scan(&usable); err != nil {
			return err
		}
		if usable == 0 {
			return ErrLastCredential
		}
		var err error
		if ended, err = endSessions(ctx, tx.tx, user); err != nil {
			return err
		}
		_, err = tx.tx.ExecContext(ctx, "DELETE FROM credentials WHERE id=?", id)
		return err
	})
	return ended, err
}

// CheckEnrollmentCode returns the account an unused, unexpired code was issued for, without using it.
func (s *Store) CheckEnrollmentCode(ctx context.Context, codeHash []byte, at time.Time) (Account, error) {
	return checkCode(ctx, s.db, codeHash, at)
}

func checkCode(ctx context.Context, q execQuerier, codeHash []byte, at time.Time) (Account, error) {
	var user, expires string
	var used sql.NullString
	err := q.QueryRowContext(ctx, "SELECT user,expires,used FROM enrollment_codes WHERE code_hash=?", codeHash).Scan(&user, &expires, &used)
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, ErrCodeInvalid
	}
	if err != nil {
		return Account{}, err
	}
	until, err := time.Parse(time.RFC3339Nano, expires)
	if err != nil {
		return Account{}, fmt.Errorf("enrollment code expiry: %w", err)
	}
	if used.Valid || !at.Before(until) {
		return Account{}, ErrCodeInvalid
	}
	a, err := account(ctx, q, "u.id=?", user)
	if err != nil {
		return Account{}, err
	}
	if a.Disabled {
		return Account{}, ErrCodeInvalid
	}
	return a, nil
}

// EnrollPasskey uses the code, sets the account's handle if it has none, adds the passkey,
// ends the account's sessions and creates sess, all in one transaction, at the time clock gives
// once the transaction holds the lock. It returns the ended ids.
func (s *Store) EnrollPasskey(ctx context.Context, codeHash []byte, clock func() time.Time, handle []byte, c Credential, sess Session) ([]string, error) {
	return s.enroll(ctx, codeHash, clock, c, sess, func(tx *sql.Tx, a Account, at time.Time) error {
		if a.Handle != nil && !bytes.Equal(a.Handle, handle) {
			return ErrHandleMismatch
		}
		if a.Handle == nil {
			if _, err := tx.ExecContext(ctx, "UPDATE users SET webauthn_handle=? WHERE id=?", handle, a.ID); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO credentials(id,user,kind,name,credential_id,public_key,sign_count,aaguid,flags,transports,created)
			VALUES(?,?,'passkey',?,?,?,?,?,?,?,?)`, c.ID, a.ID, c.Name, c.CredentialID, c.PublicKey, int64(c.SignCount), c.AAGUID, int64(c.Flags), strings.Join(c.Transports, ","), stamp(at))
		return err
	})
}

// EnrollPassword is EnrollPasskey for a password, replacing the account's password; refused
// where the account does not allow one.
func (s *Store) EnrollPassword(ctx context.Context, codeHash []byte, clock func() time.Time, c Credential, sess Session) ([]string, error) {
	return s.enroll(ctx, codeHash, clock, c, sess, func(tx *sql.Tx, a Account, at time.Time) error {
		if !a.PasswordAllowed {
			return ErrPasswordNotAllowed
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM credentials WHERE user=? AND kind='password'", a.ID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, "INSERT INTO credentials(id,user,kind,name,password_hash,created) VALUES(?,?,'password',?,?,?)", c.ID, a.ID, c.Name, c.PasswordHash, stamp(at))
		return err
	})
}

func (s *Store) enroll(ctx context.Context, codeHash []byte, clock func() time.Time, c Credential, sess Session, add func(*sql.Tx, Account, time.Time) error) ([]string, error) {
	var ended []string
	err := s.write(ctx, c.User, func(tx *Tx) error {
		at := clock() // read with the lock held, so a code cannot expire while the write waits
		a, err := checkCode(ctx, tx.tx, codeHash, at)
		if err != nil {
			return err
		}
		if a.ID != c.User || sess.User != c.User || sess.Credential != c.ID {
			return fmt.Errorf("enrollment for %s does not match its code", c.User)
		}
		// Sessions go first: a replaced password's sessions still refer to it.
		if ended, err = endSessions(ctx, tx.tx, a.ID); err != nil {
			return err
		}
		if err := add(tx.tx, a, at); err != nil {
			return err
		}
		if _, err := tx.tx.ExecContext(ctx, "UPDATE enrollment_codes SET used=? WHERE code_hash=?", stamp(at), codeHash); err != nil {
			return err
		}
		return insertSession(ctx, tx.tx, sess, at)
	})
	return ended, err
}

// CreateSession adds a session for a credential the account may sign in with now.
func (s *Store) CreateSession(ctx context.Context, sess Session, at time.Time) error {
	return s.write(ctx, sess.User, func(tx *Tx) error {
		if err := requireUsable(ctx, tx.tx, sess.User, sess.Credential, ""); err != nil {
			return err
		}
		return insertSession(ctx, tx.tx, sess, at)
	})
}

func insertSession(ctx context.Context, q execQuerier, sess Session, at time.Time) error {
	_, err := q.ExecContext(ctx, "INSERT INTO sessions(id,token_hash,user,credential,created,last_used,address) VALUES(?,?,?,?,?,?,?)", sess.ID, sess.TokenHash, sess.User, sess.Credential, SessionStamp(at), SessionStamp(at), sess.Address)
	return err
}

const sessionColumns = "id,token_hash,user,credential,created,last_used,address"

func scanSession(row interface{ Scan(...any) error }) (Session, error) {
	var sess Session
	err := row.Scan(&sess.ID, &sess.TokenHash, &sess.User, &sess.Credential, &sess.Created, &sess.LastUsed, &sess.Address)
	if errors.Is(err, sql.ErrNoRows) {
		return sess, ErrNotFound
	}
	return sess, err
}

func (s *Store) SessionByToken(ctx context.Context, tokenHash []byte) (Session, error) {
	return scanSession(s.db.QueryRowContext(ctx, "SELECT "+sessionColumns+" FROM sessions WHERE token_hash=?", tokenHash))
}

func (s *Store) Session(ctx context.Context, id string) (Session, error) {
	return scanSession(s.db.QueryRowContext(ctx, "SELECT "+sessionColumns+" FROM sessions WHERE id=?", id))
}

// Sessions lists the account's sessions live at the cutoff, or every account's for "", oldest first.
func (s *Store) Sessions(ctx context.Context, user string, live Live) ([]Session, error) {
	query, args := "SELECT "+sessionColumns+" FROM sessions s WHERE "+liveSession, live.args()
	if user != "" {
		query, args = query+" AND user=?", append(args, user)
	}
	rows, err := s.db.QueryContext(ctx, query+" ORDER BY created,id", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	sessions := []Session{}
	for rows.Next() {
		sess, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		sessions = append(sessions, sess)
	}
	return sessions, rows.Err()
}

// TouchSession moves the session's last use to at if it still holds the observed value; a newer
// use another request wrote is kept. ErrNotFound when the session is gone.
func (s *Store) TouchSession(ctx context.Context, id, observed string, at time.Time) error {
	return s.write(ctx, "", func(tx *Tx) error {
		if _, err := tx.tx.ExecContext(ctx, "UPDATE sessions SET last_used=? WHERE id=? AND last_used=?", SessionStamp(at), id, observed); err != nil {
			return err
		}
		var exists bool
		if err := tx.tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM sessions WHERE id=?)", id).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return ErrNotFound
		}
		return nil
	})
}

// DeleteExpiredSession deletes the session found expired only if its last use is still the one
// it was judged by; it reports whether the row is gone.
func (s *Store) DeleteExpiredSession(ctx context.Context, id, observed string) (bool, error) {
	if _, err := s.db.ExecContext(ctx, "DELETE FROM sessions WHERE id=? AND last_used=?", id, observed); err != nil {
		return false, err
	}
	var exists bool
	err := s.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM sessions WHERE id=?)", id).Scan(&exists)
	return !exists, err
}

// DeleteSession deletes a session by id; ErrNotFound when there is none.
func (s *Store) DeleteSession(ctx context.Context, id string) error {
	return deleteSession(ctx, s.db, id, "")
}

// DeleteUserSession deletes one of the account's own sessions for the session by, which must
// still be live; another account's is ErrNotFound.
func (s *Store) DeleteUserSession(ctx context.Context, by Session, id string, live func() Live) error {
	return s.write(ctx, by.User, func(tx *Tx) error {
		if err := requireSession(ctx, tx.tx, by, live); err != nil {
			return err
		}
		return deleteSession(ctx, tx.tx, id, by.User)
	})
}

// requireSession checks, in the transaction of a change made for it, that the session making the
// change is still the account's and live at the cutoff read now, once the transaction holds the
// lock: ErrSessionEnded otherwise.
func requireSession(ctx context.Context, q execQuerier, by Session, live func() Live) error {
	var ok bool
	if err := q.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM sessions s WHERE s.id=? AND s.user=? AND "+liveSession+")", append([]any{by.ID, by.User}, live().args()...)...).Scan(&ok); err != nil {
		return err
	}
	if !ok {
		return ErrSessionEnded
	}
	return nil
}
