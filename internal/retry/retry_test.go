package retry

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRunContext_CancelsDuringBackoff(t *testing.T) {
	r := &ExponentialRetryer{
		InitialDelay: 200 * time.Millisecond,
		MaxDelay:     200 * time.Millisecond,
		Multiplier:   1.0,
		MaxAttempts:  0,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := r.RunContext(ctx, func() error { return errors.New("fail") })
	d := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline exceeded, got %v", err)
	}
	if d > 150*time.Millisecond {
		t.Fatalf("expected cancel to stop retries quickly; took %v", d)
	}
}

func TestRunContext_MaxAttempts(t *testing.T) {
	sentinel := errors.New("boom")
	r := &ExponentialRetryer{
		InitialDelay: 1 * time.Millisecond,
		MaxDelay:     1 * time.Millisecond,
		Multiplier:   1.0,
		MaxAttempts:  3,
	}

	calls := 0
	err := r.RunContext(context.Background(), func() error {
		calls++
		return sentinel
	})

	if !errors.Is(err, sentinel) {
		t.Fatalf("expected sentinel error, got %v", err)
	}
	if calls != 3 {
		t.Fatalf("expected 3 calls, got %d", calls)
	}
}
