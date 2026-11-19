package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const diagnosticsSchema = `
CREATE TABLE IF NOT EXISTS log_config(singleton INTEGER PRIMARY KEY CHECK(singleton=1), enabled INTEGER NOT NULL DEFAULT 1, paused INTEGER NOT NULL DEFAULT 0, llm_only INTEGER NOT NULL DEFAULT 1, capture_body INTEGER NOT NULL DEFAULT 0, capacity INTEGER NOT NULL DEFAULT 2000, dropped INTEGER NOT NULL DEFAULT 0, epoch INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS log_events(seq INTEGER PRIMARY KEY AUTOINCREMENT, request_id TEXT NOT NULL, direction TEXT NOT NULL, created INTEGER NOT NULL, method TEXT NOT NULL, path TEXT NOT NULL, status INTEGER NOT NULL, latency_ms INTEGER NOT NULL, owner TEXT, session TEXT, model TEXT, kind TEXT, stream INTEGER, error_code TEXT, message TEXT, request_body TEXT, response_body TEXT);
CREATE INDEX IF NOT EXISTS log_request ON log_events(request_id);
CREATE TABLE IF NOT EXISTS request_metrics(request_id TEXT PRIMARY KEY REFERENCES requests(id), cached INTEGER, reasoning INTEGER, ttft_ms INTEGER, speed REAL, cost REAL, account TEXT);
CREATE TABLE IF NOT EXISTS session_metrics(session TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE, cached INTEGER, reasoning INTEGER);
CREATE TABLE IF NOT EXISTS error_groups(source TEXT NOT NULL, code TEXT NOT NULL, status INTEGER NOT NULL, message TEXT NOT NULL, occurrences INTEGER NOT NULL, unread INTEGER NOT NULL, first_seen INTEGER NOT NULL,last_seen INTEGER NOT NULL,request_id TEXT NOT NULL,path TEXT NOT NULL,model TEXT NOT NULL,PRIMARY KEY(source,code,status));
`

func initDiagnostics(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(diagnosticsSchema); err != nil {
		return err
	}
	result, err := tx.Exec("INSERT OR IGNORE INTO log_config(singleton) VALUES(1)")
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 1 {
		// Seed the bounded viewer once. The complete historical ledger stays intact.
		_, err = tx.Exec(`INSERT INTO log_events(request_id,direction,created,method,path,status,latency_ms,owner,session,model,kind)
SELECT id,'ingress',created,'POST',CASE kind WHEN 'image' THEN '/v1/images/generations' ELSE '/v1/chat/completions' END,status,latency_ms,owner,session,model,kind FROM
(SELECT rowid AS ordinal,* FROM requests ORDER BY created DESC,rowid DESC LIMIT 2000) ORDER BY created,ordinal`)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

type traceKey struct{}
type hop struct {
	method, path string
	start        time.Time
	elapsed      int64
	status       int
}
type requestTrace struct {
	id, owner, session, model, kind, code, message string
	start                                          time.Time
	status                                         int
	stream                                         *bool
	reqBody, resBody, outBody                      string
	wireBody                                       []byte
	capture                                        bool
	epoch                                          int64
	hops                                           []hop
}

func tracing(ctx context.Context) *requestTrace {
	t, _ := ctx.Value(traceKey{}).(*requestTrace)
	return t
}

// Capture the timing of one outbound operation without retaining its arguments or credentials.
func recordHop(ctx context.Context, method, path string, start time.Time, err error) {
	if t := tracing(ctx); t != nil {
		t.hops = append(t.hops, hop{method, path, start, time.Since(start).Milliseconds(), errorStatus(ctx, err)})
	}
}

type observedWriter struct {
	http.ResponseWriter
	trace *requestTrace
	wrote bool
}

func (w *observedWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *observedWriter) WriteHeader(status int) {
	if w.wrote {
		return
	}
	w.trace.status = status
	w.wrote = true
	w.ResponseWriter.WriteHeader(status)
}
func (w *observedWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(200)
	}
	t := w.trace
	if t.capture && !strings.HasPrefix(t.id, "resp_") && t.reqBody != "" && t.kind == "chat" && len(t.wireBody) < 8193 {
		t.wireBody = append(t.wireBody, b[:min(len(b), 8193-len(t.wireBody))]...)
	}
	return w.ResponseWriter.Write(b)
}

func (a *app) observe(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if !(strings.HasPrefix(path, "/v1/") || strings.HasPrefix(path, "/admin/")) || strings.HasPrefix(path, "/admin/diagnostics") || path == "/admin/analytics" || path == "/admin/session" {
			next.ServeHTTP(w, r)
			return
		}
		t := &requestTrace{id: id("req_"), start: time.Now(), status: 200}
		w.Header().Set("X-Request-ID", t.id)
		switch path {
		case "/v1/chat/completions", "/v1/responses":
			t.kind = "chat"
		case "/v1/images/generations":
			t.kind = "image"
		}
		if err := a.db.QueryRow(`SELECT epoch,enabled AND NOT paused AND capture_body FROM log_config WHERE singleton=1`).Scan(&t.epoch, &t.capture); err != nil {
			log.Printf("log configuration: %v", err)
			t.epoch = -1
		}
		req := r.WithContext(context.WithValue(r.Context(), traceKey{}, t))
		next.ServeHTTP(&observedWriter{ResponseWriter: w, trace: t}, req)
		_, pattern, found := strings.Cut(req.Pattern, " ")
		if !found || pattern == "/" {
			pattern = "(unmatched)"
		}
		if err := a.saveTrace(t, r.Method, pattern); err != nil {
			log.Printf("request diagnostics: %v", err)
		}
	})
}

func (a *app) saveTrace(t *requestTrace, method, path string) error {
	elapsed := time.Since(t.start).Milliseconds()
	tx, err := a.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if t.status >= 400 {
		source := "gateway"
		if t.code == "upstream_error" || t.code == "image_unavailable" {
			source = "codex"
		}
		if t.code == "storage_error" {
			source = "storage"
		}
		if t.code == "" {
			t.code = "http_" + strconv.Itoa(t.status)
		}
		_, err = tx.Exec(`INSERT INTO error_groups VALUES(?,?,?,?,1,1,?,?,?,?,?)
ON CONFLICT(source,code,status) DO UPDATE SET message=excluded.message, occurrences=occurrences+1,unread=unread+1,last_seen=excluded.last_seen,request_id=excluded.request_id,path=excluded.path,model=excluded.model`, source, t.code, t.status, t.message, now(), now(), t.id, path, t.model)
		if err != nil {
			return err
		}
	}
	var enabled, paused, llmOnly, capture bool
	var epoch int64
	var capacity int
	if err = tx.QueryRow("SELECT enabled,paused,llm_only,capture_body,capacity,epoch FROM log_config WHERE singleton=1").Scan(&enabled, &paused, &llmOnly, &capture, &capacity, &epoch); err != nil {
		return err
	}
	if !enabled || paused || epoch != t.epoch || (llmOnly && t.kind == "") {
		return tx.Commit()
	}
	insert := func(direction, method, path string, status int, at time.Time, ms int64, request, response string) error {
		if !capture || !t.capture {
			request, response = "", ""
		}
		code, message := "", ""
		if status >= 400 {
			code, message = t.code, t.message
		}
		_, e := tx.Exec(`INSERT INTO log_events(request_id,direction,created,method,path,status,latency_ms,owner,session,model,kind,stream,error_code,message,request_body,response_body) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, t.id, direction, at.Unix(), method, path, status, ms, t.owner, t.session, t.model, t.kind, t.stream, code, message, nullable(request), nullable(response))
		return e
	}
	for _, h := range t.hops {
		if err = insert("egress", h.method, h.path, h.status, h.start, h.elapsed, t.outBody, t.resBody); err != nil {
			return err
		}
	}
	response := t.resBody
	if len(t.wireBody) > 0 {
		response = previewText(string(t.wireBody))
	}
	if err = insert("ingress", method, path, t.status, t.start, elapsed, t.reqBody, response); err != nil {
		return err
	}
	if err = trimLogs(tx, capacity); err != nil {
		return err
	}
	return tx.Commit()
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
func trimLogs(tx *sql.Tx, capacity int) error {
	result, err := tx.Exec(`DELETE FROM log_events WHERE seq <= (SELECT seq FROM log_events ORDER BY seq DESC LIMIT 1 OFFSET ?)`, capacity)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n > 0 {
		_, err = tx.Exec("UPDATE log_config SET dropped=dropped+? WHERE singleton=1", n)
	}
	return err
}
func preview(v any) string {
	raw, e := json.Marshal(v)
	if e != nil {
		return ""
	}
	return previewText(string(raw))
}

var credentialPattern = regexp.MustCompile(`(?i)(Bearer [A-Za-z0-9._~+/=-]+|(?:sk-|dt_|ghp_|github_pat_)[A-Za-z0-9_-]{12,})`)

func previewText(s string) string {
	const limit = 8192
	if len(s) > limit {
		s = strings.ToValidUTF8(s[:limit], "") + "…"
	}
	return credentialPattern.ReplaceAllString(s, "[redacted]")
}

func (a *app) logSettings(w http.ResponseWriter, r *http.Request, _ string) {
	if r.Method == "PATCH" {
		var patch struct {
			Enabled, Paused *bool
			LLMOnly         *bool `json:"llm_only"`
			Capture         *bool `json:"capture_body"`
			Capacity        *int
		}
		if !body(w, r, &patch) {
			return
		}
		if patch.Capacity != nil && (*patch.Capacity < 1 || *patch.Capacity > 10000) {
			fail(w, 400, "invalid_request", "Log capacity must be 1–10000")
			return
		}
		tx, e := a.db.Begin()
		if e != nil {
			internal(w, e)
			return
		}
		defer tx.Rollback()
		_, e = tx.Exec(`UPDATE log_config SET enabled=COALESCE(?,enabled),paused=COALESCE(?,paused),llm_only=COALESCE(?,llm_only),capture_body=COALESCE(?,capture_body),capacity=COALESCE(?,capacity),epoch=epoch+1 WHERE singleton=1`, patch.Enabled, patch.Paused, patch.LLMOnly, patch.Capture, patch.Capacity)
		if e == nil && patch.Capacity != nil {
			e = trimLogs(tx, *patch.Capacity)
		}
		if e == nil {
			e = tx.Commit()
		}
		if e != nil {
			internal(w, e)
			return
		}
	}
	res, e := rows(a.db, `SELECT enabled,paused,llm_only,capture_body,capacity,dropped,(SELECT COUNT(*) FROM log_events) size FROM log_config`)
	if e != nil {
		internal(w, e)
		return
	}
	jsonOut(w, 200, res[0])
}

const eventJoin = ` FROM log_events e LEFT JOIN keys k ON k.id=e.owner LEFT JOIN requests r ON r.id=e.request_id LEFT JOIN request_metrics m ON m.request_id=r.id `
const eventFields = `e.seq,e.request_id,e.direction,e.created,e.method,e.path,e.status,e.latency_ms,e.owner,k.name AS key_name,e.session,e.model,e.kind,e.stream,e.error_code,e.message,r.metered,CASE WHEN r.metered THEN r.input END input,CASE WHEN r.metered THEN r.output END output,m.cached,m.reasoning,m.ttft_ms,m.speed,m.cost,m.account,` + requestTPS + " tps"

func (a *app) diagnosticLogs(w http.ResponseWriter, r *http.Request, _ string) {
	if r.Method == "DELETE" {
		tx, e := a.db.Begin()
		if e != nil {
			internal(w, e)
			return
		}
		defer tx.Rollback()
		_, e = tx.Exec("DELETE FROM log_events; UPDATE log_config SET dropped=0,epoch=epoch+1")
		if e == nil {
			e = tx.Commit()
		}
		if e != nil {
			internal(w, e)
			return
		}
		jsonOut(w, 200, M{"ok": true})
		return
	}
	if seq := r.PathValue("seq"); seq != "" {
		res, e := rows(a.db, "SELECT "+eventFields+",e.request_body,e.response_body"+eventJoin+"WHERE e.seq=?", seq)
		if e != nil {
			internal(w, e)
			return
		}
		if len(res) == 0 {
			fail(w, 404, "not_found", "Log entry expired")
			return
		}
		jsonOut(w, 200, res[0])
		return
	}
	q := r.URL.Query()
	where := " WHERE 1=1"
	var args []any
	if dir := q.Get("direction"); dir != "" && dir != "all" {
		if dir != "ingress" && dir != "egress" {
			fail(w, 400, "invalid_request", "Invalid direction")
			return
		}
		where += " AND e.direction=?"
		args = append(args, dir)
	}
	if sid := q.Get("session"); sid != "" {
		where += " AND e.session=?"
		args = append(args, sid)
	}
	if q.Get("errors") == "1" {
		where += " AND e.status>=400"
	}
	if s := strings.TrimSpace(q.Get("search")); s != "" {
		where += ` AND instr(lower(e.method||' '||e.path||' '||e.request_id||' '||e.status||' '||COALESCE(e.model,'')||' '||COALESCE(e.session,'')||' '||COALESCE(k.name,'')||' '||COALESCE(e.error_code,'')||' '||COALESCE(m.account,'')),lower(?))>0`
		args = append(args, s)
	}
	tx, e := a.db.BeginTx(r.Context(), nil)
	if e != nil {
		internal(w, e)
		return
	}
	defer tx.Rollback()
	var total int
	e = tx.QueryRow("SELECT COUNT(*)"+eventJoin+where, args...).Scan(&total)
	if e != nil {
		internal(w, e)
		return
	}
	items, e := rows(tx, "SELECT "+eventFields+eventJoin+where+" ORDER BY e.seq DESC LIMIT 50 OFFSET ?", append(args, offset(r))...)
	if e != nil {
		internal(w, e)
		return
	}
	if e = tx.Commit(); e != nil {
		internal(w, e)
		return
	}
	jsonOut(w, 200, M{"data": items, "total": total})
}
func (a *app) errorGroups(w http.ResponseWriter, r *http.Request, _ string) {
	var e error
	switch r.Method {
	case "DELETE":
		_, e = a.db.Exec("DELETE FROM error_groups")
	case "POST":
		_, e = a.db.Exec("UPDATE error_groups SET unread=0")
	}
	if e != nil {
		internal(w, e)
		return
	}
	res, e := rows(a.db, "SELECT * FROM error_groups ORDER BY last_seen DESC,occurrences DESC")
	listOut(w, res, e)
}

func errorStatus(ctx context.Context, err error) int {
	if err == nil {
		return 200
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		return 499
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return 504
	}
	var re *responsesError
	if errors.As(err, &re) {
		return re.status
	}
	var provider *imageProviderError
	if errors.As(err, &provider) {
		switch provider.status {
		case 400, 403, 404, 409, 413, 422, 429, 503:
			return provider.status
		}
	}
	return 502
}
