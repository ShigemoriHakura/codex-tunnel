package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	pngcodec "image/png"
	"math/rand"
	"net/http/httptest"
	"strings"
	"testing"
)

func fixtureImage(t *testing.T) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 600, 600))
	rng := rand.New(rand.NewSource(37))
	if _, err := rng.Read(img.Pix); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := pngcodec.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	data := base64.StdEncoding.EncodeToString(buf.Bytes())
	if len(data) <= 1<<20 {
		t.Fatal("expected a fixture larger than the original limit")
	}
	return data
}

func visionPayload(t *testing.T, text string, stream bool, urls ...string) string {
	t.Helper()
	var messages []M
	if stream {
		messages = append(messages, M{"role": "assistant", "content": "previous answer", "reasoning_content": "private-fixture-metadata", "tool_calls": nil})
	}
	parts := []M{{"type": "text", "text": text}}
	for _, url := range urls {
		parts = append(parts, M{"type": "image_url", "image_url": M{"url": url, "detail": "high"}})
	}
	messages = append(messages, M{"role": "user", "content": parts})
	raw, err := json.Marshal(M{"messages": messages, "stream": stream, "stream_options": M{"include_usage": true}})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
func TestChatContent(t *testing.T) {
	url := "data:image/png;base64," + png
	chat, err := chatResponseRequest([]chatMessage{{Role: "system", Content: json.RawMessage(`"Context"`)}, {Role: "user", Content: json.RawMessage(`[{"type":"image_url","image_url":{"url":"` + url + `","detail":"auto"}},{"type":"text","text":"read this"}]`)}}, "")
	if err != nil {
		t.Fatal(err)
	}
	var turn turn
	if err := turn.responseInput(chat.input); err != nil {
		t.Fatal(err)
	}
	if len(turn.history) != 1 || !strings.Contains(string(turn.history[0]), `"role":"system"`) || len(turn.input) != 2 || turn.input[0].URL != url || turn.input[0].Detail != "auto" || turn.input[1].Text != "read this" || strings.Contains(preview(responseMessages(chat.input)), url) {
		t.Fatal("multimodal history or typed user input changed")
	}
	for _, raw := range []string{`[{"type":"image_url","image_url":{"url":"https://example.invalid/image.png"}}]`, `[{"type":"image_url","image_url":{"url":"data:image/png;base64,` + png + `","detail":"medium"}}]`, `[{"type":"image_url","image_url":{"url":"file:///tmp/example"}}]`, `[{"type":"image_url","image_url":{"url":"data:image/png;base64,!"}}]`, `[]`, `null`} {
		if _, err := (chatMessage{Role: "user", Content: json.RawMessage(raw)}).parts(); err == nil {
			t.Fatal("accepted malformed image input")
		}
	}
	part, err := (chatMessage{Role: "user", Content: json.RawMessage(`[{"type":"image_url","image_url":{"url":"data:image/png;base64,` + png + `"}}]`)}).parts()
	if err != nil || len(part) != 1 || part[0].Type != "image" {
		t.Fatal("expected an image-only turn")
	}
	for _, length := range []int64{-1, 32} {
		r := httptest.NewRequest("POST", "/", strings.NewReader(`{"a":"123456789012345678901234"}`))
		r.ContentLength = length
		w := httptest.NewRecorder()
		if boundedBody(w, r, &M{}, 16) || w.Code != 413 {
			t.Fatal("expected 413 for oversized body")
		}
	}
}

func TestChatReasoningMetadata(t *testing.T) {
	const payload = `{"reasoning_content":"private-fixture-metadata","messages":[{"role":"assistant","content":null,"reasoning_content":"private-fixture-metadata","tool_calls":[]},{"role":"assistant","content":"visible answer","reasoning_content":"private-fixture-metadata"},{"role":"user","content":"continue","reasoning_content":null}]}`
	var b chatRequest
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(payload))
	if !boundedBody(httptest.NewRecorder(), r, &b, chatBodyLimit) {
		t.Fatal("reasoning metadata rejected by the request decoder")
	}
	chat, err := chatResponseRequest(b.Messages, b.Model)
	if err != nil {
		t.Fatal(err)
	}
	transcript := responseMessages(chat.input)
	if len(transcript) != 2 || transcript[0].Content != "visible answer" || transcript[1].Content != "continue" || strings.Contains(string(rawJSON(chat.input)), "private-fixture-metadata") {
		t.Fatal("incorrect public transcript")
	}
}

func TestChatAssistantContinuation(t *testing.T) {
	msgs := []chatMessage{
		{Role: "user", Content: json.RawMessage(`"fixture question"`)},
		{Role: "assistant", Content: json.RawMessage(`"fixture answer"`)},
	}
	if !chatMessagesCanEndTurn(msgs) {
		t.Fatal("assistant history cannot be used as a continuation")
	}
	chat, err := chatResponseRequest(msgs, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	var turn turn
	if err := turn.responseInput(chat.input); err != nil {
		t.Fatal(err)
	}
	if len(turn.history) != 2 || len(turn.input) != 0 {
		t.Fatalf("unexpected continuation context: history=%d input=%d", len(turn.history), len(turn.input))
	}
}
