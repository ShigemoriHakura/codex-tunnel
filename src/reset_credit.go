package main

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"time"
)

var resetAttemptID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func (a *app) consumeResetCredit(w http.ResponseWriter, r *http.Request, _ string) {
	var b struct {
		Confirmed bool    `json:"confirmed"`
		CreditID  *string `json:"creditId"`
		AttemptID string  `json:"idempotencyKey"`
	}
	if !body(w, r, &b) {
		return
	}
	if !b.Confirmed || !resetAttemptID.MatchString(b.AttemptID) || (b.CreditID != nil && strings.TrimSpace(*b.CreditID) == "") {
		fail(w, 400, "invalid_request", "Confirm the reset and provide a UUID for this attempt")
		return
	}
	if !a.usingResetCredit.TryLock() {
		fail(w, 409, "reset_in_progress", "A reset credit request is already in progress")
		return
	}
	defer a.usingResetCredit.Unlock()
	if !a.acquire(w) {
		return
	}
	defer a.release()
	run, err := a.current()
	if err != nil {
		internal(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	p, err := run.open(ctx, false)
	if err != nil {
		upstream(w, ctx, err)
		return
	}
	defer p.close()
	params := M{"idempotencyKey": b.AttemptID}
	if b.CreditID != nil {
		params["creditId"] = *b.CreditID
	}
	raw, err := p.call("account/rateLimitResetCredit/consume", params, nil)
	var reply struct {
		Outcome string `json:"outcome"`
	}
	if err == nil && json.Unmarshal(raw, &reply) == nil {
		switch reply.Outcome {
		case "reset", "nothingToReset", "noCredit", "alreadyRedeemed":
			jsonOut(w, http.StatusOK, reply)
			return
		}
	}
	fail(w, http.StatusBadGateway, "reset_outcome_unknown", "Could not confirm the reset outcome. Refresh the quota before retrying the same attempt.")
}
