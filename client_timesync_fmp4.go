package gohlslib

import (
	"context"
	"fmt"
	"sync"
	"time"
)

func durationGoToMp4(v time.Duration, timeScale uint32) uint64 {
	timeScale64 := uint64(timeScale)
	secs := v / time.Second
	dec := v % time.Second
	return uint64(secs)*timeScale64 + uint64(dec)*timeScale64/uint64(time.Second)
}

func durationMp4ToGo(v uint64, timeScale uint32) time.Duration {
	timeScale64 := uint64(timeScale)
	secs := v / timeScale64
	dec := v % timeScale64
	return time.Duration(secs)*time.Second + time.Duration(dec)*time.Second/time.Duration(timeScale64)
}

type clientTimeSyncFMP4 struct {
	leadingTimeScale uint32
	initialBaseTime  uint64

	startRTC time.Time
	startDTS time.Duration

	// the time sync is shared by all the track processors of a stream and the
	// stream processor itself (NTP anchor). The conversion is currently
	// stateless, but serialize accesses anyway so concurrent readers cannot
	// observe a partially updated state and the two implementations expose the
	// same concurrency contract.
	mutex sync.Mutex
}

func (ts *clientTimeSyncFMP4) initialize() {
	ts.startRTC = time.Now()
	ts.startDTS = durationMp4ToGo(ts.initialBaseTime, ts.leadingTimeScale)
}

func (ts *clientTimeSyncFMP4) convert(v uint64, timeScale uint32) time.Duration {
	ts.mutex.Lock()
	defer ts.mutex.Unlock()

	return durationMp4ToGo(v, timeScale) - ts.startDTS
}

func (ts *clientTimeSyncFMP4) convertAndSync(
	ctx context.Context,
	timeScale uint32,
	rawDTS uint64,
	ptsOffset int32,
) (time.Duration, time.Duration, error) {
	pts := ts.convert(rawDTS+uint64(ptsOffset), timeScale)
	dts := ts.convert(rawDTS, timeScale)

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
