package main

import (
	"errors"
	"slices"
	"strings"
)

// Only app-server's public summary text enters these items.
type responseSummary struct {
	cliID, id string
	index     int
	parts     []*strings.Builder
}

func (s *responseStream) summaryFor(cliID string) *responseSummary {
	for _, item := range s.pendingSummaries {
		if item.cliID == cliID {
			return item
		}
	}
	return nil
}

func (s *responseStream) summaryPart(cliID string, index int, text string) error {
	if cliID == "" || index < 0 {
		return errors.New("invalid CLI reasoning summary index or ID")
	}
	item := s.summaryFor(cliID)
	if item == nil {
		if index != 0 {
			return nil // An out-of-order part is recovered from the completed snapshot.
		}
		item = &responseSummary{cliID: cliID, id: id("rs_"), index: len(s.items)}
		start := M{"type": "reasoning", "id": item.id, "summary": []M{}, "status": "in_progress"}
		s.items = append(s.items, rawJSON(start))
		s.pendingSummaries = append(s.pendingSummaries, item)
		if err := s.send("response.output_item.added", M{"output_index": item.index, "item": start}); err != nil {
			return err
		}
	}
	if index > len(item.parts) {
		// A CLI can omit deltas for an earlier part. Its completed item supplies the full list.
		return nil
	}
	if index == len(item.parts) {
		item.parts = append(item.parts, new(strings.Builder))
		if err := s.send("response.reasoning_summary_part.added", M{"output_index": item.index, "item_id": item.id, "summary_index": index, "part": M{"type": "summary_text", "text": ""}}); err != nil {
			return err
		}
	}
	if text == "" {
		return nil
	}
	item.parts[index].WriteString(text)
	return s.send("response.reasoning_summary_text.delta", M{"output_index": item.index, "item_id": item.id, "summary_index": index, "delta": text})
}

func (s *responseStream) finishSummary(cliID string, final []string) error {
	item := s.summaryFor(cliID)
	if item == nil && !slices.ContainsFunc(final, func(v string) bool { return v != "" }) {
		return nil
	}
	for i, text := range final {
		// A completed snapshot may include text already sent as live deltas.
		if item != nil && i < len(item.parts) {
			prefix := item.parts[i].String()
			if !strings.HasPrefix(text, prefix) {
				continue // The done event below carries the authoritative snapshot.
			}
			text = text[len(prefix):]
		}
		if err := s.summaryPart(cliID, i, text); err != nil {
			return err
		}
		if item == nil {
			item = s.summaryFor(cliID)
		}
	}
	parts := make([]M, len(item.parts))
	for i, builder := range item.parts {
		text := builder.String()
		if i < len(final) {
			text = final[i]
		}
		parts[i] = M{"type": "summary_text", "text": text}
		if err := s.send("response.reasoning_summary_text.done", M{"output_index": item.index, "item_id": item.id, "summary_index": i, "text": text}); err != nil {
			return err
		}
		if err := s.send("response.reasoning_summary_part.done", M{"output_index": item.index, "item_id": item.id, "summary_index": i, "part": parts[i]}); err != nil {
			return err
		}
	}
	done := M{"id": item.id, "type": "reasoning", "status": "completed", "summary": parts}
	s.items[item.index] = rawJSON(done)
	s.pendingSummaries = slices.DeleteFunc(s.pendingSummaries, func(v *responseSummary) bool { return v == item })
	return s.send("response.output_item.done", M{"output_index": item.index, "item": done})
}
