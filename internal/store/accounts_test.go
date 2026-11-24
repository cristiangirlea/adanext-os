package store

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"
)

var accountClock = time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)

func hash32(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}

func createAccount(t *testing.T, s *Store, id, name string, passwordAllowed bool) {
	t.Helper()
	if err := s.write(ctx, "local", func(tx *Tx) error { return tx.CreateAccount(ctx, id, name, passwordAllowed, accountClock) }); err != nil {
		t.Fatal(err)
	}
}

func issueCode(t *testing.T, s *Store, user, code string, at time.Time) {
	t.Helper()
	if err := s.write(ctx, "local", func(tx *Tx) error { return tx.IssueEnrollmentCode(ctx, user, hash32(code), at) }); err != nil {
		t.Fatal(err)
	}
}

func passkey(user, id string) Credential {
	return Credential{ID: id, User: user, Kind: Passkey, Name: "key " + id, CredentialID: []byte("raw-" + id), PublicKey: []byte("cose-" + id), AAGUID: make([]byte, 16), Flags: 0x45, Transports: []string{"internal"}}
}

func session(user, credential, id string) Session {
	return Session{ID: id, TokenHash: hash32("token-" + id), User: user, Credential: credential, Address: "192.0.2.1"}
}

// enrolled gives user a passkey through a code and two sessions on it.
func enrolled(t *testing.T, s *Store, user string) {
	t.Helper()
	issueCode(t, s, user, "code-"+user, accountClock)
	handle := bytes.Repeat([]byte{user[0]}, 64)
	if _, err := s.EnrollPasskey(ctx, hash32("code-"+user), clockAt(accountClock), handle, passkey(user, "pk-"+user), session(user, "pk-"+user, "s1-"+user)); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSession(ctx, session(user, "pk-"+user, "s2-"+user), accountClock); err != nil {
		t.Fatal(err)
	}
}

func sessionIDs(t *testing.T, s *Store, user string) []string {
	t.Helper()
	sessions, err := s.Sessions(ctx, user, Live{})
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{}
	for _, session := range sessions {
		ids = append(ids, session.ID)
	}
	slices.Sort(ids)
	return ids
}

func TestTheAccountsMigrationLeavesEveryUserEnabledWithoutAPasswordOrHandle(t *testing.T) {
	s := openTestStore(t)
	seed(t, s)
	path := s.path
	for _, q := range []string{"DROP TABLE enrollment_codes", "DROP TABLE sessions", "DROP TABLE credentials", "DROP INDEX users_webauthn_handle", "ALTER TABLE users DROP COLUMN webauthn_handle", "ALTER TABLE users DROP COLUMN password_allowed", "ALTER TABLE users DROP COLUMN disabled", "DELETE FROM schema_migrations WHERE version>=14", "PRAGMA user_version=13"} {
		execute(t, s, q)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	upgraded, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	account, err := upgraded.Account(ctx, "u")
	if err != nil || account.Disabled || account.PasswordAllowed || account.Handle != nil || account.Credentials != 0 || account.Sessions != 0 {
		t.Fatalf("account after the migration = %+v, %v", account, err)
	}
}

func TestTheCredentialSchemaRefusesMixedKindsAndASecondPassword(t *testing.T) {
	s := openTestStore(t)
	seed(t, s)
	for _, q := range []string{
		`INSERT INTO credentials(id,user,kind,name,credential_id,public_key,password_hash,created) VALUES ('c','u','passkey','k',x'01',x'02','$argon2id$','now')`,
		`INSERT INTO credentials(id,user,kind,name,password_hash,credential_id,created) VALUES ('c','u','password','p','$argon2id$',x'01','now')`,
		`INSERT INTO credentials(id,user,kind,name,created) VALUES ('c','u','passkey','k','now')`,
		`INSERT INTO credentials(id,user,kind,name,created) VALUES ('c','u','password','p','now')`,
	} {
		if _, err := s.db.ExecContext(ctx, q); err == nil {
			t.Fatalf("accepted %s", q)
		}
	}
	execute(t, s, `INSERT INTO credentials(id,user,kind,name,password_hash,created) VALUES ('p1','u','password','p','$argon2id$a','now')`)
	if _, err := s.db.ExecContext(ctx, `INSERT INTO credentials(id,user,kind,name,password_hash,created) VALUES ('p2','u','password','p','$argon2id$b','now')`); err == nil {
		t.Fatal("a second password was accepted")
	}
}

func TestCreatingAnAccountRefusesATakenName(t *testing.T) {
	s := openTestStore(t)
	seed(t, s)
	createAccount(t, s, "v", "Vera", true)
	err := s.write(ctx, "local", func(tx *Tx) error { return tx.CreateAccount(ctx, "w", "Vera", false, accountClock) })
	if !errors.Is(err, ErrNameTaken) {
		t.Fatalf("a taken name: %v", err)
	}
	account, err := s.Account(ctx, "v")
	if err != nil || account.Name != "Vera" || !account.PasswordAllowed || account.Disabled || account.Created == "" {
		t.Fatalf("account = %+v, %v", account, err)
	}
}

func TestAnEnrollmentCodeIsSingleUseAndExpiresAfterADay(t *testing.T) {
	s := openTestStore(t)
	seed(t, s)
	createAccount(t, s, "v", "Vera", false)
	issueCode(t, s, "v", "first", accountClock)
	if account, err := s.CheckEnrollmentCode(ctx, hash32("first"), accountClock.Add(23*time.Hour)); err != nil || account.ID != "v" {
		t.Fatalf("a fresh code: %+v, %v", account, err)
	}
	if _, err := s.CheckEnrollmentCode(ctx, hash32("first"), accountClock.Add(24*time.Hour)); !errors.Is(err, ErrCodeInvalid) {
		t.Fatalf("a code at its expiry: %v", err)
	}
	if _, err := s.EnrollPasskey(ctx, hash32("first"), clockAt(accountClock.Add(25*time.Hour)), bytes.Repeat([]byte{1}, 64), passkey("v", "k1"), session("v", "k1", "s1")); !errors.Is(err, ErrCodeInvalid) {
		t.Fatalf("an expired code enrolled: %v", err)
	}
	issueCode(t, s, "v", "second", accountClock)
	if _, err := s.CheckEnrollmentCode(ctx, hash32("first"), accountClock); !errors.Is(err, ErrCodeInvalid) {
		t.Fatalf("an unused code survived a new one: %v", err)
	}
	if _, err := s.EnrollPasskey(ctx, hash32("second"), clockAt(accountClock), bytes.Repeat([]byte{1}, 64), passkey("v", "k1"), session("v", "k1", "s1")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnrollPasskey(ctx, hash32("second"), clockAt(accountClock), bytes.Repeat([]byte{1}, 64), passkey("v", "k2"), session("v", "k2", "s2")); !errors.Is(err, ErrCodeInvalid) {
		t.Fatalf("a used code enrolled again: %v", err)
	}
	if _, err := s.CheckEnrollmentCode(ctx, hash32("unknown"), accountClock); !errors.Is(err, ErrCodeInvalid) {
		t.Fatalf("an unknown code: %v", err)
	}
	var stored []byte
	if err := s.db.QueryRowContext(ctx, "SELECT code_hash FROM enrollment_codes WHERE user='v'").Scan(&stored); err != nil || bytes.Equal(stored, []byte("second")) {
		t.Fatalf("stored code %q, %v", stored, err)
	}
}

func TestAddingAPasskeyEndsEverySessionOfTheAccountAndKeepsTheNewOne(t *testing.T) {
	s := openTestStore(t)
	seed(t, s)
	createAccount(t, s, "v", "Vera", false)
	enrolled(t, s, "v")
	enrolled(t, s, "u")
	issueCode(t, s, "v", "again", accountClock)
	handle := bytes.Repeat([]byte{'v'}, 64)
	ended, err := s.EnrollPasskey(ctx, hash32("again"), clockAt(accountClock), handle, passkey("v", "pk2"), session("v", "pk2", "s3-v"))
	slices.Sort(ended)
	if err != nil || !slices.Equal(ended, []string{"s1-v", "s2-v"}) {
		t.Fatalf("ended %v, %v", ended, err)
	}
	if got := sessionIDs(t, s, "v"); !slices.Equal(got, []string{"s3-v"}) {
		t.Fatalf("sessions of v = %v", got)
	}
	if got := sessionIDs(t, s, "u"); !slices.Equal(got, []string{"s1-u", "s2-u"}) {
		t.Fatalf("another account's sessions = %v", got)
	}
	account, err := s.Account(ctx, "v")
	if err != nil || !bytes.Equal(account.Handle, handle) || account.Credentials != 2 || account.Sessions != 1 {
		t.Fatalf("account = %+v, %v", account, err)
	}
	if byHandle, err := s.AccountByHandle(ctx, handle); err != nil || byHandle.ID != "v" {
		t.Fatalf("by handle = %+v, %v", byHandle, err)
	}
	issueCode(t, s, "v", "other", accountClock)
	if _, err := s.EnrollPasskey(ctx, hash32("other"), clockAt(accountClock), bytes.Repeat([]byte{'x'}, 64), passkey("v", "pk3"), session("v", "pk3", "s4-v")); !errors.Is(err, ErrHandleMismatch) {
		t.Fatalf("a different handle: %v", err)
	}
}

func TestAPasswordEnrollmentReplacesThePasswordOnlyWhereAllowed(t *testing.T) {
	s := openTestStore(t)
	seed(t, s)
	createAccount(t, s, "v", "Vera", false)
	issueCode(t, s, "v", "c1", accountClock)
	password := Credential{ID: "pw1", User: "v", Kind: Password, Name: "Password", PasswordHash: "$argon2id$one"}
	if _, err := s.EnrollPassword(ctx, hash32("c1"), clockAt(accountClock), password, session("v", "pw1", "s1")); !errors.Is(err, ErrPasswordNotAllowed) {
		t.Fatalf("a password where not allowed: %v", err)
	}
	if _, err := s.CheckEnrollmentCode(ctx, hash32("c1"), accountClock); err != nil {
		t.Fatalf("a refused enrollment used the code: %v", err)
	}
	execute(t, s, "UPDATE users SET password_allowed=1 WHERE id='v'")
	if _, err := s.EnrollPassword(ctx, hash32("c1"), clockAt(accountClock), password, session("v", "pw1", "s1")); err != nil {
		t.Fatal(err)
	}
	issueCode(t, s, "v", "c2", accountClock)
	second := Credential{ID: "pw2", User: "v", Kind: Password, Name: "Password", PasswordHash: "$argon2id$two"}
	ended, err := s.EnrollPassword(ctx, hash32("c2"), clockAt(accountClock), second, session("v", "pw2", "s2"))
	if err != nil || !slices.Equal(ended, []string{"s1"}) {
		t.Fatalf("ended %v, %v", ended, err)
	}
	current, err := s.PasswordCredential(ctx, "v")
	if err != nil || current.ID != "pw2" || current.PasswordHash != "$argon2id$two" {
		t.Fatalf("password = %+v, %v", current, err)
	}
}

func TestADisabledAccountLosesItsSessionsAndGetsNoNewOne(t *testing.T) {
	s := openTestStore(t)
	seed(t, s)
	createAccount(t, s, "v", "Vera", false)
	enrolled(t, s, "v")
	var ended []string
	disabled := true
	if err := s.write(ctx, "local", func(tx *Tx) error {
		var err error
		_, ended, err = tx.UpdateAccount(ctx, "v", &disabled, nil, Live{})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	slices.Sort(ended)
	if !slices.Equal(ended, []string{"s1-v", "s2-v"}) || len(sessionIDs(t, s, "v")) != 0 {
		t.Fatalf("ended %v, left %v", ended, sessionIDs(t, s, "v"))
	}
	if err := s.CreateSession(ctx, session("v", "pk-v", "s3-v"), accountClock); !errors.Is(err, ErrAccountDisabled) {
		t.Fatalf("a session for a disabled account: %v", err)
	}
	issueCode(t, s, "v", "late", accountClock)
	if _, err := s.CheckEnrollmentCode(ctx, hash32("late"), accountClock); !errors.Is(err, ErrCodeInvalid) {
		t.Fatalf("a disabled account's code: %v", err)
	}
}

func TestRemovingACredentialEndsTheAccountsSessionsButNeverTheLastOne(t *testing.T) {
	s := openTestStore(t)
	seed(t, s)
	createAccount(t, s, "v", "Vera", true)
	enrolled(t, s, "v")
	if _, err := s.RemoveCredential(ctx, Session{ID: "s1-v", User: "v"}, "pk-v", func() Live { return Live{} }); !errors.Is(err, ErrLastCredential) {
		t.Fatalf("the last credential: %v", err)
	}
	enrolled(t, s, "u")
	if _, err := s.RemoveCredential(ctx, Session{ID: "s1-u", User: "u"}, "pk-v", func() Live { return Live{} }); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another account's credential: %v", err)
	}
	issueCode(t, s, "v", "pw", accountClock)
	if _, err := s.EnrollPassword(ctx, hash32("pw"), clockAt(accountClock), Credential{ID: "pw", User: "v", Kind: Password, Name: "Password", PasswordHash: "$argon2id$x"}, session("v", "pw", "s3-v")); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSession(ctx, session("v", "pk-v", "s4-v"), accountClock); err != nil {
		t.Fatal(err)
	}
	ended, err := s.RemoveCredential(ctx, Session{ID: "s4-v", User: "v"}, "pk-v", func() Live { return Live{} })
	slices.Sort(ended)
	if err != nil || !slices.Equal(ended, []string{"s3-v", "s4-v"}) || len(sessionIDs(t, s, "v")) != 0 {
		t.Fatalf("ended %v, %v", ended, err)
	}
}

func TestASignCountUpdateIsNotACredentialChange(t *testing.T) {
	s := openTestStore(t)
	seed(t, s)
	createAccount(t, s, "v", "Vera", false)
	enrolled(t, s, "v")
	if err := s.PasskeySignIn(ctx, 7, 0x45, session("v", "pk-v", "s3-v"), accountClock.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got := sessionIDs(t, s, "v"); len(got) != 3 {
		t.Fatalf("sessions after a login = %v", got)
	}
	credentials, err := s.Credentials(ctx, "v")
	if err != nil || len(credentials) != 1 || credentials[0].SignCount != 7 || credentials[0].LastUsed == "" || !slices.Equal(credentials[0].Transports, []string{"internal"}) {
		t.Fatalf("credentials = %+v, %v", credentials, err)
	}
	byID, err := s.PasskeyByCredentialID(ctx, []byte("raw-pk-v"))
	if err != nil || byID.ID != "pk-v" || !bytes.Equal(byID.PublicKey, []byte("cose-pk-v")) {
		t.Fatalf("by credential id = %+v, %v", byID, err)
	}
}

func TestSessionsAreFoundByTokenHashTouchedAndDeleted(t *testing.T) {
	s := openTestStore(t)
	seed(t, s)
	createAccount(t, s, "v", "Vera", false)
	enrolled(t, s, "v")
	got, err := s.SessionByToken(ctx, hash32("token-s1-v"))
	if err != nil || got.ID != "s1-v" || got.User != "v" || got.Address != "192.0.2.1" || got.Created != got.LastUsed {
		t.Fatalf("session = %+v, %v", got, err)
	}
	later := accountClock.Add(time.Hour)
	if err := s.TouchSession(ctx, "s1-v", got.LastUsed, later); err != nil {
		t.Fatal(err)
	}
	// A request that saw the older use writes nothing over the newer one.
	if err := s.TouchSession(ctx, "s1-v", got.LastUsed, accountClock.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Session(ctx, "s1-v"); err != nil || got.LastUsed != stamp(later) {
		t.Fatalf("touched = %+v, %v", got, err)
	}
	if gone, err := s.DeleteExpiredSession(ctx, "s1-v", got.LastUsed); err != nil || gone {
		t.Fatalf("a session used since it was judged expired was deleted: %v, %v", gone, err)
	}
	if err := s.DeleteUserSession(ctx, Session{ID: "s2-v", User: "u"}, "s1-v", func() Live { return Live{} }); !errors.Is(err, ErrSessionEnded) {
		t.Fatalf("another account's session: %v", err)
	}
	if err := s.DeleteUserSession(ctx, Session{ID: "s2-v", User: "v"}, "s1-v", func() Live { return Live{} }); err != nil {
		t.Fatal(err)
	}
	if err := s.TouchSession(ctx, "s1-v", stamp(later), accountClock.Add(2*time.Hour)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("touching a revoked session: %v", err)
	}
	if _, err := s.SessionByToken(ctx, hash32("token-s1-v")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a deleted session: %v", err)
	}
	all, err := s.Sessions(ctx, "", Live{})
	if err != nil || len(all) != 1 || all[0].ID != "s2-v" {
		t.Fatalf("every session = %+v, %v", all, err)
	}
}

func TestAnAccountWithHistoryIsRefusedRemovalAndOneWithoutGoesWithEverything(t *testing.T) {
	s := openTestStore(t)
	seed(t, s)
	createAccount(t, s, "v", "Vera", false)
	enrolled(t, s, "v")
	issueCode(t, s, "v", "spare", accountClock)
	execute(t, s, `INSERT INTO project_members VALUES ('p', 'v', 'member', 'added')`)
	remove := func(id string) ([]string, error) {
		var ended []string
		err := s.write(ctx, "local", func(tx *Tx) error {
			var err error
			ended, err = tx.RemoveAccount(ctx, id)
			return err
		})
		return ended, err
	}
	if _, err := remove("v"); !errors.Is(err, ErrAccountInUse) {
		t.Fatalf("a member: %v", err)
	}
	execute(t, s, `DELETE FROM project_members WHERE user='v'`)
	execute(t, s, `INSERT INTO findings(id,task,attempt,author,body,created) VALUES ('f','t','a','v','note','now')`)
	if _, err := remove("v"); !errors.Is(err, ErrAccountInUse) {
		t.Fatalf("an author of a finding: %v", err)
	}
	execute(t, s, `DELETE FROM findings`)
	if _, err := remove("u"); !errors.Is(err, ErrAccountInUse) {
		t.Fatalf("an account with tasks: %v", err)
	}
	ended, err := remove("v")
	slices.Sort(ended)
	if err != nil || !slices.Equal(ended, []string{"s1-v", "s2-v"}) {
		t.Fatalf("removed: %v, %v", ended, err)
	}
	for _, table := range []string{"credentials", "sessions", "enrollment_codes", "users"} {
		var n int
		column := "user"
		if table == "users" {
			column = "id"
		}
		if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM "+table+" WHERE "+column+"='v'").Scan(&n); err != nil || n != 0 {
			t.Fatalf("%s left %d, %v", table, n, err)
		}
	}
	if _, err := remove("v"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a removed account: %v", err)
	}
}

func TestAccountsListCountsCredentialsAndSessions(t *testing.T) {
	s := openTestStore(t)
	seed(t, s)
	createAccount(t, s, "v", "Vera", true)
	enrolled(t, s, "v")
	accounts, err := s.Accounts(ctx, Live{})
	if err != nil || len(accounts) != 2 {
		t.Fatalf("accounts = %+v, %v", accounts, err)
	}
	for _, a := range accounts {
		if a.ID == "v" && (a.Credentials != 1 || a.Sessions != 2 || !a.PasswordAllowed) {
			t.Fatalf("v = %+v", a)
		}
		if a.ID == "u" && (a.Credentials != 0 || a.Sessions != 0) {
			t.Fatalf("u = %+v", a)
		}
	}
	if byName, err := s.AccountByName(ctx, "Vera"); err != nil || byName.ID != "v" {
		t.Fatalf("by name = %+v, %v", byName, err)
	}
	if _, err := s.AccountByName(ctx, "nobody"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an unknown name: %v", err)
	}
}

func TestASignInWhoseSignCountDoesNotAdvanceAtCommitIsRefused(t *testing.T) {
	s := openTestStore(t)
	seed(t, s)
	createAccount(t, s, "v", "Vera", false)
	enrolled(t, s, "v")
	// Two assertions verified against the stored count; the higher commits first.
	if err := s.PasskeySignIn(ctx, 5, 0x45, session("v", "pk-v", "a"), accountClock); err != nil {
		t.Fatal(err)
	}
	if err := s.PasskeySignIn(ctx, 3, 0x45, session("v", "pk-v", "b"), accountClock); !errors.Is(err, ErrSignCount) {
		t.Fatalf("a lower count after a higher one: %v", err)
	}
	if err := s.PasskeySignIn(ctx, 5, 0x45, session("v", "pk-v", "c"), accountClock); !errors.Is(err, ErrSignCount) {
		t.Fatalf("the same count again: %v", err)
	}
	if c, _ := s.PasskeyByCredentialID(ctx, []byte("raw-pk-v")); c.SignCount != 5 {
		t.Fatalf("stored count %d", c.SignCount)
	}
	if _, err := s.Session(ctx, "b"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a refused sign-in left a session: %v", err)
	}
	createAccount(t, s, "w", "Wim", false)
	issueCode(t, s, "w", "code-w", accountClock)
	zero := passkey("w", "pk-w")
	zero.SignCount = 0
	if _, err := s.EnrollPasskey(ctx, hash32("code-w"), clockAt(accountClock), bytes.Repeat([]byte{'w'}, 64), zero, session("w", "pk-w", "w1")); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"w2", "w3"} {
		if err := s.PasskeySignIn(ctx, 0, 0x45, session("w", "pk-w", id), accountClock); err != nil {
			t.Fatalf("an authenticator without a counter: %v", err)
		}
	}
}

func TestAPasswordSignInIsRecheckedAgainstTheAccountInItsTransaction(t *testing.T) {
	s := openTestStore(t)
	seed(t, s)
	createAccount(t, s, "v", "Vera", true)
	issueCode(t, s, "v", "c", accountClock)
	if _, err := s.EnrollPassword(ctx, hash32("c"), clockAt(accountClock), Credential{ID: "pw", User: "v", Kind: Password, Name: "Password", PasswordHash: "$argon2id$x"}, session("v", "pw", "s1")); err != nil {
		t.Fatal(err)
	}
	if err := s.PasswordSignIn(ctx, "$argon2id$rehashed", session("v", "pw", "s2"), accountClock); err != nil {
		t.Fatal(err)
	}
	if c, _ := s.PasswordCredential(ctx, "v"); c.PasswordHash != "$argon2id$rehashed" || c.LastUsed == "" {
		t.Fatalf("password = %+v", c)
	}
	execute(t, s, "UPDATE users SET password_allowed=0 WHERE id='v'")
	for _, err := range []error{
		s.PasswordSignIn(ctx, "", session("v", "pw", "s3"), accountClock),
		s.CreateSession(ctx, session("v", "pw", "s3"), accountClock),
	} {
		if !errors.Is(err, ErrPasswordNotAllowed) {
			t.Fatalf("a password where none is allowed now: %v", err)
		}
	}
	execute(t, s, "UPDATE users SET password_allowed=1, disabled=1 WHERE id='v'")
	if err := s.PasswordSignIn(ctx, "", session("v", "pw", "s3"), accountClock); !errors.Is(err, ErrAccountDisabled) {
		t.Fatalf("a disabled account: %v", err)
	}
}

func TestTheLastCredentialTheAccountMaySignInWithCannotBeRemoved(t *testing.T) {
	s := openTestStore(t)
	seed(t, s)
	createAccount(t, s, "v", "Vera", true)
	enrolled(t, s, "v")
	issueCode(t, s, "v", "pw", accountClock)
	if _, err := s.EnrollPassword(ctx, hash32("pw"), clockAt(accountClock), Credential{ID: "pw", User: "v", Kind: Password, Name: "Password", PasswordHash: "$argon2id$x"}, session("v", "pw", "s")); err != nil {
		t.Fatal(err)
	}
	execute(t, s, "UPDATE users SET password_allowed=0 WHERE id='v'")
	if _, err := s.RemoveCredential(ctx, Session{ID: "s", User: "v"}, "pk-v", func() Live { return Live{} }); !errors.Is(err, ErrLastCredential) {
		t.Fatalf("the passkey beside a disallowed password: %v", err)
	}
	if _, err := s.RemoveCredential(ctx, Session{ID: "s", User: "v"}, "pw", func() Live { return Live{} }); err != nil {
		t.Fatalf("the disallowed password beside the passkey: %v", err)
	}
}

func TestASignInCommitsOnlyACredentialOfItsKindThatIsTheAccountsOwn(t *testing.T) {
	s := openTestStore(t)
	seed(t, s)
	createAccount(t, s, "v", "Vera", true)
	enrolled(t, s, "v")
	createAccount(t, s, "w", "Wim", true)
	enrolled(t, s, "w")
	before, _ := s.Credentials(ctx, "v")
	for name, err := range map[string]error{
		"a passkey as a password":      s.PasswordSignIn(ctx, "", session("v", "pk-v", "x1"), accountClock.Add(time.Hour)),
		"another account's passkey":    s.PasskeySignIn(ctx, 9, 0x45, session("v", "pk-w", "x2"), accountClock.Add(time.Hour)),
		"another account's as session": s.CreateSession(ctx, session("v", "pk-w", "x3"), accountClock.Add(time.Hour)),
	} {
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	for _, id := range []string{"x1", "x2", "x3"} {
		if _, err := s.Session(ctx, id); !errors.Is(err, ErrNotFound) {
			t.Fatalf("session %s was created: %v", id, err)
		}
	}
	after, _ := s.Credentials(ctx, "v")
	other, _ := s.Credentials(ctx, "w")
	if after[0].LastUsed != before[0].LastUsed || after[0].SignCount != before[0].SignCount || other[0].SignCount != 0 || other[0].LastUsed != "" {
		t.Fatalf("credential state changed: %+v, %+v", after, other)
	}
}

func TestListsAndCountsTakeOnlySessionsLiveAtTheCutoff(t *testing.T) {
	s := openTestStore(t)
	seed(t, s)
	createAccount(t, s, "v", "Vera", false)
	enrolled(t, s, "v") // s1-v and s2-v, created and used at accountClock
	if err := s.TouchSession(ctx, "s2-v", stamp(accountClock), accountClock.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	usedAfter := Live{UsedAfter: accountClock.Add(time.Hour)}
	createdAfter := Live{CreatedAfter: accountClock.Add(time.Millisecond)}
	if got, _ := s.Sessions(ctx, "v", usedAfter); len(got) != 1 || got[0].ID != "s2-v" {
		t.Fatalf("own sessions used after the cutoff = %+v", got)
	}
	if got, _ := s.Sessions(ctx, "", createdAfter); len(got) != 0 {
		t.Fatalf("sessions created after the cutoff = %+v", got)
	}
	accounts, _ := s.Accounts(ctx, usedAfter)
	for _, a := range accounts {
		if a.ID == "v" && a.Sessions != 1 {
			t.Fatalf("counted %d", a.Sessions)
		}
	}
	if err := s.write(ctx, "local", func(tx *Tx) error {
		a, _, err := tx.UpdateAccount(ctx, "v", nil, nil, createdAfter)
		if err == nil && a.Sessions != 0 {
			err = fmt.Errorf("the answer counts %d", a.Sessions)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if all, _ := s.Sessions(ctx, "", Live{}); len(all) != 2 {
		t.Fatalf("a listing deleted rows: %+v", all)
	}
}

func TestAChangeForASessionThatHasEndedIsRefused(t *testing.T) {
	s := openTestStore(t)
	seed(t, s)
	createAccount(t, s, "v", "Vera", true)
	enrolled(t, s, "v")
	issueCode(t, s, "v", "pw", accountClock)
	if _, err := s.EnrollPassword(ctx, hash32("pw"), clockAt(accountClock), Credential{ID: "pw", User: "v", Kind: Password, Name: "Password", PasswordHash: "$argon2id$x"}, session("v", "pw", "s3-v")); err != nil {
		t.Fatal(err)
	}
	// s1-v ended with the password's enrollment; s3-v is the session that created.
	if _, err := s.RemoveCredential(ctx, Session{ID: "s1-v", User: "v"}, "pk-v", func() Live { return Live{} }); !errors.Is(err, ErrSessionEnded) {
		t.Fatalf("a removal for an ended session: %v", err)
	}
	if err := s.DeleteUserSession(ctx, Session{ID: "s1-v", User: "v"}, "s3-v", func() Live { return Live{} }); !errors.Is(err, ErrSessionEnded) {
		t.Fatalf("a revocation for an ended session: %v", err)
	}
	if _, err := s.RemoveCredential(ctx, Session{ID: "s3-v", User: "v"}, "pk-v", func() Live { return Live{UsedAfter: accountClock.Add(time.Second)} }); !errors.Is(err, ErrSessionEnded) {
		t.Fatalf("a removal for an expired session: %v", err)
	}
	if got := sessionIDs(t, s, "v"); !slices.Equal(got, []string{"s3-v"}) {
		t.Fatalf("sessions %v", got)
	}
	if c, _ := s.Credentials(ctx, "v"); len(c) != 2 {
		t.Fatalf("credentials %+v", c)
	}
}

func clockAt(t time.Time) func() time.Time { return func() time.Time { return t } }
