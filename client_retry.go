package gohlslib

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/bluenviron/mediacommon/v2/pkg/limitedreader"
)

// ClientRetryPolicy configures retries for transient failures of segment
// and low-latency part downloads.
//
// The zero value is invalid and disables retries: downloads then behave
// exactly like a single HTTP attempt.
type ClientRetryPolicy struct {
	// MaxAttempts is the maximum number of HTTP attempts performed for a
	// single segment or part download, including the first attempt.
	// A value lower than or equal to zero disables retries.
	MaxAttempts int

	// InitialBackoff is the delay waited before the second attempt.
	// The delay doubles after every failed attempt until MaxBackoff is
	// reached. A value lower than or equal to zero disables retries.
	InitialBackoff time.Duration

	// MaxBackoff caps the exponential backoff. A valid Retry-After header
	// is honored, but it is also capped by this value.
	// If zero, negative or lower than InitialBackoff, it defaults to
	// InitialBackoff.
	MaxBackoff time.Duration
}

// clientRetryConfig is the normalized, read-only retry configuration shared
// by all downloads of a client. A nil pointer means retries are disabled.
type clientRetryConfig struct {
	maxAttempts    int
	initialBackoff time.Duration
	maxBackoff     time.Duration
}

func (p *ClientRetryPolicy) normalize() *clientRetryConfig {
	if p == nil || p.MaxAttempts <= 0 || p.InitialBackoff <= 0 {
		return nil
	}

	maxBackoff := max(p.MaxBackoff, p.InitialBackoff)

	return &clientRetryConfig{
		maxAttempts:    p.MaxAttempts,
		initialBackoff: p.InitialBackoff,
		maxBackoff:     maxBackoff,
	}
}

// isRetriableStatus reports whether an HTTP status code represents a
// transient server condition. 429 (Too Many Requests) and all 5xx codes
// are retriable, while 404 and other 4xx codes are permanent errors.
func isRetriableStatus(status int) bool {
	return status == http.StatusTooManyRequests || status >= 500
}

// isRetriableHTTPError reports whether a transport or body-read error may
// be transient. Context cancellations and oversized bodies are not retried.
func isRetriableHTTPError(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}

	if errors.Is(err, limitedreader.ErrSizeExceeded) {
		return false
	}

	return true
}

func (rc *clientRetryConfig) canRetry(attempt int, status int, err error) bool {
	if rc == nil || attempt >= rc.maxAttempts {
		return false
	}

	if status != 0 {
		return isRetriableStatus(status)
	}

	return isRetriableHTTPError(err)
}

// parseRetryAfter parses a Retry-After header value, which can be either
// a number of delta-seconds or an HTTP-date (RFC 9110, section 10.2.4).
// A date in the past yields a zero delay. Missing, negative or otherwise
// invalid values are not an error: they simply yield ok == false.
func parseRetryAfter(value string, now time.Time) (time.Duration, bool) {
	if value == "" {
		return 0, false
	}

	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		if seconds < 0 {
			return 0, false
		}
		return time.Duration(seconds) * time.Second, true
	}

	date, err := http.ParseTime(value)
	if err != nil {
		return 0, false
	}

	delay := date.Sub(now)
	if delay < 0 {
		return 0, true
	}

	return delay, true
}

// exponentialBackoff returns initialBackoff doubled after every attempt
// already performed, capped at maxBackoff. attempt is 1-based: after the
// first failed attempt, attempt == 1 and the initial backoff is returned.
func (rc *clientRetryConfig) exponentialBackoff(attempt int) time.Duration {
	delay := rc.initialBackoff

	for range attempt - 1 {
		if delay >= rc.maxBackoff {
			return rc.maxBackoff
		}

		next := delay * 2
		if next <= 0 { // integer overflow
			return rc.maxBackoff
		}

		delay = next
	}

	if delay > rc.maxBackoff {
		delay = rc.maxBackoff
	}

	return delay
}

// backoff computes the delay before the next attempt. A valid Retry-After
// header overrides the exponential delay, but is capped at maxBackoff.
func (rc *clientRetryConfig) backoff(attempt int, retryAfter string, now time.Time) time.Duration {
	delay := rc.exponentialBackoff(attempt)

	if retryDelay, ok := parseRetryAfter(retryAfter, now); ok && retryDelay > delay {
		delay = retryDelay
	}

	if delay > rc.maxBackoff {
		delay = rc.maxBackoff
	}

	return delay
}

// sleepWithContext waits for delay to elapse, returning true on timeout
// and false if the context is canceled first. The timer is always stopped
// and, when needed, drained, so neither a timer nor a goroutine is leaked
// when the client is closed while a backoff is pending.
func sleepWithContext(ctx context.Context, delay time.Duration) bool {
	if delay <= 0 {
		return ctx.Err() == nil
	}

	timer := time.NewTimer(delay)
	defer func() {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	}()

	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
