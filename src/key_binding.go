package main

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
)

// Preserve the identity used by historical records after its credential is erased.
// keys.revoked: 0 active, 1 revoked, 2 deleted from the credential inventory.
const keyDeleted = 2

type bindingContextKey struct{}

func boundSession(r *http.Request) string {
	sid, _ := r.Context().Value(bindingContextKey{}).(string)
	return sid
}
func withBinding(r *http.Request, sid string) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), bindingContextKey{}, sid))
}

func (a *app) keyState(w http.ResponseWriter, r *http.Request, _ string) {
	kid := r.PathValue("id")
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		internal(w, err)
		return
	}
	defer tx.Rollback()
	var res sql.Result
	if r.Pattern == "DELETE /admin/keys/{id}/record" {
		res, err = tx.Exec("UPDATE keys SET revoked=?,hash='removed:'||id WHERE id=? AND revoked<>?", keyDeleted, kid, keyDeleted)
	} else {
		res, err = tx.Exec("UPDATE keys SET revoked=1 WHERE id=? AND revoked=0", kid)
	}
	if err == nil {
		_, err = tx.Exec("DELETE FROM key_bindings WHERE key_id=?", kid)
	}
	if err == nil {
		err = tx.Commit()
	}
	changed(w, res, err)
}

func (a *app) keyBinding(w http.ResponseWriter, r *http.Request, _ string) {
	var b struct {
		Session *string `json:"session_id"`
		Create  *string `json:"create_name"`
	}
	if !body(w, r, &b) {
		return
	}
	if (b.Session == nil) == (b.Create == nil) || (b.Create != nil && !nameOK(*b.Create)) {
		fail(w, 400, "invalid_request", "Specify a session_id or a new session name")
		return
	}
	kid := r.PathValue("id")
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		internal(w, err)
		return
	}
	defer tx.Rollback()
	var state int
	err = tx.QueryRow("SELECT revoked FROM keys WHERE id=? AND revoked<>?", kid, keyDeleted).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		fail(w, 404, "not_found", "Key not found")
		return
	}
	if err != nil {
		internal(w, err)
		return
	}
	if state != 0 {
		fail(w, 409, "inactive_key", "Revoked key cannot be bound")
		return
	}
	var sid string
	if b.Create != nil {
		sid = id("sess_")
		_, err = tx.Exec("INSERT INTO sessions(id,owner,name,created,updated) VALUES(?,?,?,?,?)", sid, kid, *b.Create, now(), now())
	} else if *b.Session != "" {
		err = tx.QueryRow("SELECT id FROM sessions WHERE id=?", *b.Session).Scan(&sid)
		if errors.Is(err, sql.ErrNoRows) {
			fail(w, 404, "not_found", "Session not found")
			return
		}
	}
	if err == nil {
		if sid == "" {
			_, err = tx.Exec("DELETE FROM key_bindings WHERE key_id=?", kid)
		} else {
			_, err = tx.Exec("INSERT INTO key_bindings(key_id,session) VALUES(?,?) ON CONFLICT(key_id) DO UPDATE SET session=excluded.session", kid, sid)
		}
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		internal(w, err)
		return
	}
	jsonOut(w, 200, M{"session_id": sid})
}
