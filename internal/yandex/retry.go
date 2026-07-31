package yandex

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"time"
)

// RetryPolicy controls how a failed request is repeated.
//
// Reads and writes want different policies: a background poll can afford to
// back off for a while, but a write comes from someone standing at a light
// switch, so it gets fewer attempts and shorter delays.
type RetryPolicy struct {
	MaxAttempts int
	BaseDelay   time.Duration
	MaxDelay    time.Duration
}

// ReadPolicy is used for polling. Nobody is waiting, so back off generously.
var ReadPolicy = RetryPolicy{MaxAttempts: 4, BaseDelay: time.Second, MaxDelay: 30 * time.Second}

// WritePolicy is used for device actions. A person is waiting for a light to
// come on, so give up quickly and let HomeKit show "Not Responding".
var WritePolicy = RetryPolicy{MaxAttempts: 3, BaseDelay: 300 * time.Millisecond, MaxDelay: 2 * time.Second}

// ConfirmPolicy is used for the single-device reads that follow a write.
//
// These run inside a short confirmation window and repeat on their own every
// second, so a generous backoff would simply consume the window and turn every
// slow response into a context deadline. Failing fast and letting the next tick
// try again is both quicker and quieter.
var ConfirmPolicy = RetryPolicy{MaxAttempts: 2, BaseDelay: 250 * time.Millisecond, MaxDelay: time.Second}

// Do runs fn, retrying while the error is retryable and attempts remain. The
// error returned is the last one, so the caller sees the real cause rather
// than a generic "gave up".
func (p RetryPolicy) Do(ctx context.Context, logger *slog.Logger, op string, fn func(context.Context) error) error {
	attempts := max(p.MaxAttempts, 1)

	var err error
	for attempt := range attempts {
		if ctxErr := ctx.Err(); ctxErr != nil {
			if err != nil {
				return err
			}
			return ctxErr
		}

		err = fn(ctx)
		if err == nil {
			if attempt > 0 && logger != nil {
				logger.Info("recovered after retry", slog.String("op", op), slog.Int("attempts", attempt+1))
			}
			return nil
		}
		if !Retryable(err) {
			return err
		}
		if attempt == attempts-1 {
			break
		}

		delay := p.delay(attempt, err)
		if logger != nil {
			logger.Warn("request failed, retrying",
				slog.String("op", op),
				slog.Int("attempt", attempt+1),
				slog.Int("max_attempts", attempts),
				slog.Duration("delay", delay),
				slog.String("error", err.Error()))
		}
		if sleepErr := sleep(ctx, delay); sleepErr != nil {
			return err
		}
	}

	return err
}

// delay computes the wait before the next attempt using exponential backoff
// with full jitter. Jitter matters even for a single client: without it every
// retry after a Yandex outage lands at the same instant across every bridge.
//
// A Retry-After header wins outright — the server said how long to wait.
func (p RetryPolicy) delay(attempt int, err error) time.Duration {
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.RetryAfter > 0 {
		return min(apiErr.RetryAfter, p.MaxDelay)
	}

	base := p.BaseDelay
	if base <= 0 {
		base = time.Second
	}
	maxDelay := p.MaxDelay
	if maxDelay <= 0 {
		maxDelay = 30 * time.Second
	}

	// Cap the shift before it overflows on a long-running retry loop.
	backoff := maxDelay
	if attempt < 32 {
		if scaled := base << attempt; scaled > 0 && scaled < maxDelay {
			backoff = scaled
		}
	}
	return time.Duration(rand.Int64N(int64(backoff)) + 1)
}

// sleep waits for d or until ctx is done.
func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
