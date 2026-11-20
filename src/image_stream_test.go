package main

import (
	"bytes"
	"context"
	"image"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type failedImageWriter struct{ *httptest.ResponseRecorder }

func (w failedImageWriter) FlushError() error { return io.ErrClosedPipe }

func TestImageHeartbeatCancelsFailedDelivery(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := generationResponse{w: failedImageWriter{httptest.NewRecorder()}}
	stop := s.startHeartbeats(ctx, cancel, time.Millisecond)
	defer stop()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("heartbeat write failure did not cancel image generation")
	}
	stop()
	if !s.opened || errorStatus(ctx, ctx.Err()) != 499 {
		t.Fatal("lost downstream disconnect")
	}
	expired, end := context.WithTimeout(context.Background(), 0)
	defer end()
	if errorStatus(expired, expired.Err()) != 504 {
		t.Fatal("gateway deadline must remain 504")
	}
}

// Supply a CLI peer with a project login and the image capability. The real-CLI
// integration suite separately exercises providers that return only text.
func TestCLIImageEvents(t *testing.T) {
	a := testApp(t, t.TempDir())
	key, _ := newKey(t, a)
	peer := &appServer{
		config: serverConfig{runner: a.base}, image: true, ctx: a.servers.ctx,
		ready: make(chan struct{}), writes: make(chan M), denials: make(chan M),
		pending: make(map[uint64]chan packet), threads: make(map[string]*turnInbox),
	}
	close(peer.ready)
	a.servers.current[true] = peer
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
				case "account/read":
					reply = M{"account": M{"type": "apiKey"}}
				case "modelProvider/capabilities/read":
					reply = M{"imageGeneration": true}
				case "thread/start":
					reply = M{"thread": M{"id": "image-thread"}, "model": "fixture-model"}
				case "turn/start":
					reply = M{"turn": M{"id": "image-turn"}}
				case "thread/unsubscribe":
				default:
					t.Errorf("unexpected image RPC %v", frame["method"])
				}
				peer.dispatch(packet{ID: rawJSON(frame["id"]), Result: rawJSON(reply)})
				if frame["method"] == "turn/start" {
					for _, e := range []struct {
						method string
						field  string
						value  M
					}{
						{"item/completed", "item", M{"type": "imageGeneration", "status": "completed", "result": png}},
						{"thread/tokenUsage/updated", "tokenUsage", M{"total": counts{Input: 11, Output: 20}}},
						{"turn/completed", "turn", M{"id": "image-turn", "status": "completed"}},
					} {
						peer.dispatch(packet{Method: e.method, Params: rawJSON(M{"threadId": "image-thread", "turnId": "image-turn", e.field: e.value})})
					}
				}
			}
		}
	}()
	for _, stream := range []bool{false, true} {
		body := hit(t, a, "POST", "/v1/images/generations", key, string(rawJSON(M{"prompt": "draw", "stream": stream, "output_format": "webp"})), "", 200)
		var obj struct {
			Type  string
			Size  string
			Bytes []byte `json:"b64_json"`
			Data  []struct {
				Bytes []byte `json:"b64_json"`
			}
		}
		if stream {
			_, body, _ = strings.Cut(body, "data: ")
		}
		decode(t, body, &obj)
		data := obj.Bytes
		if stream {
			if obj.Type != imageCompleted || obj.Size != "1x1" {
				t.Fatalf("invalid terminal image event %s", body)
			}
		} else {
			if len(obj.Data) != 1 {
				t.Fatal("missing generated image")
			}
			data = obj.Data[0].Bytes
		}
		_, format, err := image.DecodeConfig(bytes.NewReader(data))
		if err != nil || format != "webp" {
			t.Fatal("invalid encoded image", err)
		}
	}
	var metered int
	if err := a.db.QueryRow("SELECT count(*) FROM requests WHERE kind='image' AND model='fixture-model' AND input=11 AND output=20 AND status=200").Scan(&metered); err != nil || metered != 2 {
		t.Fatal(metered, err)
	}
}
