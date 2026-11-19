package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"io"
	"strings"
)

const (
	imagePartial   = "image_generation.partial_image"
	imageCompleted = "image_generation.completed"
	maxImageEvent  = maxGeneratedImageBytes*4/3 + (1 << 20)
)

type imageMetadata struct {
	Created      int64           `json:"created_at"`
	Size         string          `json:"size"`
	Quality      string          `json:"quality"`
	Background   string          `json:"background"`
	OutputFormat string          `json:"output_format"`
	Usage        json.RawMessage `json:"usage,omitempty"`
}

type imageEvent struct {
	imageMetadata
	Type  string `json:"type"`
	Bytes []byte `json:"b64_json"`
	Index *int   `json:"partial_image_index,omitempty"`
}

func (b imageRequest) metadata(meta imageMetadata) imageMetadata {
	if meta.Created == 0 {
		meta.Created = now()
	}
	if meta.Size == "" {
		meta.Size = b.Size
	}
	if meta.Size == "" {
		meta.Size = "auto"
	}
	if meta.Quality == "" {
		meta.Quality = b.Quality
	}
	if meta.Quality == "" {
		meta.Quality = "auto"
	}
	if meta.Background == "" {
		meta.Background = b.Background
	}
	if meta.Background == "" {
		meta.Background = "auto"
	}
	meta.OutputFormat = b.Format
	return meta
}

func (b imageRequest) completedEvent(res result) imageEvent {
	meta := b.metadata(res.imageMeta)
	// Workspace responses can contain dimensions different from the requested canvas.
	if cfg, _, err := image.DecodeConfig(bytes.NewReader(res.image)); err == nil {
		meta.Size = fmt.Sprintf("%dx%d", cfg.Width, cfg.Height)
	}
	return imageEvent{imageMetadata: meta, Type: imageCompleted, Bytes: res.image}
}

// Each event is bounded independently so previews can be flushed and discarded
// before reading the final image. A stream must end with a completed image.
func readImageEvents(r io.Reader, b imageRequest, partial func(imageEvent) error) (result, error) {
	scan := bufio.NewScanner(io.LimitReader(r, 4*maxImageEvent+(1<<20)))
	scan.Buffer(make([]byte, 4096), maxImageEvent)
	var data bytes.Buffer
	var name string
	previews := 0
	dispatch := func() (result, bool, error) {
		defer data.Reset()
		if data.Len() == 0 {
			return result{}, false, nil
		}
		raw := data.Bytes()
		if bytes.Equal(bytes.TrimSpace(raw), []byte("[DONE]")) {
			return result{}, false, errNoImage
		}
		var event struct {
			imageEvent
			Model string
			Error *struct{ Code string }
			Code  string
		}
		if err := json.Unmarshal(raw, &event); err != nil {
			return result{}, false, errors.New("invalid image stream event")
		}
		if event.Type == "" {
			event.Type = name
		}
		switch event.Type {
		case "error", "image_generation.failed":
			code := event.Code
			if event.Error != nil {
				code = event.Error.Code
			}
			if !imageErrorCode.MatchString(code) {
				code = "image_stream_error"
			}
			return result{}, false, &imageProviderError{status: 502, code: code}
		case imagePartial, imageCompleted:
		default:
			return result{}, false, nil
		}
		res, err := b.providerResult(event.Model, event.Bytes, event.imageMetadata)
		if err != nil {
			return result{}, false, err
		}
		if event.Type == imageCompleted {
			return res, true, nil
		}
		if previews >= 3 {
			return result{}, false, errors.New("too many image previews")
		}
		index := previews
		previews++
		if event.Index == nil {
			event.Index = &index
		}
		event.Bytes = res.image
		event.imageMetadata = res.imageMeta
		return result{}, false, partial(event.imageEvent)
	}
	for scan.Scan() {
		line := scan.Bytes()
		if len(line) == 0 {
			res, done, err := dispatch()
			if err != nil || done {
				return res, err
			}
			name = ""
			continue
		}
		field, value, _ := bytes.Cut(line, []byte(":"))
		value = bytes.TrimPrefix(value, []byte(" "))
		switch string(field) {
		case "event":
			if len(value) > 128 {
				return result{}, errors.New("invalid image event name")
			}
			name = string(value)
		case "data":
			if data.Len()+len(value)+1 > maxImageEvent {
				return result{}, errors.New("image event too large")
			}
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.Write(value)
		}
	}
	if err := scan.Err(); err != nil {
		return result{}, err
	}
	if res, done, err := dispatch(); done || err != nil {
		return res, err
	}
	return result{}, errNoImage
}

func isEventStream(contentType string) bool {
	mime, _, _ := strings.Cut(contentType, ";")
	return strings.EqualFold(strings.TrimSpace(mime), "text/event-stream")
}
