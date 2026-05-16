package pipeline

import (
	"testing"
	"time"
)

func TestBackoffDefaultSchedule(t *testing.T) {
	b := DefaultBackoff()
	want := []time.Duration{
		1 * time.Second,
		5 * time.Second,
		15 * time.Second,
		60 * time.Second,
		5 * time.Minute,
	}
	for i, d := range want {
		got, isFinal := b.NextDelay(i + 1)
		if isFinal {
			t.Fatalf("attempt %d: isFinal=true unexpectedly", i+1)
		}
		if got != d {
			t.Errorf("attempt %d: got %s, want %s", i+1, got, d)
		}
	}
}

func TestBackoffMaxAttempts(t *testing.T) {
	b := DefaultBackoff()
	if _, isFinal := b.NextDelay(6); !isFinal {
		t.Fatalf("attempt=MaxAttempts should be final")
	}
	if _, isFinal := b.NextDelay(7); !isFinal {
		t.Fatalf("attempt>MaxAttempts should also be final")
	}
}

func TestBackoffAttemptZeroClamps(t *testing.T) {
	b := DefaultBackoff()
	d, isFinal := b.NextDelay(0)
	if isFinal {
		t.Fatalf("isFinal=true on attempt=0")
	}
	if d != time.Second {
		t.Fatalf("got %s, want 1s", d)
	}
}

func TestBackoffEmptySchedule(t *testing.T) {
	b := Backoff{Schedule: nil, MaxAttempts: 3}
	d, isFinal := b.NextDelay(1)
	if isFinal {
		t.Fatalf("expected not final at attempt=1 below max")
	}
	if d != 0 {
		t.Fatalf("expected zero delay for empty schedule, got %v", d)
	}
	if _, isFinal := b.NextDelay(3); !isFinal {
		t.Fatalf("attempt==max must be final")
	}
}

func TestBackoffScheduleShorterThanMax(t *testing.T) {
	// schedule of 2 entries but max=5 -> attempts 1..4 reuse last entry, attempt 5 is final.
	b := Backoff{Schedule: []time.Duration{time.Second, 2 * time.Second}, MaxAttempts: 5}
	cases := map[int]time.Duration{
		1: time.Second,
		2: 2 * time.Second,
		3: 2 * time.Second, // clamped
		4: 2 * time.Second,
	}
	for attempt, want := range cases {
		got, final := b.NextDelay(attempt)
		if final {
			t.Errorf("attempt %d: unexpectedly final", attempt)
		}
		if got != want {
			t.Errorf("attempt %d: got %s want %s", attempt, got, want)
		}
	}
	if _, final := b.NextDelay(5); !final {
		t.Fatalf("attempt=max should be final")
	}
}

func TestBackoffMaxAttemptsZeroDefaultsToScheduleLen(t *testing.T) {
	// When MaxAttempts is unset, fall back to the schedule length.
	b := Backoff{Schedule: []time.Duration{time.Second, time.Second}}
	if _, final := b.NextDelay(1); final {
		t.Fatalf("attempt 1 should not be final")
	}
	if _, final := b.NextDelay(2); !final {
		t.Fatalf("attempt 2 should be final (schedule len)")
	}
}
