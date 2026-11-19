package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSessionTurns(t *testing.T) {
	var q sessionTurns
	head, finish := q.join("same", 4)
	<-head
	second, cancel := q.join("same", 4)
	third, next := q.join("same", 4)
	fourth, last := q.join("same", 4)
	if _, extra := q.join("same", 4); extra != nil {
		t.Fatal("session exceeded its admission limit")
	}
	other, done := q.join("other", 4)
	<-other
	done()
	cancel()
	select {
	case <-second:
		t.Fatal("canceled waiter acquired the session")
	case <-third:
		t.Fatal("canceling a waiter released the active turn")
	default:
	}
	finish()
	<-third
	select {
	case <-fourth:
		t.Fatal("two active turns in one session")
	default:
	}
	next()
	<-fourth
	last()
	if len(q.lanes) != 0 {
		t.Fatal("empty session lanes were retained")
	}
}

func TestWorkerQueueCancellation(t *testing.T) {
	var a app
	a.initWorkers(1)
	a.slots <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := a.waitForWorker(ctx, nil); err != context.Canceled {
		t.Fatal("canceled waiter acquired a full worker", err)
	}
	a.release()
	if err := a.waitForWorker(ctx, nil); err != context.Canceled || len(a.slots) != 0 {
		t.Fatal("canceled waiter leaked a slot", err)
	}
}

func TestQueuedChatKeepalive(t *testing.T) {
	for _, stream := range []bool{false, true} {
		name := "json"
		if stream {
			name = "sse"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			a := testApp(t, t.TempDir())
			a.jsonKeepalive = true
			key, _ := newKey(t, a)
			session := prop(hit(t, a, "POST", "/v1/sessions", key, "{}", "", 201), "id")
			_, finish := a.sessionTurns.join(session, 4)
			server := httptest.NewServer(a.routes())
			defer server.Close()
			defer finish()
			client := &http.Client{Timeout: 12 * time.Second}
			payload, _ := json.Marshal(M{"model": "requested", "stream": stream, "messages": []message{{"user", "hello"}}})
			req, _ := http.NewRequest("POST", server.URL+"/v1/chat/completions", strings.NewReader(string(payload)))
			req.Header.Set("Authorization", "Bearer "+key)
			req.Header.Set("X-Session-ID", session)
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			reader := bufio.NewReader(resp.Body)
			line, err := reader.ReadString('\n')
			if err != nil || resp.StatusCode != 200 || (stream && line != ": keep-alive\n") || (!stream && line != "\n") {
				t.Fatal("missing queued heartbeat", line, err)
			}
			if len(a.slots) != 0 {
				t.Fatal("same-session waiter occupied an execution slot")
			}
			// Simulate the first turn pinning a model before releasing its session.
			if _, err := a.db.Exec("UPDATE sessions SET model='different' WHERE id=?", session); err != nil {
				t.Fatal(err)
			}
			finish()
			body, err := io.ReadAll(reader)
			if err != nil {
				t.Fatal(err)
			}
			if stream {
				if !strings.Contains(string(body), `"code":"model_mismatch"`) || !strings.HasSuffix(string(body), "data: [DONE]\n\n") {
					t.Fatal("incorrect SSE error", string(body))
				}
			} else {
				var obj struct{ Error struct{ Code string } }
				if json.Unmarshal(body, &obj) != nil || obj.Error.Code != "model_mismatch" {
					t.Fatal("invalid terminal JSON error", string(body))
				}
			}
			var status int
			if err := a.db.QueryRow("SELECT status FROM log_events WHERE request_id=? AND direction='ingress'", resp.Header.Get("X-Request-ID")).Scan(&status); err != nil || status != 400 {
				t.Fatal("lost logical error status", status, err)
			}
		})
	}
}
