package resync

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"time"

	"github.com/kolsys/opencloud-extensions/common/cs3"
)

// The schedule of the retries: ten attempts, waiting 1, 2, 4, … and then 30
// seconds between them, which rides out an outage of about three minutes.
const (
	retryAttempts = 10
	retryInitial  = time.Second
	retryMax      = 30 * time.Second
	retryFactor   = 2
)

// backoff is the schedule in use; the tests shorten it.
var backoff = schedule{attempts: retryAttempts, initial: retryInitial, max: retryMax}

type schedule struct {
	attempts     int
	initial, max time.Duration
}

// retry runs fn again after a transient error, waiting longer each time,
// until it succeeds, fails for good or the attempts run out. The end of the
// context ends the waiting.
func retry(ctx context.Context, log *slog.Logger, what string, fn func() error) error {
	delay := backoff.initial
	for attempt := 1; ; attempt++ {
		err := fn()
		if err == nil || !transient(err) || attempt >= backoff.attempts {
			return err
		}
		log.Warn("retrying", slog.String("op", what), slog.Int("attempt", attempt), slog.Duration("in", delay), slog.Any("error", err))
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		}
		delay = min(delay*retryFactor, backoff.max)
	}
}

// transient reports whether an error is one a retry may get past: the
// gateway or the bucket away or slow, not a wrong answer.
func transient(err error) bool {
	if cs3.Transient(err) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr)
}
