package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const chatBodyLimit = 24 << 20

type chatMessage struct {
	Role             string            `json:"role"`
	Content          json.RawMessage   `json:"content"`
	ReasoningContent *string           `json:"reasoning_content,omitempty"`
	ToolCalls        []json.RawMessage `json:"tool_calls,omitempty"`
}
type turnInput struct {
	Type   string `json:"type"`
	Text   string `json:"text,omitempty"`
	URL    string `json:"url,omitempty"`
	Detail string `json:"detail,omitempty"`
}
type contentPart struct {
	Type  string                        `json:"type"`
	Text  *string                       `json:"text,omitempty"`
	Image *struct{ URL, Detail string } `json:"image_url,omitempty"`
}

func validInlineImage(raw string) bool {
	header, data, ok := strings.Cut(raw, ",")
	if !ok || data == "" {
		return false
	}
	var mime string
	switch header {
	case "data:image/png;base64", "data:image/jpeg;base64", "data:image/webp;base64", "data:image/gif;base64":
		mime = strings.TrimSuffix(strings.TrimPrefix(header, "data:"), ";base64")
	default:
		return false
	}
	reader := base64.NewDecoder(base64.StdEncoding, strings.NewReader(data))
	var prefix [512]byte
	n, err := io.ReadFull(reader, prefix[:])
	if err != nil && err != io.ErrUnexpectedEOF {
		return false
	}
	if http.DetectContentType(prefix[:n]) != mime {
		return false
	}
	_, err = io.Copy(io.Discard, reader)
	return err == nil
}

func (m chatMessage) parts() ([]turnInput, error) {
	if len(m.ToolCalls) != 0 {
		return nil, errors.New("Chat tool calls are not supported by this gateway")
	}
	switch m.Role {
	case "user", "assistant", "developer", "system":
	default:
		return nil, errors.New("Unsupported message role")
	}
	raw := bytes.TrimSpace(m.Content)
	// Some clients replay a reasoning-only assistant message with no visible answer.
	reasoningOnlyAllowed := m.Role == "assistant" && m.ReasoningContent != nil
	if reasoningOnlyAllowed && (len(raw) == 0 || bytes.Equal(raw, []byte("null"))) {
		return nil, nil
	}
	if len(raw) > 0 && raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, err
		}
		if strings.TrimSpace(s) != "" {
			return []turnInput{{Type: "text", Text: s}}, nil
		}
		if reasoningOnlyAllowed {
			return nil, nil
		}
		return nil, errors.New("Message text is empty")
	}
	var parts []contentPart
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&parts); err != nil {
		return nil, errors.New("Use a text string or an array of text and image_url parts")
	}
	var input []turnInput
	for _, part := range parts {
		switch part.Type {
		case "text":
			if part.Text == nil || part.Image != nil {
				return nil, errors.New("Invalid text part")
			}
			if *part.Text != "" {
				input = append(input, turnInput{Type: "text", Text: *part.Text})
			}
		case "image_url":
			if m.Role != "user" || part.Text != nil || part.Image == nil || !validInlineImage(part.Image.URL) {
				return nil, errors.New("User image_url must contain an inline base64 PNG, JPEG, WebP or GIF data URL")
			}
			switch part.Image.Detail {
			case "", "auto", "high", "original":
			default:
				return nil, errors.New("Use auto, high or original for image_url.detail")
			}
			input = append(input, turnInput{Type: "image", URL: part.Image.URL, Detail: part.Image.Detail})
		default:
			return nil, errors.New("Unsupported content part; use text or image_url")
		}
	}
	for _, p := range input {
		if p.Type == "image" || strings.TrimSpace(p.Text) != "" {
			return input, nil
		}
	}
	if reasoningOnlyAllowed {
		return nil, nil
	}
	return nil, errors.New("Message must contain text or an image")
}

// Preserve role boundaries for stateless history. The transcript contains only text
// and numbered references. The actual images travel as typed CLI inputs.
func chatInputs(messages []chatMessage) ([]turnInput, []message, error) {
	var images, single []turnInput
	transcript := make([]message, 0, len(messages))
	for _, m := range messages {
		parts, err := m.parts()
		if err != nil {
			return nil, nil, err
		}
		if len(parts) == 0 {
			continue
		}
		var summary strings.Builder
		for _, p := range parts {
			if p.Type == "image" {
				images = append(images, p)
				fmt.Fprintf(&summary, "[Image %d]", len(images))
			} else {
				summary.WriteString(p.Text)
			}
		}
		transcript = append(transcript, message{m.Role, summary.String()})
		if len(transcript) == 1 {
			single = parts
		}
	}
	if len(transcript) == 1 {
		return single, transcript, nil
	}
	raw, _ := json.Marshal(transcript)
	input := []turnInput{{Type: "text", Text: "Continue this conversation, answering the final user message:\n" + string(raw)}}
	for i, img := range images {
		input = append(input, turnInput{Type: "text", Text: fmt.Sprintf("Image %d:", i+1)}, img)
	}
	return input, transcript, nil
}
