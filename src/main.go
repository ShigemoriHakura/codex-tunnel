package main

import (
	"context"
	"crypto/tls"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
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
	db               *sql.DB
	admin, cliHome   string
	base             runner
	servers          *appServers
	imageTransport   http.RoundTripper
	slots            chan struct{}
	admitted         chan struct{}
	sessionTurns     sessionTurns
	jsonKeepalive    bool
	installing       sync.Mutex
	usingResetCredit sync.Mutex
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
	jsonKeepalive, e := strconv.ParseBool(env("JSON_KEEPALIVE", "false"))
	if e != nil {
		log.Fatal("JSON_KEEPALIVE must be true or false")
	}
	a := &app{db: db, admin: digest(secret), base: runner{bin, home, work}, cliHome: os.Getenv("CLI_INSTALL_DIR"), jsonKeepalive: jsonKeepalive}
	a.initWorkers(n)
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
	a.servers = newAppServers(ctx)
	defer a.servers.close()
	warm, e := a.servers.acquire(ctx, active, false)
	if e != nil {
		log.Fatalf("initialize Codex app-server: %v", e)
	}
	a.servers.release(warm, false)
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
	if observed, ok := w.(*observedWriter); ok {
		observed.trace.code = code
		observed.trace.message = msg
	}
	jsonOut(w, status, M{"error": M{"code": code, "message": msg}})
}
func internal(w http.ResponseWriter, e error) {
	log.Printf("storage: %v", e)
	fail(w, 500, "storage_error", "Gateway storage error")
}
func body(w http.ResponseWriter, r *http.Request, v any) bool {
	return boundedBody(w, r, v, 1<<20)
}
func boundedBody(w http.ResponseWriter, r *http.Request, v any, limit int64) bool {
	if r.ContentLength > limit {
		fail(w, 413, "body_too_large", fmt.Sprintf("Request body exceeds %d MiB", limit>>20))
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	err := d.Decode(v)
	if err == nil {
		if err = d.Decode(new(json.RawMessage)); err == io.EOF {
			return true
		}
	}
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		fail(w, 413, "body_too_large", fmt.Sprintf("Request body exceeds %d MiB", limit>>20))
		return false
	}
	if err != nil && strings.HasPrefix(err.Error(), "json: unknown field ") {
		field := strings.TrimPrefix(err.Error(), "json: unknown field ")
		if len(field) <= 64 && strings.IndexFunc(strings.Trim(field, "\""), func(r rune) bool {
			return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r == '_' || r >= '0' && r <= '9')
		}) < 0 {
			fail(w, 400, "unsupported_field", "Unsupported request field "+field)
			return false
		}
	}
	fail(w, 400, "invalid_request", "Invalid JSON or unexpected field type")
	return false
}

func (a *app) auth(admin bool, fn clientFn) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if admin {
			if r.Header.Get("Authorization") != "" {
				if a.adminBearer(r) {
					fn(w, r, "")
					return
				}
			} else {
				ok, err := a.hasConsoleSession(r)
				if err != nil {
					internal(w, err)
					return
				}
				if ok {
					if allowConsoleWrite(w, r) {
						fn(w, r, "")
					}
					return
				}
			}
		} else if key, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok && key != "" {
			var owner, binding string
			e := a.db.QueryRow(`SELECT k.id,COALESCE(b.session,'') FROM keys k LEFT JOIN key_bindings b ON b.key_id=k.id WHERE k.hash=? AND k.revoked=0`, digest(key)).Scan(&owner, &binding)
			if e == nil {
				if t := tracing(r.Context()); t != nil {
					t.owner = owner
				}
				fn(w, withBinding(r, binding), owner)
				return
			}
			if !errors.Is(e, sql.ErrNoRows) {
				internal(w, e)
				return
			}
		}
		fail(w, 401, "unauthorized", "Valid bearer token required")
	}
}
func (a *app) routes() http.Handler {
	m := http.NewServeMux()
	api := func(path string, fn clientFn) { m.HandleFunc(path, a.auth(false, fn)) }
	adm := func(path string, fn clientFn) { m.HandleFunc(path, a.auth(true, fn)) }
	for _, method := range []string{"GET", "POST", "DELETE"} {
		m.HandleFunc(method+" /admin/session", a.consoleSession)
	}
	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { jsonOut(w, 200, M{"status": "ok"}) })
	api("GET /v1/models", a.models)
	api("POST /v1/chat/completions", a.generate)
	api("POST /v1/images/generations", a.generate)
	api("GET /v1/usage", a.usage)
	api("POST /v1/sessions", a.createSession)
	api("GET /v1/sessions", a.sessions)
	api("GET /v1/sessions/{id}/history", a.history)
	api("DELETE /v1/sessions/{id}", a.deleteSession)
	for _, method := range []string{"GET", "PATCH"} {
		adm(method+" /admin/diagnostics/settings", a.logSettings)
	}
	for _, method := range []string{"GET", "DELETE"} {
		adm(method+" /admin/diagnostics/logs", a.diagnosticLogs)
	}
	adm("GET /admin/diagnostics/logs/{seq}", a.diagnosticLogs)
	for _, method := range []string{"GET", "POST", "DELETE"} {
		adm(method+" /admin/diagnostics/errors", a.errorGroups)
	}
	adm("GET /admin/analytics", a.analytics)
	adm("GET /admin/site", a.siteSettings)
	adm("PUT /admin/site", a.siteSettings)
	adm("GET /admin/keys", a.keys)
	adm("POST /admin/keys", a.keys)
	adm("DELETE /admin/keys/{id}", a.keyState)
	adm("DELETE /admin/keys/{id}/record", a.keyState)
	adm("PUT /admin/keys/{id}/binding", a.keyBinding)
	adm("GET /admin/usage", a.usage)
	adm("GET /admin/requests", a.logs)
	adm("GET /admin/sessions", a.sessions)
	adm("GET /admin/sessions/{id}/history", a.history)
	adm("DELETE /admin/sessions/{id}", a.deleteSession)
	adm("GET /admin/models", a.models)
	adm("GET /admin/account", a.accountRead)
	adm("GET /admin/quota", a.accountRead)
	adm("POST /admin/quota/reset-credit", a.consumeResetCredit)
	adm("GET /admin/memory", memoryUsage)
	adm("GET /admin/cli", a.cliStatus)
	adm("POST /admin/cli/update", a.cliUpdate)
	sub, _ := fs.Sub(webFiles, "web")
	m.Handle("GET /", http.FileServer(http.FS(sub)))
	observed := a.serveMount(a.observe(m))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		observed.ServeHTTP(w, r)
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
	default:
		items, e := rows(a.db, `SELECT k.id,k.name,k.created,k.revoked,b.session AS session_id,s.name AS session_name FROM keys k LEFT JOIN key_bindings b ON k.id=b.key_id LEFT JOIN sessions s ON s.id=b.session WHERE k.revoked<>? ORDER BY k.created DESC,k.rowid DESC LIMIT 100 OFFSET ?`, keyDeleted, offset(r))
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
	if boundSession(r) != "" {
		fail(w, 403, "session_fixed", "This key is fixed to an existing session")
		return
	}
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
	where := ""
	var args []any
	if sid := boundSession(r); sid != "" {
		where = " WHERE s.id=?"
		args = append(args, sid)
	} else if owner != "" {
		where = " WHERE s.owner=?"
		args = append(args, owner)
	}
	items, e := rows(a.db, `SELECT s.id,s.owner,k.name key_name,s.name,s.model,s.created,s.updated FROM sessions s JOIN keys k ON k.id=s.owner`+where+` ORDER BY s.updated DESC,s.rowid DESC LIMIT 100 OFFSET ?`, append(args, offset(r))...)
	listOut(w, items, e)
}
func (a *app) deleteSession(w http.ResponseWriter, r *http.Request, owner string) {
	sid := r.PathValue("id")
	_, release := a.sessionTurns.join(sid, 1)
	if release == nil {
		fail(w, 409, "session_busy", "Another request is active on this session")
		return
	}
	defer release()
	if boundSession(r) != "" {
		fail(w, 403, "session_fixed", "A bound key cannot delete sessions")
		return
	}
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		internal(w, err)
		return
	}
	defer tx.Rollback()
	var linked bool
	err = tx.QueryRow("SELECT EXISTS(SELECT 1 FROM key_bindings WHERE session=s.id) FROM sessions s WHERE id=? AND (?='' OR owner=?)", sid, owner, owner).Scan(&linked)
	if errors.Is(err, sql.ErrNoRows) {
		fail(w, 404, "not_found", "Session not found")
		return
	}
	if err != nil {
		internal(w, err)
		return
	}
	if linked {
		fail(w, 409, "session_bound", "Unbind active keys before removing this session")
		return
	}
	res, err := tx.Exec("DELETE FROM sessions WHERE id=?", sid)
	if err == nil {
		err = tx.Commit()
	}
	changed(w, res, err)
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
	list, e := run.models(ctx, a.servers)
	if e != nil {
		upstream(w, ctx, e)
		return
	}
	jsonOut(w, 200, M{"object": "list", "data": list})
}
func upstreamFailure(ctx context.Context, e error) (int, string, string) {
	status := errorStatus(ctx, e)
	if status == 499 {
		return status, "request_canceled", "Request canceled before completion"
	}
	var provider *imageProviderError
	if errors.As(e, &provider) {
		return status, "image_provider_error", provider.Error()
	}
	log.Printf("Upstream request failed: %v", e)
	if errors.Is(e, errNoImage) {
		return 502, "image_unavailable", "Codex did not return an image. Check that your login, model and quota support the built-in image tool."
	}
	return status, "upstream_error", "Codex CLI unavailable or request failed; check login, model support and quota"
}
func upstream(w http.ResponseWriter, ctx context.Context, err error) {
	status, code, message := upstreamFailure(ctx, err)
	fail(w, status, code, message)
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
	p, e := a.servers.acquire(ctx, run, false)
	if e != nil {
		upstream(w, ctx, e)
		return
	}
	defer a.servers.release(p, false)
	if r.URL.Path == "/admin/account" {
		b, err := p.call(ctx, "account/read", M{"refreshToken": false})
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
	b, e := p.call(ctx, "account/rateLimits/read", M{"excludeResetCreditDetails": false})
	if e != nil {
		upstream(w, ctx, e)
		return
	}
	jsonOut(w, 200, b)
}
