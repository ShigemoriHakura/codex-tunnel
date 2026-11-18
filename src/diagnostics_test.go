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
