package main

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// A missing baseline after upgrading an existing thread cannot be billed as a new turn.
func counterDelta(current, prior *int64, fresh bool) *int64 {
	if current == nil || *current < 0 {
		return nil
	}
	if fresh {
		return current
	}
	if prior == nil {
		return nil
	}
	n := *current - *prior
	if n < 0 {
		return current
	}
	return &n
}
func accountLabel(home string) any {
	f, e := os.Open(filepath.Join(home, "auth.json"))
	if e != nil {
		return nil
	}
	defer f.Close()
	var auth struct {
		Tokens struct {
			AccountID string `json:"account_id"`
		}
	}
	if json.NewDecoder(io.LimitReader(f, 1<<20)).Decode(&auth) != nil || auth.Tokens.AccountID == "" {
		return nil
	}
	return "acct_" + digest(auth.Tokens.AccountID)[:12]
}

// Standard short-context USD per million tokens, in input/cached/output order.
// Source: https://developers.openai.com/api/docs/pricing (2026-11-19).
var standardRates = map[string][3]float64{
	"gpt-6.2-astra": {8, .4, 40}, "gpt-6.2-sol": {2, .1, 10}, "gpt-6-astra": {10, 1, 50},
	"gpt-6.1-sol": {2, .1, 10}, "gpt-6.1-terra": {1, .05, 5}, "gpt-6-luna": {.1, .01, .5},
	"gpt-6-sol": {2, .2, 10}, "gpt-5.6-sol": {4, .4, 20}, "gpt-5.6-terra": {2, .2, 12},
	"gpt-5.6-luna": {.2, .02, 1.2}, "gpt-5.5": {5, .5, 30}, "gpt-5.4": {2.5, .25, 15},
	"gpt-5.4-mini": {.75, .075, 4.5}, "gpt-5.4-nano": {.2, .02, 1.25}, "gpt-5.2": {1.75, .175, 14},
	"gpt-5.3-codex": {1.75, .175, 14}, "gpt-5.2-codex": {1.75, .175, 14}, "gpt-5.1-codex": {1.25, .125, 10}, "gpt-5.1": {1.25, .125, 10}, "gpt-5": {1.25, .125, 10}, "gpt-5-mini": {.25, .025, 2}, "gpt-5-nano": {.05, .005, .4},
}

func estimateCost(model, kind string, used counts) *float64 {
	rates, ok := standardRates[model]
	if !ok || kind != "chat" || used.Cached == nil || *used.Cached > used.Input {
		return nil
	}
	cost := (float64(used.Input-*used.Cached)*rates[0] + float64(*used.Cached)*rates[1] + float64(used.Output)*rates[2]) / 1e6
	return &cost
}

const ledger = ` FROM requests r LEFT JOIN request_metrics m ON m.request_id=r.id LEFT JOIN keys k ON k.id=r.owner `

// Count each successful metered chat once, using the output speed recorded for
// that request. Older ledgers and short replies may have no speed sample.
const requestTPS = `CASE WHEN r.kind='chat' AND r.status BETWEEN 200 AND 299 AND r.metered=1 AND r.output>=100 AND m.speed>0 THEN m.speed END`
const tpsStats = "AVG(" + requestTPS + ") avg_tps, MAX(" + requestTPS + ") peak_tps, COUNT(" + requestTPS + ") tps_samples"
const requestFields = "r.*,k.name key_name," + requestTPS + " tps"
const totals = `COUNT(*) requests,COUNT(CASE WHEN r.status>=400 THEN 1 END) errors,
COUNT(CASE WHEN r.metered=0 THEN 1 END) unmetered,COALESCE(SUM(r.input),0) input,COALESCE(SUM(r.output),0) output,
SUM(m.cached) cached,SUM(m.reasoning) reasoning,100.0*SUM(m.cached)/NULLIF(SUM(CASE WHEN m.cached IS NOT NULL THEN r.input END),0) hit_rate,
SUM(m.cost) cost,COUNT(m.cost) priced,COUNT(DISTINCT r.owner) active_keys,COUNT(DISTINCT m.account) active_accounts,
COUNT(CASE WHEN r.kind='image' AND r.status<400 THEN 1 END) image_ok,COUNT(CASE WHEN r.kind='image' AND r.status>=400 THEN 1 END) image_failed,
SUM(CASE WHEN r.kind='image' AND r.metered THEN r.input END) image_turn_input,SUM(CASE WHEN r.kind='image' AND r.metered THEN r.output END) image_turn_output,
AVG(r.latency_ms) latency_ms,AVG(m.ttft_ms) ttft_ms,AVG(m.speed) speed,` + tpsStats

func (a *app) analytics(w http.ResponseWriter, r *http.Request, _ string) {
	q := r.URL.Query()
	hours, step := q.Get("hours"), q.Get("step")
	if hours == "" {
		hours = "24"
	}
	if step == "" {
		step = "3600"
	}
	seconds, e := strconv.Atoi(step)
	if e != nil || (seconds != 300 && seconds != 3600 && seconds != 86400) {
		fail(w, 400, "invalid_request", "Invalid bucket size")
		return
	}
	stop := now()
	var start int64
	if hours != "all" {
		h, e := strconv.Atoi(hours)
		switch h {
		case 1, 6, 24, 72, 168, 720, 2160:
		default:
			e = strconv.ErrRange
		}
		if e != nil {
			fail(w, 400, "invalid_request", "Invalid history range")
			return
		}
		start = stop - int64(h)*3600
	}
	tx, e := a.db.BeginTx(r.Context(), nil)
	if e != nil {
		internal(w, e)
		return
	}
	defer tx.Rollback()
	where := " WHERE r.created<=?"
	args := []any{stop}
	for _, f := range []struct{ key, col string }{{"owner", "r.owner"}, {"model", "r.model"}, {"kind", "r.kind"}} {
		if v := strings.TrimSpace(q.Get(f.key)); v != "" {
			where += " AND " + f.col + "=?"
			args = append(args, v)
		}
	}
	lifetime, e := rows(tx, "SELECT "+totals+ledger+where, args...)
	if e != nil {
		internal(w, e)
		return
	}
	if start == 0 {
		e = tx.QueryRow("SELECT COALESCE(MIN(r.created),?)"+ledger+where, append([]any{stop}, args...)...).Scan(&start)
		if e != nil {
			internal(w, e)
			return
		}
	}
	where += " AND r.created>=?"
	args = append(args, start)
	summary, e := rows(tx, "SELECT "+totals+ledger+where, args...)
	if e != nil {
		internal(w, e)
		return
	}
	buckets, e := rows(tx, "SELECT (r.created/?)*? bucket,"+totals+ledger+where+" GROUP BY bucket ORDER BY bucket", append([]any{seconds, seconds}, args...)...)
	if e != nil {
		internal(w, e)
		return
	}
	models, e := rows(tx, "SELECT r.model,"+totals+ledger+where+" GROUP BY r.model ORDER BY requests DESC,r.model", args...)
	if e != nil {
		internal(w, e)
		return
	}
	keys, e := rows(tx, "SELECT r.owner,k.name,"+totals+ledger+where+" GROUP BY r.owner ORDER BY requests DESC,r.owner", args...)
	if e != nil {
		internal(w, e)
		return
	}
	report := M{"summary": summary[0], "lifetime": lifetime[0], "buckets": buckets, "models": models, "keys": keys, "start": start, "end": stop, "step": seconds}
	if q.Get("recent") == "1" {
		recent, err := rows(tx, "SELECT "+requestFields+ledger+where+" ORDER BY r.created DESC,r.rowid DESC LIMIT 8", args...)
		if err != nil {
			internal(w, err)
			return
		}
		report["recent"] = recent
	}
	if e = tx.Commit(); e != nil {
		internal(w, e)
		return
	}
	jsonOut(w, 200, report)
}
