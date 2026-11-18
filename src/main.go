package main

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

//go:embed web/*
var webFiles embed.FS

type app struct {
	db             *sql.DB
	admin, cliHome string
	base           runner
	slots          chan struct{}
	busy           sync.Map
	installing     sync.Mutex
}
type clientFn func(http.ResponseWriter, *http.Request, string)

func env(k, v string) string {
	if s := os.Getenv(k); s != "" {
		return s
	}
	return v
}
func main() {
	listen, cert, key := env("LISTEN_ADDR", "127.0.0.1:56439"), os.Getenv("TLS_CERT_FILE"), os.Getenv("TLS_KEY_FILE")
	host, port, e := net.SplitHostPort(listen)
	if e != nil {
		log.Fatal(e)
	}
	nport, e := strconv.Atoi(port)
	if e != nil || nport < 49152 || nport > 65535 {
		log.Fatal("use a high service port (49152–65535)")
	}
	if (cert == "") != (key == "") {
		log.Fatal("both TLS_CERT_FILE and TLS_KEY_FILE are required for HTTPS")
	}
	if len(os.Args) == 2 && os.Args[1] == "healthcheck" {
		if e = healthcheck(host, port, cert, os.Getenv("TLS_HEALTH_SERVER_NAME")); e != nil {
			log.Fatal(e)
		}
		return
	}
	syscall.Umask(0077)
	data, e := filepath.Abs(env("DATA_DIR", "./data"))
	if e != nil {
		log.Fatal(e)
	}
	secret := os.Getenv("ADMIN_TOKEN")
	if len(secret) < 32 {
		log.Fatal("set ADMIN_TOKEN to at least 32 random characters")
	}
	home, e := filepath.Abs(env("CODEX_HOME", filepath.Join(data, "codex")))
	if e != nil {
		log.Fatal(e)
	}
	work := filepath.Join(data, "empty-workdir")
	for _, s := range []string{home, work} {
		if e = os.MkdirAll(s, 0700); e != nil {
			log.Fatal(e)
		}
	}
	db, e := openDB(data)
	if e != nil {
		log.Fatal(e)
	}
	defer db.Close()
	bin, e := exec.LookPath(env("CODEX_BIN", "codex"))
	if e != nil {
		log.Fatal(e)
	}
	bin, e = filepath.Abs(bin)
	if e != nil {
		log.Fatal(e)
	}
	n, e := strconv.Atoi(env("MAX_CONCURRENT", "4"))
	if e != nil || n < 1 || n > 64 {
		log.Fatal("MAX_CONCURRENT must be 1..64")
	}
	a := &app{db: db, admin: digest(secret), base: runner{bin, home, work}, cliHome: os.Getenv("CLI_INSTALL_DIR"), slots: make(chan struct{}, n)}
	if a.cliHome != "" && !filepath.IsAbs(a.cliHome) {
		log.Fatal("CLI_INSTALL_DIR must be absolute")
	}
	active, e := a.current()
	if e != nil {
		log.Fatal(e)
	}
	probe, cancelProbe := context.WithTimeout(context.Background(), 20*time.Second)
	e = active.checkProtocol(probe)
	cancelProbe()
	if e != nil {
		log.Fatalf("incompatible Codex CLI: %v", e)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	srv := http.Server{Addr: listen, Handler: a.routes(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 20 * time.Second, WriteTimeout: 11 * time.Minute, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32768, BaseContext: func(_ net.Listener) context.Context { return ctx }}
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		<-ctx.Done()
		c, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		srv.Shutdown(c)
	}()
	log.Printf("Desk Tunnel listening on %s", srv.Addr)
	if cert != "" {
		srv.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
		e = srv.ListenAndServeTLS(cert, key)
	} else {
		e = srv.ListenAndServe()
	}
	if e != nil && !errors.Is(e, http.ErrServerClosed) {
		log.Fatal(e)
	}
	<-stopped
}
func jsonOut(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
func fail(w http.ResponseWriter, status int, code, msg string) {
	jsonOut(w, status, M{"error": M{"code": code, "message": msg}})
}
func internal(w http.ResponseWriter, e error) {
	log.Printf("storage: %v", e)
	fail(w, 500, "storage_error", "Gateway storage error")
}
func body(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if e := d.Decode(v); e != nil || d.Decode(new(any)) != io.EOF {
		fail(w, 400, "invalid_request", "Invalid JSON, unsupported field, or body exceeds 1 MiB")
		return false
	}
	return true
}
func (a *app) auth(admin bool, fn clientFn) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if ok && key != "" {
			hash := digest(key)
			if admin {
				if subtle.ConstantTimeCompare([]byte(hash), []byte(a.admin)) == 1 {
					fn(w, r, "")
					return
				}
			} else {
				var owner string
				e := a.db.QueryRow(`SELECT id FROM keys WHERE hash=? AND revoked=0`, hash).Scan(&owner)
				if e == nil {
					fn(w, r, owner)
					return
				}
				if !errors.Is(e, sql.ErrNoRows) {
					internal(w, e)
					return
				}
			}
		}
		fail(w, 401, "unauthorized", "Valid bearer token required")
	}
}
func (a *app) routes() http.Handler {
	m := http.NewServeMux()
	api := func(path string, fn clientFn) { m.HandleFunc(path, a.auth(false, fn)) }
	adm := func(path string, fn clientFn) { m.HandleFunc(path, a.auth(true, fn)) }
	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { jsonOut(w, 200, M{"status": "ok"}) })
	api("GET /v1/models", a.models)
	api("POST /v1/chat/completions", a.generate)
	api("POST /v1/images/generations", a.generate)
	api("GET /v1/usage", a.usage)
	api("POST /v1/sessions", a.createSession)
	api("GET /v1/sessions", a.sessions)
	api("GET /v1/sessions/{id}/history", a.history)
	api("DELETE /v1/sessions/{id}", a.deleteSession)
	adm("GET /admin/keys", a.keys)
	adm("POST /admin/keys", a.keys)
	adm("DELETE /admin/keys/{id}", a.keys)
	adm("GET /admin/usage", a.usage)
	adm("GET /admin/requests", a.logs)
	adm("GET /admin/sessions", a.sessions)
	adm("GET /admin/sessions/{id}/history", a.history)
	adm("DELETE /admin/sessions/{id}", a.deleteSession)
	adm("GET /admin/models", a.models)
	adm("GET /admin/account", a.accountRead)
	adm("GET /admin/quota", a.accountRead)
	adm("GET /admin/memory", memoryUsage)
	adm("GET /admin/cli", a.cliStatus)
	adm("POST /admin/cli/update", a.cliUpdate)
	sub, _ := fs.Sub(webFiles, "web")
	m.Handle("GET /", http.FileServer(http.FS(sub)))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		m.ServeHTTP(w, r)
	})
}
func (a *app) acquire(w http.ResponseWriter) bool {
	select {
	case a.slots <- struct{}{}:
		return true
	default:
		w.Header().Set("Retry-After", "2")
		fail(w, 429, "busy", "All CLI workers are busy")
		return false
	}
}
func (a *app) release() { <-a.slots }
func (a *app) keys(w http.ResponseWriter, r *http.Request, _ string) {
	switch r.Method {
	case "POST":
		var b struct{ Name string }
		if !body(w, r, &b) {
			return
		}
		if !nameOK(b.Name) {
			fail(w, 400, "invalid_name", "Name must be 1–100 characters")
			return
		}
		k := id("dt_")
		kid := id("key_")
		_, e := a.db.Exec(`INSERT INTO keys(id,name,hash,created) VALUES(?,?,?,?)`, kid, b.Name, digest(k), now())
		if e != nil {
			internal(w, e)
			return
		}
		jsonOut(w, 201, M{"id": kid, "key": k})
	case "DELETE":
		res, e := a.db.Exec(`UPDATE keys SET revoked=1 WHERE id=? AND revoked=0`, r.PathValue("id"))
		changed(w, res, e)
	default:
		items, e := rows(a.db, `SELECT id,name,created,revoked FROM keys ORDER BY created DESC,rowid DESC LIMIT 100 OFFSET ?`, offset(r))
		listOut(w, items, e)
	}
}
func nameOK(s string) bool       { return strings.TrimSpace(s) != "" && len([]rune(s)) <= 100 }
func offset(r *http.Request) int { n, _ := strconv.Atoi(r.URL.Query().Get("offset")); return max(n, 0) }
func listOut(w http.ResponseWriter, items []M, e error) {
	if e != nil {
		internal(w, e)
		return
	}
	jsonOut(w, 200, M{"data": items})
}
func changed(w http.ResponseWriter, res sql.Result, e error) {
	if e != nil {
		internal(w, e)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		fail(w, 404, "not_found", "Not found")
		return
	}
	jsonOut(w, 200, M{"ok": true})
}
func (a *app) createSession(w http.ResponseWriter, r *http.Request, owner string) {
	var b struct{ Name, Model string }
	if !body(w, r, &b) {
		return
	}
	if b.Name == "" {
		b.Name = "未命名会话"
	}
	if !nameOK(b.Name) || len(b.Model) > 200 {
		fail(w, 400, "invalid_request", "Invalid name or model")
		return
	}
	sid := id("sess_")
	_, e := a.db.Exec(`INSERT INTO sessions(id,owner,name,model,created,updated) VALUES(?,?,?,?,?,?)`, sid, owner, b.Name, b.Model, now(), now())
	if e != nil {
		internal(w, e)
		return
	}
	jsonOut(w, 201, M{"id": sid, "name": b.Name, "model": b.Model})
}
func (a *app) sessions(w http.ResponseWriter, r *http.Request, owner string) {
	items, e := rows(a.db, `SELECT s.id,s.owner,k.name key_name,s.name,s.model,s.created,s.updated FROM sessions s JOIN keys k ON k.id=s.owner WHERE (?='' OR s.owner=?) ORDER BY s.updated DESC,s.rowid DESC LIMIT 100 OFFSET ?`, owner, owner, offset(r))
	listOut(w, items, e)
}
func (a *app) lock(w http.ResponseWriter, id string) bool {
	if _, loaded := a.busy.LoadOrStore(id, true); loaded {
		fail(w, 409, "session_busy", "Another request is active on this session")
		return false
	}
	return true
}
func (a *app) deleteSession(w http.ResponseWriter, r *http.Request, owner string) {
	sid := r.PathValue("id")
	if !a.lock(w, sid) {
		return
	}
	defer a.busy.Delete(sid)
	res, e := a.db.Exec(`DELETE FROM sessions WHERE id=? AND (?='' OR owner=?)`, sid, owner, owner)
	changed(w, res, e)
}
func (a *app) logs(w http.ResponseWriter, r *http.Request, _ string) {
	s := r.URL.Query().Get("session")
	items, e := rows(a.db, `SELECT requests.*,keys.name key_name FROM requests LEFT JOIN keys ON owner=keys.id WHERE (?='' OR session=?) ORDER BY requests.created DESC,requests.rowid DESC LIMIT 100 OFFSET ?`, s, s, offset(r))
	listOut(w, items, e)
}
func (a *app) usage(w http.ResponseWriter, r *http.Request, owner string) {
	items, e := rows(a.db, `SELECT COUNT(*) requests,COALESCE(SUM(status>=400),0) errors,COALESCE(SUM(input),0) input_tokens,COALESCE(SUM(output),0) output_tokens,COALESCE(SUM(metered=0),0) unmetered_requests,COALESCE(SUM(kind='image' AND status=200),0) images FROM requests WHERE (?='' OR owner=?)`, owner, owner)
	if e != nil {
		internal(w, e)
		return
	}
	jsonOut(w, 200, items[0])
}
func (a *app) models(w http.ResponseWriter, r *http.Request, _ string) {
	if !a.acquire(w) {
		return
	}
	defer a.release()
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	run, e := a.current()
	if e != nil {
		internal(w, e)
		return
	}
	list, e := run.models(ctx)
	if e != nil {
		upstream(w, ctx, e)
		return
	}
	jsonOut(w, 200, M{"object": "list", "data": list})
}
func upstream(w http.ResponseWriter, ctx context.Context, e error) {
	status := 502
	if ctx.Err() != nil {
		status = 504
	}
	log.Printf("CLI request failed: %v", e)
	if errors.Is(e, errNoImage) {
		fail(w, 502, "image_unavailable", "Codex did not return an image. Check that your login, model and quota support the built-in image tool.")
		return
	}
	fail(w, status, "upstream_error", "Codex CLI unavailable or request failed; check login, model support and quota")
}
func (a *app) accountRead(w http.ResponseWriter, r *http.Request, _ string) {
	if !a.acquire(w) {
		return
	}
	defer a.release()
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	run, e := a.current()
	if e != nil {
		internal(w, e)
		return
	}
	p, e := run.open(ctx, false)
	if e != nil {
		upstream(w, ctx, e)
		return
	}
	defer p.close()
	if r.URL.Path == "/admin/account" {
		b, err := p.call("account/read", M{"refreshToken": false}, nil)
		if err != nil {
			upstream(w, ctx, err)
			return
		}
		var status struct {
			Account *struct {
				Type          string `json:"type"`
				Email         string `json:"email,omitempty"`
				PlanType      string `json:"planType,omitempty"`
				WorkspaceName string `json:"workspaceName,omitempty"`
			} `json:"account"`
			RequiresOpenAIAuth bool `json:"requiresOpenaiAuth"`
		}
		if err := json.Unmarshal(b, &status); err != nil {
			upstream(w, ctx, err)
			return
		}
		jsonOut(w, 200, status)
		return
	}
	b, e := p.call("account/rateLimits/read", M{"excludeResetCreditDetails": false}, nil)
	if e != nil {
		upstream(w, ctx, e)
		return
	}
	jsonOut(w, 200, b)
}
