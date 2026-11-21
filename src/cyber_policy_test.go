package main

import (
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
)

func TestCyberRerouteInterruptsOnlyItsTurn(t *testing.T) {
	a := testApp(t, t.TempDir())
	key, _ := newKey(t, a)
	peer := &appServer{config: serverConfig{runner: a.base}, version: "0.171.0", ctx: a.servers.ctx, ready: make(chan struct{}), writes: make(chan M),
		pending: make(map[uint64]chan packet), threads: make(map[string]*turnInbox)}
	close(peer.ready)
	a.servers.current[false] = peer
	done := make(chan struct{})
	defer func() { a.servers.close(); <-done }()
	var starts, interrupts atomic.Int32
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
				case "turn/interrupt":
					interrupts.Add(1)
				case "thread/unsubscribe":
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
					if starts.Add(1) == 1 {
						event("model/rerouted", M{"turnId": "previous", "reason": "highRiskCyberActivity"})
						event("error", M{"turnId": "current", "willRetry": true, "error": M{"codexErrorInfo": M{"responseStreamDisconnected": M{"httpStatusCode": nil}}, "message": "cyber_policy is mentioned in an ordinary transport error"}})
						event("item/completed", M{"turnId": "current", "item": M{"type": "agentMessage", "text": "fixture-ok"}})
						event("turn/completed", M{"turn": M{"id": "current", "status": "completed"}})
					} else {
						event("model/rerouted", M{"turnId": "current", "reason": "highRiskCyberActivity"})
					}
				case "turn/interrupt":
					event("turn/completed", M{"turn": M{"id": "current", "status": "interrupted"}})
				}
			}
		}
	}()
	post := func(input string, status int) string {
		return hit(t, a, "POST", "/v1/responses", key, string(rawJSON(M{"model": "fixture", "input": input})), "", status)
	}
	if !strings.Contains(post("first", 200), "fixture-ok") {
		t.Fatal("unrelated notice interrupted the turn")
	}
	for range 2 {
		var response struct{ Error struct{ Code string } }
		if err := json.Unmarshal([]byte(post("second", 400)), &response); err != nil || response.Error.Code != "cyber_policy" {
			t.Fatal(err, response)
		}
	}
	if starts.Load() != 2 || interrupts.Load() != 1 {
		t.Fatalf("starts=%d interrupts=%d", starts.Load(), interrupts.Load())
	}
}
