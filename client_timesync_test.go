package gohlslib

import (
	"context"
	"sync"
	"testing"
	"time"
)

const (
	concTestTracks     = 8
	concTestTimepoints = 200
	concTestRounds     = 10
)

// hammerMPEGTSTimeSync has every track goroutine convert the same kind of
// monotonic timestamp stream a shared timeSync instance would see in a real
// multi-track playback. It mirrors the call pattern of
// clientTrackProcessorMPEGTS.process.
func hammerMPEGTSTimeSync(t *testing.T, ts *clientTimeSyncMPEGTS) {
	t.Helper()

	var wg sync.WaitGroup
	errs := make(chan error, concTestTracks)

	for track := 0; track < concTestTracks; track++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			// timestamps run slightly in the past relative to startRTC so that
			// convertAndSync never blocks on the RTC wait
			start := ts.startDTS - int64(concTestTimepoints)*90000

			for i := 0; i < concTestTimepoints; i++ {
				v := start + int64(i)*90000
				_, _, err := ts.convertAndSync(context.Background(), v, v)
				if err != nil {
					select {
					case errs <- err:
					default:
					}
					return
				}
			}
		}()
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestClientTimeSyncMPEGTSConcurrent(t *testing.T) {
	for round := 0; round < concTestRounds; round++ {
		ts := &clientTimeSyncMPEGTS{startDTS: 0}
		ts.initialize()
		hammerMPEGTSTimeSync(t, ts)
	}
}

// TestClientTimeSyncMPEGTSConcurrentConvert reproduces the race between the
// stream processor goroutine (which calls convert directly when recording the
// NTP anchor) and the track processor goroutines (which call convertAndSync).
func TestClientTimeSyncMPEGTSConcurrentConvert(t *testing.T) {
	for round := 0; round < concTestRounds; round++ {
		ts := &clientTimeSyncMPEGTS{startDTS: 0}
		ts.initialize()

		var wg sync.WaitGroup
		stop := make(chan struct{})

		wg.Add(1)
		go func() {
			defer wg.Done()
			// stay in the past relative to startRTC, crossing the 33-bit
			// wraparound several times, like the stream processor does when
			// recording the NTP anchor
			start := ts.startDTS - int64(concTestTracks*concTestTimepoints)*90000
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				ts.convert(start + int64(i%1000)*90000)
			}
		}()

		wg.Add(1)
		go func() {
			defer wg.Done()
			defer close(stop)
			hammerMPEGTSTimeSync(t, ts)
		}()

		wg.Wait()
	}
}

// TestClientTimeSyncMPEGTSWraparound verifies that 33-bit timestamp wraparound
// keeps producing a monotonically increasing shared timeline.
func TestClientTimeSyncMPEGTSWraparound(t *testing.T) {
	ts := &clientTimeSyncMPEGTS{startDTS: 0}
	ts.initialize()

	max33 := int64(0x1FFFFFFFF)
	var prev time.Duration
	for i := int64(0); i < 4*int64(max33); i += 90000 {
		raw := i & max33
		got := ts.convert(raw)
		if got < prev {
			t.Fatalf("timeline went backwards at raw=%d: %v < %v", raw, got, prev)
		}
		prev = got
	}
}

func hammerFMP4TimeSync(t *testing.T, ts *clientTimeSyncFMP4) {
	t.Helper()

	var wg sync.WaitGroup
	errs := make(chan error, concTestTracks)

	for track := 0; track < concTestTracks; track++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			// single time-scale units: 200 points span ~2.2us, so the RTC
			// wait inside convertAndSync never blocks
			for i := uint64(0); i < concTestTimepoints; i++ {
				_, _, err := ts.convertAndSync(context.Background(), 90000, i, 0)
				if err != nil {
					select {
					case errs <- err:
					default:
					}
					return
				}
			}
		}()
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestClientTimeSyncFMP4Concurrent(t *testing.T) {
	for round := 0; round < concTestRounds; round++ {
		ts := &clientTimeSyncFMP4{
			leadingTimeScale: 90000,
			initialBaseTime:  0,
		}
		ts.initialize()
		hammerFMP4TimeSync(t, ts)
	}
}

// TestClientTimeSyncFMP4ConcurrentConvert mirrors the stream processor's
// direct convert call (NTP anchor) racing with track processors.
func TestClientTimeSyncFMP4ConcurrentConvert(t *testing.T) {
	for round := 0; round < concTestRounds; round++ {
		ts := &clientTimeSyncFMP4{
			leadingTimeScale: 90000,
			initialBaseTime:  0,
		}
		ts.initialize()

		var wg sync.WaitGroup
		stop := make(chan struct{})

		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := uint64(0); ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				// stay in the past relative to startRTC
				ts.convert(i%1000, 90000)
			}
		}()

		wg.Add(1)
		go func() {
			defer wg.Done()
			defer close(stop)
			hammerFMP4TimeSync(t, ts)
		}()

		wg.Wait()
	}
}
