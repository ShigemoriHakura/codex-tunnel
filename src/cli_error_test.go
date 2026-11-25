package main

import (
	"errors"
	"strings"
	"testing"
)

func TestCliCapacityErrorsBecomeImmediateUpstreamUnavailable(t *testing.T) {
	cases := []cliTurnError{
		{Message: "Selected model is at capacity. Please try a different model."},
		{Message: "Policy-restricted capacity is unavailable. Please try again later."},
		{Message: "Reconnecting... 1/5", Details: "Selected model is at capacity. Please try a different model."},
		{Message: `{"error":{"code":"model_at_capacity"}}`},
	}
	for _, detail := range cases {
		t.Run(detail.Message, func(t *testing.T) {
			if !detail.isCapacityUnavailable() {
				t.Fatalf("capacity error was not recognized: %+v", detail)
			}
			var got *responsesError
			if !errors.As(cliTurnFailure("failed", detail), &got) || got != errCapacityUnavailable {
				t.Fatalf("unexpected mapped error: %#v", cliTurnFailure("failed", detail))
			}
		})
	}
}

func TestCliCapacityErrorDoesNotTreatCyberTextAsCapacity(t *testing.T) {
	detail := cliTurnError{Message: "A normal prompt mentioned capacity while discussing cyber safety."}
	if detail.isCapacityUnavailable() {
		t.Fatal("ordinary text was treated as a capacity error")
	}
}

func TestCapacityNotificationEndsResponsesTurn(t *testing.T) {
	a := testApp(t, t.TempDir())
	key, _ := newKey(t, a)
	peer := &appServer{config: serverConfig{runner: a.base}, version: "0.171.0", ctx: a.servers.ctx, ready: make(chan struct{}), writes: make(chan M),
		pending: make(map[uint64]chan packet), threads: make(map[string]*turnInbox)}
	close(peer.ready)
	a.servers.current[false] = peer
	done := make(chan struct{})
	defer func() { a.servers.close(); <-done }()
	go func() {
		defer close(done)
		for {
			select {
			case <-peer.ctx.Done():
				return
			case frame := <-peer.writes:
				reply := M{}
				switch frame["method"] {
				case "thread/start":
					reply = M{"thread": M{"id": "thread"}, "model": "fixture"}
				case "turn/start":
					reply = M{"turn": M{"id": "current"}}
				case "turn/interrupt", "thread/unsubscribe":
				default:
					t.Errorf("unexpected RPC %v", frame["method"])
				}
				peer.dispatch(packet{ID: rawJSON(frame["id"]), Result: rawJSON(reply)})
				event := func(method string, params M) {
					params["threadId"] = "thread"
					peer.dispatch(packet{Method: method, Params: rawJSON(params)})
				}
				switch frame["method"] {
				case "turn/start":
					event("error", M{"turnId": "other", "willRetry": true, "error": M{
						"message":           "Reconnecting... 1/5",
						"additionalDetails": "Selected model is at capacity. Please try a different model.",
					}})
				case "turn/interrupt":
					event("turn/completed", M{"turn": M{"id": "current", "status": "interrupted"}})
				}
			}
		}
	}()
	r := hit(t, a, "POST", "/v1/responses", key, `{"model":"fixture","input":"capacity","stream":true}`, "", 200)
	if !strings.Contains(r, "Selected model is at capacity") {
		t.Fatalf("capacity detail missing from response: %s", r)
	}
}
