package retry

import (
	"context"
	"math"
	"math/rand"
	"time"
)

// ExponentialRetryer provides exponential backoff with jitter for retrying operations.
// Matches AWS session-manager-plugin behavior (retry/retryer.go).
type ExponentialRetryer struct {
	// InitialDelay is the delay before the first retry.
	InitialDelay time.Duration
	// MaxDelay is the maximum delay between retries.
	MaxDelay time.Duration
	// Multiplier is the factor by which the delay increases each attempt.
	Multiplier float64
	// MaxAttempts is the maximum number of retry attempts. 0 means infinite.
	MaxAttempts int

	rng *rand.Rand
}

// DefaultRetryer returns a retryer with sensible defaults matching AWS behavior.
func DefaultRetryer() *ExponentialRetryer {
	return &ExponentialRetryer{
		InitialDelay: 1 * time.Second,
		MaxDelay:     30 * time.Second,
		Multiplier:   2.0,
		MaxAttempts:  10,
		rng:          rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

// Run executes the given function with exponential backoff retries.
// Returns nil on success, or the last error if all retries are exhausted.
func (r *ExponentialRetryer) Run(fn func() error) error {
	return r.RunContext(context.Background(), fn)
}

// RunContext is like Run but stops sleeping/retrying when ctx is canceled.
func (r *ExponentialRetryer) RunContext(ctx context.Context, fn func() error) error {
	var lastErr error
	attempt := 0

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		lastErr = fn()
		if lastErr == nil {
			return nil
		}

		attempt++
		if r.MaxAttempts > 0 && attempt >= r.MaxAttempts {
			return lastErr
		}

		delay := r.nextDelay(attempt)
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			if !t.Stop() {
				<-t.C
			}
			return ctx.Err()
		case <-t.C:
		}
	}
}

// nextDelay calculates the next delay with jitter.
// Formula: min(MaxDelay, InitialDelay * Multiplier^attempt) + jitter
func (r *ExponentialRetryer) nextDelay(attempt int) time.Duration {
	delay := float64(r.InitialDelay) * math.Pow(r.Multiplier, float64(attempt-1))

	if delay > float64(r.MaxDelay) {
		delay = float64(r.MaxDelay)
	}

	// Add jitter: random value between 0 and 25% of delay
	rng := r.rng
	if rng == nil {
		rng = rand.New(rand.NewSource(time.Now().UnixNano()))
	}
	jitter := rng.Float64() * delay * 0.25
	delay += jitter

	return time.Duration(delay)
}
