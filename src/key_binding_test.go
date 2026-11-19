package main

import (
	"strings"
	"testing"
)

func TestKeyBindingAndRemoval(t *testing.T) {
	dir := t.TempDir()
	a := testApp(t, dir)
	original, _ := newKey(t, a)
	delegate, id := newKey(t, a)
	s := prop(hit(t, a, "POST", "/v1/sessions", original, `{"name":"shared"}`, "", 201), "id")
	own := prop(hit(t, a, "POST", "/v1/sessions", delegate, `{}`, "", 201), "id")
	path := "/admin/keys/" + id + "/binding"
	hit(t, a, "PUT", path, delegate, `{"session_id":"`+s+`"}`, "", 401)
	hit(t, a, "PUT", path, testAdmin, `{"session_id":"`+s+`"}`, "", 200)
	a.db.Close()
	a = testApp(t, dir)
	hit(t, a, "GET", "/v1/sessions/"+s+"/history", delegate, "", "", 200)
	if list := hit(t, a, "GET", "/v1/sessions", delegate, "", "", 200); !strings.Contains(list, s) || strings.Contains(list, own) {
		t.Fatal("wrong session list")
	}
	hit(t, a, "GET", "/v1/sessions/"+own+"/history", delegate, "", "", 404)
	hit(t, a, "DELETE", "/admin/sessions/"+s, testAdmin, "", "", 409)
	hit(t, a, "POST", "/v1/chat/completions", delegate, `{"messages":[{"role":"user","content":"hello"}]}`, own, 403)
	hit(t, a, "PUT", path, testAdmin, `{"session_id":""}`, "", 200)
	hit(t, a, "GET", "/v1/sessions/"+s+"/history", delegate, "", "", 404)
	created := prop(hit(t, a, "PUT", path, testAdmin, `{"create_name":"new session"}`, "", 200), "session_id")
	if created == "" {
		t.Fatal("session not created")
	}
	hit(t, a, "DELETE", "/admin/keys/"+id+"/record", testAdmin, "", "", 200)
	hit(t, a, "GET", "/v1/usage", delegate, "", "", 401)
	if strings.Contains(hit(t, a, "GET", "/admin/keys", testAdmin, "", "", 200), id) {
		t.Fatal("deleted credential still listed")
	}
	var count int
	if err := a.db.QueryRow("SELECT COUNT(*) FROM keys WHERE hash=?", digest(delegate)).Scan(&count); err != nil || count != 0 {
		t.Fatal("old credential digest retained")
	}
	hit(t, a, "GET", "/admin/sessions/"+created+"/history", testAdmin, "", "", 200)
	hit(t, a, "DELETE", "/admin/sessions/"+created, testAdmin, "", "", 200)
}
