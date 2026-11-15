package gohlslib

import (
	"context"
	"fmt"
	"time"
)

type clientTrack struct {
	track            *Track
	onData           func(pts int64, dts int64, data [][]byte)
	onWebVTTCue      func(cue *SubtitleCue)
	lastAbsoluteTime *time.Time
	startSystem      time.Time
}

func (t *clientTrack) absoluteTime() (time.Time, bool) {
	if t.lastAbsoluteTime == nil {
		return zero, false
	}
	return *t.lastAbsoluteTime, true
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

	// synchronize time
	elapsed := time.Since(t.startSystem)
	dtsDuration := timestampToDuration(dts, t.track.ClockRate)
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
	}

	t.lastAbsoluteTime = ntp
	t.onData(pts, dts, data)
	return nil
}

// handleWebVTTCue delivers a subtitle cue, applying the same system-clock
// pacing as regular tracks so cues stay aligned with audio and video.
func (t *clientTrack) handleWebVTTCue(
	errGroupCtx context.Context,
	cue *SubtitleCue,
) error {
	if cue.Start < 0 {
		return nil
	}

	elapsed := time.Since(t.startSystem)
	if cue.Start > elapsed {
		diff := cue.Start - elapsed
		if diff > clientMaxDTSSystemDiff {
			return fmt.Errorf("difference between DTS and system time is too big")
		}

		select {
		case <-time.After(diff):
		case <-errGroupCtx.Done():
			return fmt.Errorf("terminated")
		}
	}

	t.onWebVTTCue(cue)
	return nil
}
