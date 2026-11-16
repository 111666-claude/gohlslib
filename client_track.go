package gohlslib

import (
	"context"
	"fmt"
	"time"
)

type clientTrack struct {
	track            *Track
	onData           func(pts int64, dts int64, data [][]byte)
	lastAbsoluteTime *time.Time
	startSystem      time.Time

	// catchUp is true while the client is draining a backlog after a
	// target-latency jump. In that state samples are delivered immediately
	// instead of being paced against the system clock.
	catchUp bool
}

func (t *clientTrack) absoluteTime() (time.Time, bool) {
	if t.lastAbsoluteTime == nil {
		return zero, false
	}
	return *t.lastAbsoluteTime, true
}

// setCatchUp enables or disables the catch-up mode. When enabled, the output
// clock is re-anchored on the current media position so that subsequent
// samples follow the new timeline without delivering stale media.
func (t *clientTrack) setCatchUp(enabled bool) {
	t.catchUp = enabled
}

func (t *clientTrack) handleData(
	errGroupCtx context.Context,
	pts int64,
	dts int64,
	ntp *time.Time,
	data [][]byte,
) error {
	// silently discard packets prior to the first packet of the leading track
	if pts < 0 {
		return nil
	}

	dtsDuration := timestampToDuration(dts, t.track.ClockRate)

	// while catching up, deliver samples immediately and keep the output
	// clock anchored to the media timeline
	if t.catchUp {
		t.startSystem = time.Now().Add(-dtsDuration)
		t.lastAbsoluteTime = ntp
		t.onData(pts, dts, data)
		return nil
	}

	// synchronize time
	elapsed := time.Since(t.startSystem)
	if dtsDuration > elapsed {
		diff := dtsDuration - elapsed
		if diff > clientMaxDTSSystemDiff {
			return fmt.Errorf("difference between DTS and system time is too big")
		}

		select {
		case <-time.After(diff):
		case <-errGroupCtx.Done():
			return fmt.Errorf("terminated")
		}
	} else if elapsed-dtsDuration > clientCatchUpReanchorThreshold {
		// the playback position jumped forward (target-latency catch up):
		// re-anchor the system clock so the new timeline is followed without
		// delivering stale samples
		t.startSystem = time.Now().Add(-dtsDuration)
	}

	t.lastAbsoluteTime = ntp
	t.onData(pts, dts, data)
	return nil
}
