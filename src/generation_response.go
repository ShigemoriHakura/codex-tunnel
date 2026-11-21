package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

type generationResponse struct {
	w         http.ResponseWriter
	mu        sync.Mutex
	opened    bool
	jsonBody  bool
	image     bool
	responses *responseStream
	sequence  int
}

// Open on the first event or heartbeat. Fast validation and provider errors can
// still use an ordinary HTTP status before the first stream bytes are committed.
func (s *generationResponse) openLocked() {
	if !s.opened {
		if s.jsonBody {
			s.w.Header().Set("Content-Type", "application/json; charset=utf-8")
			// A compressor can buffer whitespace until the final object is available.
			s.w.Header().Set("Content-Encoding", "identity")
		} else {
			s.w.Header().Set("Content-Type", "text/event-stream")
		}
		s.w.Header().Set("Cache-Control", "no-store, no-transform")
		s.w.Header().Set("X-Accel-Buffering", "no")
		s.opened = true
	}
}

func (s *generationResponse) write(kind string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.openLocked()
	if kind != "" {
		if _, err = fmt.Fprintf(s.w, "event: %s\n", kind); err != nil {
			return err
		}
	}
	if _, err = io.WriteString(s.w, "data: "); err != nil {
		return err
	}
	if _, err = s.w.Write(data); err != nil {
		return err
	}
	if _, err = io.WriteString(s.w, "\n\n"); err != nil {
		return err
	}
	return http.NewResponseController(s.w).Flush()
}

func (s *generationResponse) finishJSON(value any) error {
	if !s.opened {
		s.w.Header().Set("Content-Type", "application/json; charset=utf-8")
	}
	if err := json.NewEncoder(s.w).Encode(value); err != nil {
		return err
	}
	return http.NewResponseController(s.w).Flush()
}

func (s *generationResponse) heartbeat() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.openLocked()
	frame := ": keep-alive\n\n"
	if s.jsonBody {
		frame = "\n"
	}
	if _, err := io.WriteString(s.w, frame); err != nil {
		return err
	}
	return http.NewResponseController(s.w).Flush()
}

// Stop and join the timer before recording the outcome or emitting a terminal response.
func (s *generationResponse) startHeartbeats(ctx context.Context, cancel context.CancelFunc, interval time.Duration) func() {
	life, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(interval)
		defer tick.Stop()
		for {
			select {
			case <-life.Done():
				return
			case <-tick.C:
				if s.heartbeat() != nil {
					cancel()
					return
				}
			}
		}
	}()
	return func() { stop(); <-done }
}

// Called only after the heartbeat writer has stopped. After commitment the
// original HTTP status is fixed, but diagnostics and the response body retain the error.
func (s *generationResponse) reject(status int, code, message string) {
	if o, ok := s.w.(*observedWriter); ok {
		o.trace.status, o.trace.code, o.trace.message = status, code, message
	}
	if status == 499 {
		return
	}
	if !s.opened {
		if code == errCyberPolicy.code {
			s.w.Header().Set("X-Should-Retry", "false")
		}
		fail(s.w, status, code, message)
		return
	}
	if s.jsonBody {
		_ = s.finishJSON(M{"error": M{"code": code, "message": message}})
		return
	}
	detail := M{"code": code, "message": message}
	if s.responses != nil {
		// Codex treats a bare error event followed by EOF as a disconnected
		// stream. response.failed is the terminal Responses event for failures.
		if status == 400 && code == "invalid_request" {
			detail["code"] = "invalid_prompt"
		}
		response := s.responses.envelope("failed", nil)
		response["error"] = rawJSON(detail)
		// A reserved output slot can still be empty when a live item fails.
		output := make(responseItems, 0, len(s.responses.items))
		for _, item := range s.responses.items {
			if item != nil {
				output = append(output, item)
			}
		}
		response["output"] = rawJSON(output)
		_ = s.responses.send("response.failed", M{"response": response})
	} else if s.image {
		s.write("error", M{"type": "error", "error": detail})
	} else {
		s.write("", M{"error": detail})
		io.WriteString(s.w, "data: [DONE]\n\n")
	}
}
