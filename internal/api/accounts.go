package api

import (
	"context"
	"net/http"
	"time"

	"github.com/cristiangirlea/ada-os/internal/store"
)

// ownerRoutes are the owner's account administration, served on the local channel only: the
// network channel refuses each of them with local_only, a session of the owner's included.
func (h *handler) ownerRoutes() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		"GET /v1/accounts":                        h.listAccounts,
		"POST /v1/accounts":                       h.createAccount,
		"PATCH /v1/accounts/{id}":                 h.updateAccount,
		"DELETE /v1/accounts/{id}":                h.removeAccount,
		"POST /v1/accounts/{id}/enrollment-codes": h.issueCode(false),
		"POST /v1/accounts/{id}/recover":          h.issueCode(true),
		"GET /v1/sessions":                        h.listSessions,
		"DELETE /v1/sessions/{id}":                h.revokeSession,
	}
}

type accountJSON struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Owner           bool   `json:"owner"`
	Disabled        bool   `json:"disabled"`
	PasswordAllowed bool   `json:"password_allowed"`
	Credentials     int    `json:"credentials"`
	Sessions        int    `json:"sessions"`
	Created         string `json:"created"`
}

func (h *handler) accountValue(a store.Account) accountJSON {
	return accountJSON{a.ID, a.Name, h.isOwner(a.ID), a.Disabled, a.PasswordAllowed, a.Credentials, a.Sessions, a.Created}
}

func (h *handler) isOwner(id string) bool { return h.exec.Owner != "" && id == h.exec.Owner }

// codeJSON answers an issued code; the idempotency cache keeps it without the code.
type codeJSON struct {
	Account *accountJSON `json:"account,omitempty"`
	Code    string       `json:"code,omitempty"`
	Expires string       `json:"expires"`
}

type sessionListJSON struct {
	ID          string `json:"id"`
	Account     string `json:"account"`
	AccountName string `json:"account_name"`
	Created     string `json:"created"`
	LastUsed    string `json:"last_used"`
	Address     string `json:"address"`
}

// ownerActor is the actor when it is the coordinator's owner account; anyone else is forbidden.
func (h *handler) ownerActor(w http.ResponseWriter, r *http.Request) (string, bool) {
	user, ok := h.actor(w, r)
	if !ok {
		return "", false
	}
	if !h.isOwner(user) {
		writeError(w, 403, "forbidden")
		return "", false
	}
	return user, true
}

// ownerMutation reads the key and the body of an owner-channel mutation.
func (h *handler) ownerMutation(w http.ResponseWriter, r *http.Request) (string, string, []byte, *http.Request, bool) {
	user, ok := h.ownerActor(w, r)
	if !ok {
		return "", "", nil, nil, false
	}
	k, ok := key(w, r)
	if !ok {
		return "", "", nil, nil, false
	}
	body, ok := readBody(w, r)
	if !ok {
		return "", "", nil, nil, false
	}
	return user, k, body, r.WithContext(context.WithValue(r.Context(), bodyKey{}, body)), true
}

// emptyBody accepts no body or an empty JSON object.
func emptyBody(body []byte) bool {
	return len(body) == 0 || decode(body, &struct{}{}) == nil
}

// expire deletes a session found expired, unless another request used it since, and closes
// its streams; it reports whether the row is gone.
func (h *handler) expire(ctx context.Context, sess store.Session) (bool, error) {
	gone, err := h.store.DeleteExpiredSession(ctx, sess.ID, sess.LastUsed)
	if err != nil {
		return false, err
	}
	if gone {
		h.closeSessions([]string{sess.ID})
	}
	return gone, nil
}

// live is the cutoff of sessions alive now: lists and counts take only those.
func (h *handler) live() store.Live {
	now := h.now()
	return store.Live{UsedAfter: now.Add(-sessionIdle), CreatedAfter: now.Add(-sessionAbsolute)}
}

func (h *handler) closeSessions(ids []string) {
	if h.hub != nil && len(ids) != 0 {
		h.hub.Sessions.Close(ids...)
	}
}

func (h *handler) listAccounts(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.ownerActor(w, r); !ok {
		return
	}
	accounts, err := h.store.Accounts(r.Context(), h.live())
	if err != nil {
		storeError(w, err)
		return
	}
	out := make([]accountJSON, 0, len(accounts))
	for _, a := range accounts {
		out = append(out, h.accountValue(a))
	}
	writeJSON(w, 200, out)
}

func (h *handler) createAccount(w http.ResponseWriter, r *http.Request) {
	user, k, body, r, ok := h.ownerMutation(w, r)
	if !ok {
		return
	}
	var input struct {
		Name            string `json:"name"`
		PasswordAllowed bool   `json:"password_allowed"`
	}
	if err := decode(body, &input); err != nil || !validAccountName(input.Name) {
		writeError(w, 400, "bad_request")
		return
	}
	var issued codeJSON
	code := ""
	_, err := h.commit(r, user, k, nil, "", func(tx *store.Tx) (int, any, error) {
		id, err := newID()
		if err != nil {
			return 0, nil, err
		}
		if code, err = newCode(); err != nil {
			return 0, nil, err
		}
		at := h.now()
		if err := tx.CreateAccount(r.Context(), id, input.Name, input.PasswordAllowed, at); err != nil {
			return 0, nil, err
		}
		if err := tx.IssueEnrollmentCode(r.Context(), id, codeHash(code), at); err != nil {
			return 0, nil, err
		}
		a, err := tx.Account(r.Context(), id, h.live())
		if err != nil {
			return 0, nil, err
		}
		value := h.accountValue(a)
		issued = codeJSON{Account: &value, Expires: stampTime(at.Add(store.CodeLifetime))}
		return 201, issued, nil
	})
	h.answerCode(w, err, issued, code)
}

// answerCode shows a code once: a replay of the request that issued it is refused, since the
// code is kept nowhere but as its hash.
func (h *handler) answerCode(w http.ResponseWriter, err error, issued codeJSON, code string) {
	if err != nil {
		storeError(w, err)
		return
	}
	if code == "" || issued.Expires == "" {
		writeError(w, 409, "code_shown")
		return
	}
	issued.Code = groupCode(code)
	writeJSON(w, 201, issued)
}

func (h *handler) issueCode(recover bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, k, body, r, ok := h.ownerMutation(w, r)
		if !ok {
			return
		}
		if !emptyBody(body) {
			writeError(w, 400, "bad_request")
			return
		}
		id := r.PathValue("id")
		var issued codeJSON
		var ended []string
		code := ""
		_, err := h.commit(r, user, k, nil, "", func(tx *store.Tx) (int, any, error) {
			var err error
			if recover {
				if ended, err = tx.EndAccountSessions(r.Context(), id); err != nil {
					return 0, nil, err
				}
			}
			if code, err = newCode(); err != nil {
				return 0, nil, err
			}
			at := h.now()
			if err := tx.IssueEnrollmentCode(r.Context(), id, codeHash(code), at); err != nil {
				return 0, nil, err
			}
			issued = codeJSON{Expires: stampTime(at.Add(store.CodeLifetime))}
			return 201, issued, nil
		})
		if err == nil && recover && code != "" {
			if h.hub != nil {
				h.hub.limits.clear(id)
			}
			h.closeSessions(ended)
		}
		h.answerCode(w, err, issued, code)
	}
}

func (h *handler) updateAccount(w http.ResponseWriter, r *http.Request) {
	user, k, body, r, ok := h.ownerMutation(w, r)
	if !ok {
		return
	}
	var input struct {
		Disabled        *bool `json:"disabled"`
		PasswordAllowed *bool `json:"password_allowed"`
	}
	if err := decode(body, &input); err != nil {
		writeError(w, 400, "bad_request")
		return
	}
	id := r.PathValue("id")
	if input.Disabled != nil && *input.Disabled && h.isOwner(id) {
		writeError(w, 409, "owner_account")
		return
	}
	var ended []string
	ok = h.mutate(w, r, user, k, nil, "", func(tx *store.Tx) (int, any, error) {
		a, sessions, err := tx.UpdateAccount(r.Context(), id, input.Disabled, input.PasswordAllowed, h.live())
		if err != nil {
			return 0, nil, err
		}
		ended = sessions
		return 200, h.accountValue(a), nil
	})
	if ok {
		h.closeSessions(ended)
	}
}

func (h *handler) removeAccount(w http.ResponseWriter, r *http.Request) {
	user, k, body, r, ok := h.ownerMutation(w, r)
	if !ok {
		return
	}
	if !emptyBody(body) {
		writeError(w, 400, "bad_request")
		return
	}
	id := r.PathValue("id")
	if h.isOwner(id) {
		writeError(w, 409, "owner_account")
		return
	}
	var ended []string
	ok = h.mutate(w, r, user, k, nil, "", func(tx *store.Tx) (int, any, error) {
		var err error
		ended, err = tx.RemoveAccount(r.Context(), id)
		return http.StatusNoContent, nil, err
	})
	if ok {
		h.closeSessions(ended)
	}
}

func (h *handler) listSessions(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.ownerActor(w, r); !ok {
		return
	}
	sessions, err := h.store.Sessions(r.Context(), "", h.live())
	if err != nil {
		storeError(w, err)
		return
	}
	accounts, err := h.store.Accounts(r.Context(), h.live())
	if err != nil {
		storeError(w, err)
		return
	}
	names := map[string]string{}
	for _, a := range accounts {
		names[a.ID] = a.Name
	}
	out := make([]sessionListJSON, 0, len(sessions))
	for _, s := range sessions {
		out = append(out, sessionListJSON{s.ID, s.User, names[s.User], s.Created, s.LastUsed, s.Address})
	}
	writeJSON(w, 200, out)
}

func (h *handler) revokeSession(w http.ResponseWriter, r *http.Request) {
	user, k, body, r, ok := h.ownerMutation(w, r)
	if !ok {
		return
	}
	if !emptyBody(body) {
		writeError(w, 400, "bad_request")
		return
	}
	id := r.PathValue("id")
	ok = h.mutate(w, r, user, k, nil, "", func(tx *store.Tx) (int, any, error) {
		return http.StatusNoContent, nil, tx.DeleteSession(r.Context(), id)
	})
	if ok {
		h.closeSessions([]string{id})
	}
}

func stampTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// now is the clock codes are issued and sessions judged by.
func (h *handler) now() time.Time {
	if h.net != nil {
		return h.net.clock()
	}
	if h.exec.Clock != nil {
		return h.exec.Clock()
	}
	return time.Now()
}
