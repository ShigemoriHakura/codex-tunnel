package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func decode(t *testing.T, s string, target any) {
	t.Helper()
	if err := json.Unmarshal([]byte(s), target); err != nil {
		t.Fatal(err)
	}
}

func TestDiagnosticsControls(t *testing.T) {
	a := testApp(t, t.TempDir())
	for _, path := range []string{"/admin/diagnostics/settings", "/admin/diagnostics/logs", "/admin/diagnostics/logs/1", "/admin/diagnostics/errors", "/admin/analytics"} {
		hit(t, a, "GET", path, "unauthorized", "", "", 401)
	}
	const settings = "/admin/diagnostics/settings"
	const logs = "/admin/diagnostics/logs"
	const groups = "/admin/diagnostics/errors"
	key, _ := newKey(t, a)
	get := func(path string) string { return hit(t, a, "GET", path, testAdmin, "", "", 200) }
	patch := func(s string) { hit(t, a, "PATCH", settings, testAdmin, s, "", 200) }
	checkTotal := func(query string, n int) {
		t.Helper()
		var list struct {
			Total int
			Data  []M
		}
		decode(t, get(logs+query), &list)
		if list.Total != n {
			t.Fatalf("%s total %d != %d", query, list.Total, n)
		}
	}
	hit(t, a, "PATCH", settings, testAdmin, `{"capacity":0}`, "", 400)
	checkTotal("", 0)
	patch(`{"llm_only":false,"capacity":2}`)
	newKey(t, a)
	hit(t, a, "GET", "/v1/sessions", key, "", "", 200)
	checkTotal("?search=POST", 1)
	hit(t, a, "GET", "/v1/usage", "bad", "", "", 401)
	checkTotal("?errors=1&direction=ingress", 1)
	checkTotal("?direction=egress", 0)
	var state struct{ Dropped, Size int }
	decode(t, get(settings), &state)
	if state.Dropped != 1 || state.Size != 2 {
		t.Fatal(state)
	}
	patch(`{"paused":true}`)
	hit(t, a, "GET", "/v1/usage", "bad", "", "", 401)
	decode(t, get(settings), &state)
	if state.Dropped != 1 {
		t.Fatal("paused log changed")
	}
	type group struct{ Occurrences, Unread int }
	var list struct{ Data []group }
	decode(t, get(groups), &list)
	if len(list.Data) != 1 || list.Data[0] != (group{2, 2}) {
		t.Fatal(list)
	}
	decode(t, hit(t, a, "POST", groups, testAdmin, "", "", 200), &list)
	if list.Data[0] != (group{2, 0}) {
		t.Fatal(list)
	}
	patch(`{"paused":false}`)
	var epoch int64
	if e := a.db.QueryRow("SELECT epoch FROM log_config").Scan(&epoch); e != nil {
		t.Fatal(e)
	}
	hit(t, a, "DELETE", logs, testAdmin, "", "", 200)
	if e := a.saveTrace(&requestTrace{id: "pending", epoch: epoch, status: 200, start: time.Now()}, "GET", "/v1/models"); e != nil {
		t.Fatal(e)
	}
	checkTotal("", 0)
	decode(t, get(groups), &list)
	if len(list.Data) != 1 {
		t.Fatal("clearing logs cleared errors")
	}
	hit(t, a, "DELETE", groups, testAdmin, "", "", 200)
	if !strings.Contains(get(groups), `"data":[]`) {
		t.Fatal("error groups not cleared")
	}
	patch(`{"enabled":false}`)
	hit(t, a, "GET", "/v1/sessions", key, "", "", 200)
	checkTotal("", 0)
}

func TestLedgerMigrationAndFiltering(t *testing.T) {
	dir := t.TempDir()
	a := testApp(t, dir)
	// Simulate a gateway upgrading with more historical requests than one viewer page.
	_, err := a.db.Exec(`DROP TABLE log_config;
WITH RECURSIVE numbers(n) AS (VALUES(1) UNION ALL SELECT n+1 FROM numbers WHERE n<54)
INSERT INTO requests SELECT 'old-'||n,'key-a','','chat','old-model',200,8,3,1,240,? FROM numbers;
INSERT INTO requests VALUES('recent','key-b','','image','new-model',502,0,0,0,550,?);`, now()-172800, now())
	if err != nil {
		t.Fatal(err)
	}
	a.db.Close()
	a = testApp(t, dir)
	get := func(path string) string { return hit(t, a, "GET", path, testAdmin, "", "", 200) }
	var logs struct {
		Data  []M
		Total int
	}
	decode(t, get("/admin/diagnostics/logs?offset=50"), &logs)
	if logs.Total != 55 || len(logs.Data) != 5 || logs.Data[0]["cached"] != nil {
		t.Fatal("legacy pagination or unknown metric")
	}
	for _, path := range []string{"/admin/analytics?hours=2", "/admin/analytics?step=0", "/admin/diagnostics/logs?direction=bad"} {
		hit(t, a, "GET", path, testAdmin, "", "", 400)
	}
	type summary struct {
		Requests, Errors, Input, Output, Unmetered int
		Cached, Cost, HitRate                      *float64
	}
	var analytics struct {
		Summary, Lifetime summary
		Buckets           []M
		Keys, Models      []M
	}
	decode(t, get("/admin/analytics?hours=1&step=300"), &analytics)
	if analytics.Summary.Requests != 1 || analytics.Summary.Errors != 1 || analytics.Summary.Unmetered != 1 || analytics.Summary.Cached != nil || analytics.Lifetime.Requests != 55 {
		t.Fatal(analytics)
	}
	decode(t, get("/admin/analytics?hours=all&step=86400&model=old-model&owner=key-a"), &analytics)
	if analytics.Summary.Requests != 54 || analytics.Summary.Input != 432 || len(analytics.Buckets) != 1 || len(analytics.Keys) != 1 || len(analytics.Models) != 1 {
		t.Fatal(analytics)
	}
	hit(t, a, "DELETE", "/admin/diagnostics/logs", testAdmin, "", "", 200)
	a.db.Close()
	a = testApp(t, dir)
	decode(t, get("/admin/diagnostics/logs"), &logs)
	if logs.Total != 0 {
		t.Fatal("deleted legacy entries returned on restart")
	}
	decode(t, get("/admin/analytics?hours=all"), &analytics)
	if analytics.Summary.Requests != 55 {
		t.Fatal("ledger lost")
	}
}

func TestPreviewAndLegacyCounters(t *testing.T) {
	s := previewText("Bearer example-token " + strings.Repeat("字", 4000))
	if strings.Contains(s, "example-token") || len(s) > 8195 || !strings.HasSuffix(s, "…") {
		t.Fatal("preview bounds or redaction failed")
	}
	current, prior := int64(10), int64(7)
	if counterDelta(&current, nil, false) != nil || *counterDelta(&current, &prior, false) != 3 || *counterDelta(&prior, &current, false) != 7 {
		t.Fatal("incorrect cumulative delta")
	}
}

func TestOutputTPSAggregation(t *testing.T) {
	a := testApp(t, t.TempDir())
	token, owner := newKey(t, a)
	otherToken, other := newKey(t, a)
	// Separate buckets. Each row represents one request, even if both log directions exist.
	at := (now()/300)*300 - 600
	for _, row := range []struct {
		id, key, model, kind    string
		status, output, metered int
		at                      int64
		speed                   any
	}{
		{"one", owner, "alpha", "chat", 200, 100, 1, at, 12.5},
		{"two", owner, "alpha", "chat", 200, 180, 1, at, 37.5},
		{"three", other, "beta", "chat", 200, 500, 1, at + 300, 70},
		{"old", other, "beta", "chat", 200, 500, 1, at - 86400, 80},
		{"image", owner, "alpha", "image", 200, 500, 1, at, 900},
		{"failure", owner, "alpha", "chat", 499, 500, 1, at, 800},
		{"short", owner, "alpha", "chat", 200, 99, 1, at, 700},
		{"legacy", owner, "alpha", "chat", 200, 100, 1, at, nil},
		{"unmetered", owner, "alpha", "chat", 200, 100, 0, at, 600},
	} {
		if _, err := a.db.Exec("INSERT INTO requests VALUES(?,?,'',?,?,?,0,?,?,1500,?)", row.id, row.key, row.kind, row.model, row.status, row.output, row.metered, row.at); err != nil {
			t.Fatal(err)
		}
		if row.speed != nil {
			if _, err := a.db.Exec("INSERT INTO request_metrics(request_id,speed) VALUES(?,?)", row.id, row.speed); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, direction := range []string{"ingress", "egress"} {
		if _, err := a.db.Exec("INSERT INTO log_events(request_id,direction,created,method,path,status,latency_ms) VALUES('one',?,?,'POST','/v1/chat/completions',200,1500)", direction, at); err != nil {
			t.Fatal(err)
		}
	}
	type stats struct {
		Average *float64 `json:"avg_tps"`
		Peak    *float64 `json:"peak_tps"`
		Samples int      `json:"tps_samples"`
	}
	check := func(s stats, n int, avg, peak float64) {
		t.Helper()
		if s.Samples != n || s.Average == nil || *s.Average != avg || s.Peak == nil || *s.Peak != peak {
			t.Errorf("expected %d samples, average %v and peak %v, got %+v", n, avg, peak, s)
		}
	}
	get := func(path, key string, target any) {
		t.Helper()
		decode(t, hit(t, a, "GET", path, key, "", "", 200), target)
	}
	var report struct {
		Summary, Lifetime     stats
		Buckets, Models, Keys []stats
		Recent                []struct {
			ID  string
			TPS *float64
		}
	}
	get("/admin/analytics?hours=1&step=300&recent=1", testAdmin, &report)
	check(report.Summary, 3, 40, 70)
	check(report.Lifetime, 4, 50, 80)
	if len(report.Buckets) != 2 || len(report.Models) != 2 || len(report.Keys) != 2 {
		t.Fatal(report)
	}
	for _, group := range [][]stats{report.Buckets, report.Models, report.Keys} {
		check(group[0], 2, 25, 37.5)
		check(group[1], 1, 70, 70)
	}
	for _, row := range report.Recent {
		if row.ID == "three" && (row.TPS == nil || *row.TPS != 70) {
			t.Fatal("missing TPS from recent requests")
		}
		if row.ID == "short" && row.TPS != nil {
			t.Fatal("short reply acquired a TPS sample")
		}
	}
	get("/admin/analytics?hours=1&model=beta&owner="+other, testAdmin, &report)
	check(report.Summary, 1, 70, 70)
	get("/admin/analytics?hours=1&kind=image", testAdmin, &report)
	if report.Summary.Average != nil || report.Summary.Peak != nil || report.Summary.Samples != 0 {
		t.Fatal("empty TPS cohort must have null rates")
	}
	var s stats
	get("/v1/usage", token, &s)
	check(s, 2, 25, 37.5)
	get("/v1/usage", otherToken, &s)
	check(s, 2, 75, 80)
	var logs struct{ Data []struct{ TPS float64 } }
	get("/admin/diagnostics/logs?search=one", testAdmin, &logs)
	if len(logs.Data) != 2 || logs.Data[0].TPS != 12.5 || logs.Data[1].TPS != 12.5 {
		t.Fatal("incorrect per-request log TPS", logs)
	}
}
