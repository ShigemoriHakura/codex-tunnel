package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestImageStreamTermination(t *testing.T) {
	b := imageRequest{Model: "gpt-image-2.5-sunburst", Stream: true}
	part := ": keepalive\r\nevent: " + imagePartial + "\r\ndata: {\"b64_json\":\"" + png + "\",\r\ndata: \"partial_image_index\":0}\r\n\r\n"
	for _, end := range []string{"", "data: [DONE]\n\n", "event: error\ndata: {\"error\":{\"code\":\"upstream_failure\"}}\n\n"} {
		calls := 0
		_, err := readImageEvents(strings.NewReader(part+end), b, func(e imageEvent) error { calls++; return nil })
		if err == nil || calls != 1 {
			t.Fatal("incomplete stream reported success or skipped the preview", err)
		}
	}
	cancel := errors.New("client left")
	_, err := readImageEvents(strings.NewReader(part), b, func(imageEvent) error { return cancel })
	if !errors.Is(err, cancel) {
		t.Fatal("client disconnect ignored", err)
	}
}

func TestImageStreamRejection(t *testing.T) {
	for _, tc := range []struct {
		body     string
		fallback bool
	}{
		{`{"detail":[{"loc":["body","stream"],"type":"extra_forbidden"},{"loc":["body","partial_images"],"type":"extra_forbidden"}]}`, true},
		{`{"detail":"Unsupported parameter: 'stream'"}`, true},
		{`{"error":{"code":"model_not_available","param":"model"}}`, false},
		{`{"error":{"code":"invalid_value","param":"partial_images"}}`, false},
	} {
		_, err := readProviderImage(&http.Response{StatusCode: 400, Body: io.NopCloser(strings.NewReader(tc.body))}, imageRequest{})
		var e *imageProviderError
		if !errors.As(err, &e) || e.streamUnsupported != tc.fallback {
			t.Fatal("wrong retry decision", err)
		}
	}
}

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
