package main

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"strings"
)

const (
	imageCompleted   = "image_generation.completed"
	pinnedImageModel = "gpt-image-2"
)

func isImageEngine(model string) bool {
	model = strings.TrimPrefix(model, "openai/")
	return strings.HasPrefix(model, "gpt-image-") || model == "chatgpt-image-latest"
}

func (b imageRequest) cliOptions() error {
	switch {
	case isImageEngine(b.Model) && b.Model != pinnedImageModel:
		return errors.New("The CLI cannot select the requested image engine. Use " + pinnedImageModel + ", omit model, or supply a Codex conversation model")
	case b.PartialImages != nil && *b.PartialImages != 0:
		return errors.New("The CLI image tool returns a completed image only. Omit partial_images or set it to 0")
	case b.Size != "" && b.Size != "auto":
		return errors.New("The CLI image tool supports size=auto")
	case b.Quality != "" && b.Quality != "auto":
		return errors.New("The CLI image tool supports quality=auto")
	case b.Background != "" && b.Background != "auto":
		return errors.New("The CLI image tool supports background=auto")
	case b.Moderation != "" || b.SafetyIdentifier != "" || b.User != "":
		return errors.New("The CLI image tool does not accept moderation, safety_identifier or user")
	}
	return nil
}

func (b imageRequest) completedEvent(res result) M {
	event := M{"type": imageCompleted, "created_at": now(), "output_format": b.Format, "b64_json": res.image}
	if res.imageModel != "" {
		event["model"] = res.imageModel
	}
	if cfg, _, err := image.DecodeConfig(bytes.NewReader(res.image)); err == nil {
		event["size"] = fmt.Sprintf("%dx%d", cfg.Width, cfg.Height)
	}
	return event
}
