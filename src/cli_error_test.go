package main

import (
	"errors"
	"testing"
)

func TestCliCapacityErrorsBecomeImmediateUpstreamUnavailable(t *testing.T) {
	cases := []cliTurnError{
		{Message: "Selected model is at capacity. Please try a different model."},
		{Message: "Policy-restricted capacity is unavailable. Please try again later."},
		{Message: "Reconnecting... 1/5", Details: "Selected model is at capacity. Please try a different model."},
		{Message: `{"error":{"code":"model_at_capacity"}}`},
	}
	for _, detail := range cases {
		t.Run(detail.Message, func(t *testing.T) {
			if !detail.isCapacityUnavailable() {
				t.Fatalf("capacity error was not recognized: %+v", detail)
			}
			var got *responsesError
			if !errors.As(cliTurnFailure("failed", detail), &got) || got != errCapacityUnavailable {
				t.Fatalf("unexpected mapped error: %#v", cliTurnFailure("failed", detail))
			}
		})
	}
}

func TestCliCapacityErrorDoesNotTreatCyberTextAsCapacity(t *testing.T) {
	detail := cliTurnError{Message: "A normal prompt mentioned capacity while discussing cyber safety."}
	if detail.isCapacityUnavailable() {
		t.Fatal("ordinary text was treated as a capacity error")
	}
}
