package main

import (
	"crypto/subtle"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"
)

const consoleCookie = "desk_console_session"
const consoleTTL = 7 * 24 * time.Hour

func (a *app) adminBearer(r *http.Request) bool {
	key, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return ok && key != "" && subtle.ConstantTimeCompare([]byte(digest(key)), []byte(a.admin)) == 1
}

// Binding the lookup to the current admin secret invalidates sessions when it changes.
func (a *app) consoleDigest(token string) string { return digest(a.admin + "|" + token) }
func (a *app) presentedSession(r *http.Request) string {
	c, err := r.Cookie(consoleCookie)
	if err != nil || len(c.Value) != 48 {
		return ""
	}
	return a.consoleDigest(c.Value)
}
func (a *app) hasConsoleSession(r *http.Request) (bool, error) {
	key := a.presentedSession(r)
	if key == "" {
		return false, nil
	}
	var found int
	err := a.db.QueryRowContext(r.Context(), "SELECT 1 FROM console_sessions WHERE digest=? AND expires>?", key, now()).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// Cookie-authenticated writes require a header that a cross-origin HTML form cannot set.
// The gateway does not opt in to cross-origin credentialed requests.
func allowConsoleWrite(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Header.Get("X-Desk-Console") == "1" {
		return true
	}
	fail(w, http.StatusForbidden, "console_request_required", "Console request header required")
	return false
}

func sessionCookie(r *http.Request, path string) *http.Cookie {
	return &http.Cookie{Name: consoleCookie, Path: path, HttpOnly: true, SameSite: http.SameSiteStrictMode,
		Secure: r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")}
}

func (a *app) consoleSession(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		ok, err := a.hasConsoleSession(r)
		if err != nil {
			internal(w, err)
			return
		}
		jsonOut(w, 200, M{"authenticated": ok})
		return
	}
	if !allowConsoleWrite(w, r) {
		return
	}
	// The browser knows the public /admin/session URL even when a relay strips its prefix.
	// Omitting Path scopes both login and logout to that URL's /admin directory.
	cookie := sessionCookie(r, "")
	previous := a.presentedSession(r)
	if r.Method == http.MethodDelete {
		if previous != "" {
			if _, err := a.db.ExecContext(r.Context(), "DELETE FROM console_sessions WHERE digest=?", previous); err != nil {
				internal(w, err)
				return
			}
		}
		cookie.MaxAge = -1
		cookie.Expires = time.Unix(1, 0).UTC()
		http.SetCookie(w, cookie)
		jsonOut(w, 200, M{"authenticated": false})
		return
	}
	if !a.adminBearer(r) {
		fail(w, 401, "unauthorized", "Valid administrator token required")
		return
	}
	cookie.Value = id("")
	cookie.MaxAge = int(consoleTTL / time.Second)
	cookie.Expires = time.Now().UTC().Add(consoleTTL)
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		internal(w, err)
		return
	}
	defer tx.Rollback()
	_, err = tx.Exec("DELETE FROM console_sessions WHERE expires<=? OR digest=?", now(), previous)
	if err == nil {
		_, err = tx.Exec("INSERT INTO console_sessions(digest,expires) VALUES(?,?)", a.consoleDigest(cookie.Value), cookie.Expires.Unix())
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		internal(w, err)
		return
	}
	http.SetCookie(w, cookie)
	jsonOut(w, 200, M{"authenticated": true})
}
