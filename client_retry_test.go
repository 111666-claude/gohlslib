package gohlslib

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/bluenviron/mediacommon/v2/pkg/limitedreader"
	"github.com/stretchr/testify/require"
)

func TestClientRetryPolicyNormalize(t *testing.T) {
	for _, ca := range []struct {
		name string
		in   *ClientRetryPolicy
		out  *clientRetryConfig
	}{
		{
			name: "nil policy disables retries",
			in:   nil,
			out:  nil,
		},
		{
			name: "zero attempts disables retries",
			in:   &ClientRetryPolicy{InitialBackoff: time.Second},
			out:  nil,
		},
		{
			name: "negative attempts disables retries",
			in:   &ClientRetryPolicy{MaxAttempts: -1, InitialBackoff: time.Second},
			out:  nil,
		},
		{
			name: "zero backoff disables retries",
			in:   &ClientRetryPolicy{MaxAttempts: 3},
			out:  nil,
		},
		{
			name: "negative backoff disables retries",
			in:   &ClientRetryPolicy{MaxAttempts: 3, InitialBackoff: -time.Second},
			out:  nil,
		},
		{
			name: "max backoff lower than initial is clamped",
			in: &ClientRetryPolicy{
				MaxAttempts:    3,
				InitialBackoff: 100 * time.Millisecond,
				MaxBackoff:     10 * time.Millisecond,
			},
			out: &clientRetryConfig{
				maxAttempts:    3,
				initialBackoff: 100 * time.Millisecond,
				maxBackoff:     100 * time.Millisecond,
			},
		},
		{
			name: "zero max backoff defaults to initial",
			in: &ClientRetryPolicy{
				MaxAttempts:    2,
				InitialBackoff: 50 * time.Millisecond,
			},
			out: &clientRetryConfig{
				maxAttempts:    2,
				initialBackoff: 50 * time.Millisecond,
				maxBackoff:     50 * time.Millisecond,
			},
		},
		{
			name: "valid policy",
			in: &ClientRetryPolicy{
				MaxAttempts:    5,
				InitialBackoff: 10 * time.Millisecond,
				MaxBackoff:     time.Second,
			},
			out: &clientRetryConfig{
				maxAttempts:    5,
				initialBackoff: 10 * time.Millisecond,
				maxBackoff:     time.Second,
			},
		},
	} {
		t.Run(ca.name, func(t *testing.T) {
			require.Equal(t, ca.out, ca.in.normalize())
		})
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)

	for _, ca := range []struct {
		name  string
		value string
		delay time.Duration
		ok    bool
	}{
		{"missing", "", 0, false},
		{"delta seconds", "5", 5 * time.Second, true},
		{"zero", "0", 0, true},
		{"negative delta", "-2", 0, false},
		{"non numeric", "soon", 0, false},
		{"http date in the future", "Tue, 17 Nov 2026 10:00:03 GMT", 3 * time.Second, true},
		{"http date in the past", "Tue, 17 Nov 2026 09:59:50 GMT", 0, true},
		{"malformed date", "17 Nov 2026 10:00:00", 0, false},
		{"float delta", "1.5", 0, false},
	} {
		t.Run(ca.name, func(t *testing.T) {
			delay, ok := parseRetryAfter(ca.value, now)
			require.Equal(t, ca.ok, ok)
			require.Equal(t, ca.delay, delay)
		})
	}
}

func TestClientRetryConfigBackoff(t *testing.T) {
	rc := (&ClientRetryPolicy{
		MaxAttempts:    10,
		InitialBackoff: 10 * time.Millisecond,
		MaxBackoff:     80 * time.Millisecond,
	}).normalize()

	// exponential growth, then capped.
	require.Equal(t, 10*time.Millisecond, rc.exponentialBackoff(1))
	require.Equal(t, 20*time.Millisecond, rc.exponentialBackoff(2))
	require.Equal(t, 40*time.Millisecond, rc.exponentialBackoff(3))
	require.Equal(t, 80*time.Millisecond, rc.exponentialBackoff(4))
	require.Equal(t, 80*time.Millisecond, rc.exponentialBackoff(9))

	// without a Retry-After header, backoff equals the exponential delay.
	delay := rc.backoff(2, "", time.Now())
	require.Equal(t, 20*time.Millisecond, delay)

	// a Retry-After smaller than the exponential delay is ignored.
	past := time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat)
	delay = rc.backoff(3, past, time.Now())
	require.Equal(t, 40*time.Millisecond, delay)

	// a valid Retry-After overrides the exponential delay, but is capped.
	delay = rc.backoff(1, "5", time.Now())
	require.Equal(t, 80*time.Millisecond, delay)

	// an invalid Retry-After falls back to the exponential delay.
	delay = rc.backoff(2, "not-a-date", time.Now())
	require.Equal(t, 20*time.Millisecond, delay)

	// Retry-After of zero does not extend the exponential delay.
	delay = rc.backoff(2, "0", time.Now())
	require.Equal(t, 20*time.Millisecond, delay)
}

func TestIsRetriableStatus(t *testing.T) {
	require.False(t, isRetriableStatus(http.StatusOK))
	require.False(t, isRetriableStatus(http.StatusNotFound))
	require.False(t, isRetriableStatus(http.StatusBadRequest))
	require.True(t, isRetriableStatus(http.StatusTooManyRequests))
	require.True(t, isRetriableStatus(http.StatusInternalServerError))
	require.True(t, isRetriableStatus(http.StatusBadGateway))
	require.True(t, isRetriableStatus(http.StatusServiceUnavailable))
	require.True(t, isRetriableStatus(http.StatusGatewayTimeout))
	require.True(t, isRetriableStatus(599))
}

func TestIsRetriableHTTPError(t *testing.T) {
	require.False(t, isRetriableHTTPError(context.Canceled))
	require.False(t, isRetriableHTTPError(context.DeadlineExceeded))
	require.False(t, isRetriableHTTPError(limitedreader.ErrSizeExceeded))
	require.True(t, isRetriableHTTPError(errors.New("connection reset by peer")))
	require.True(t, isRetriableHTTPError(errors.New("EOF")))
}

func TestClientRetryConfigCanRetry(t *testing.T) {
	rc := (&ClientRetryPolicy{
		MaxAttempts:    3,
		InitialBackoff: time.Millisecond,
	}).normalize()

	// retries disabled
	require.False(t, (*clientRetryConfig)(nil).canRetry(1, http.StatusServiceUnavailable, nil))

	// budget remaining, transient status
	require.True(t, rc.canRetry(1, http.StatusServiceUnavailable, nil))
	require.True(t, rc.canRetry(2, http.StatusTooManyRequests, nil))

	// budget exhausted: attempt 3 is the last allowed attempt
	require.False(t, rc.canRetry(3, http.StatusServiceUnavailable, nil))

	// permanent status is never retried while the budget remains
	require.False(t, rc.canRetry(1, http.StatusNotFound, nil))

	// transport errors
	require.True(t, rc.canRetry(1, 0, errors.New("connection refused")))
	require.False(t, rc.canRetry(1, 0, context.Canceled))
	require.False(t, rc.canRetry(1, 0, limitedreader.ErrSizeExceeded))
}

func TestSleepWithContext(t *testing.T) {
	t.Run("elapses", func(t *testing.T) {
		start := time.Now()
		require.True(t, sleepWithContext(context.Background(), 10*time.Millisecond))
		require.GreaterOrEqual(t, time.Since(start), 8*time.Millisecond)
	})

	t.Run("non positive delay", func(t *testing.T) {
		require.True(t, sleepWithContext(context.Background(), 0))
	})

	t.Run("canceled while waiting", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		require.False(t, sleepWithContext(ctx, time.Second))
	})

	t.Run("canceled during wait", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(10 * time.Millisecond)
			cancel()
		}()
		start := time.Now()
		require.False(t, sleepWithContext(ctx, 5*time.Second))
		require.Less(t, time.Since(start), time.Second)
	})
}
