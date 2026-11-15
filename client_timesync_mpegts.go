package gohlslib

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/bluenviron/mediacommon/pkg/formats/mpegts"
)

type clientTimeSyncMPEGTS struct {
	startDTS int64

	startRTC time.Time
	td       *mpegts.TimeDecoder
	// td is stateful (it tracks the previously decoded timestamp in order to
	// handle 33-bit wraparound) and is shared by all the track processors of
	// a stream, plus the stream processor itself when recording the NTP
	// anchor. Serialize every access so concurrent tracks cannot corrupt each
	// other's timestamps.
	mutex sync.Mutex
}

func (ts *clientTimeSyncMPEGTS) initialize() {
	ts.startRTC = time.Now()
	ts.td = mpegts.NewTimeDecoder(ts.startDTS)
}

func (ts *clientTimeSyncMPEGTS) convert(v int64) time.Duration {
	ts.mutex.Lock()
	defer ts.mutex.Unlock()

	return ts.td.Decode(v)
}

func (ts *clientTimeSyncMPEGTS) convertAndSync(
	ctx context.Context,
	rawPTS int64,
	rawDTS int64,
) (time.Duration, time.Duration, error) {
	pts := ts.convert(rawPTS)
	dts := ts.convert(rawDTS)

	elapsed := time.Since(ts.startRTC)
	if dts > elapsed {
		diff := dts - elapsed
		if diff > clientMaxDTSRTCDiff {
			return 0, 0, fmt.Errorf("difference between DTS and RTC is too big")
		}

		select {
		case <-time.After(diff):
		case <-ctx.Done():
			return 0, 0, fmt.Errorf("terminated")
		}
	}

	return pts, dts, nil
}
