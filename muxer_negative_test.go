package gohlslib

import (
	"testing"
	"time"

	"github.com/bluenviron/mediacommon/pkg/codecs/mpeg4audio"
	"github.com/bluenviron/mediacommon/pkg/formats/fmp4"
	"github.com/stretchr/testify/require"

	"github.com/bluenviron/gohlslib/pkg/codecs"
	"github.com/bluenviron/gohlslib/pkg/storage"
)

// testTracksNegative builds the tracks used by negative-timestamp tests.
func testTracksNegative() (*Track, *Track) {
	videoTrack := &Track{
		Codec: &codecs.H264{
			SPS: testSPS,
			PPS: []byte{0x08},
		},
	}

	audioTrack := &Track{
		Codec: &codecs.MPEG4Audio{
			Config: mpeg4audio.Config{
				Type:         2,
				SampleRate:   44100,
				ChannelCount: 2,
			},
		},
	}

	return videoTrack, audioTrack
}

// newTestSegmenter builds an fMP4 segmenter that publishes the first
// finalized segment on the returned channel.
func newTestSegmenter(
	t *testing.T,
	lowLatency bool,
	videoTrack *Track,
	audioTrack *Track,
) (muxerSegmenter, <-chan muxerSegment) {
	t.Helper()

	// a buffered channel holds every published segment so that streams that
	// produce more than one segment (e.g. audio-only) cannot block the
	// segmenter while the test is still writing samples.
	segmentReady := make(chan muxerSegment, 32)

	segmenter := newMuxerSegmenterFMP4(
		lowLatency,
		1*time.Second,
		200*time.Millisecond,
		50*1024*1024,
		videoTrack,
		audioTrack,
		"test",
		storage.NewFactoryRAM(),
		func(seg muxerSegment) error {
			segmentReady <- seg
			return nil
		},
		func(*muxerPart) {},
	)

	return segmenter, segmentReady
}

// decodePublishedSegment waits for the next published segment and decodes
// its fMP4 parts.
func decodePublishedSegment(t *testing.T, ready <-chan muxerSegment) fmp4.Parts {
	t.Helper()

	seg := <-ready

	reader, err := seg.reader()
	require.NoError(t, err)
	defer reader.Close()

	buf := make([]byte, 0)
	for {
		tmp := make([]byte, 1024)
		n, err := reader.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			break
		}
	}

	var parts fmp4.Parts
	err = parts.Unmarshal(buf)
	require.NoError(t, err)

	return parts
}

// lastPartTrack returns the last part track matching the given track ID.
func lastPartTrack(parts fmp4.Parts, trackID int) *fmp4.PartTrack {
	var found *fmp4.PartTrack
	for _, part := range parts {
		for _, partTrack := range part.Tracks {
			if partTrack.ID == trackID {
				found = partTrack
			}
		}
	}
	return found
}

func writeNegativeH26x(t *testing.T, m muxerSegmenter, pts time.Duration, randomAccess bool) {
	t.Helper()

	var au [][]byte
	if randomAccess {
		au = [][]byte{testSPS, {8}, {5}} // SPS, PPS, IDR
	} else {
		au = [][]byte{{1}} // non-IDR
	}

	err := m.writeH26x(testTime.Add(pts), pts, au, randomAccess, false)
	require.NoError(t, err)
}

func writeNegativeAudio(t *testing.T, m muxerSegmenter, pts time.Duration) {
	t.Helper()

	err := m.writeMPEG4Audio(testTime.Add(pts), pts, [][]byte{{
		0x01, 0x02, 0x03, 0x04,
	}})
	require.NoError(t, err)
}

// assertNoUnderflowedBaseTime verifies that every part track carries a
// plausible, non-underflowed BaseMediaDecodeTime and that every track keeps
// a monotonically non-decreasing timeline across the parts of a segment.
func assertNoUnderflowedBaseTime(t *testing.T, parts fmp4.Parts) {
	t.Helper()

	lastBase := make(map[int]uint64)

	for _, part := range parts {
		for _, partTrack := range part.Tracks {
			require.Less(t, partTrack.BaseTime, uint64(1)<<62,
				"track %d has an underflowed (negative) BaseTime", partTrack.ID)

			if prev, ok := lastBase[partTrack.ID]; ok {
				require.GreaterOrEqual(t, partTrack.BaseTime, prev,
					"track %d timeline goes backwards", partTrack.ID)
			}
			lastBase[partTrack.ID] = partTrack.BaseTime
		}
	}
}

func firstPartTrack(parts fmp4.Parts, trackID int) *fmp4.PartTrack {
	for _, part := range parts {
		for _, partTrack := range part.Tracks {
			if partTrack.ID == trackID {
				return partTrack
			}
		}
	}
	return nil
}

// TestMuxerSegmenterNegativeTimestamps reproduces the countdown pre-show
// scenario: video starts at -10s while audio starts earlier, at -11s.
// The first segment must not contain any pre-roll sample and the audio
// BaseMediaDecodeTime must never underflow.
func TestMuxerSegmenterNegativeTimestamps(t *testing.T) {
	for _, ca := range []struct {
		name       string
		lowLatency bool
	}{
		{"fmp4", false},
		{"lowLatency", true},
	} {
		t.Run(ca.name, func(t *testing.T) {
			videoTrack, audioTrack := testTracksNegative()
			segmenter, ready := newTestSegmenter(t, ca.lowLatency, videoTrack, audioTrack)
			defer segmenter.close()

			// video: -10s, -9s, -8s
			writeNegativeH26x(t, segmenter, -10*time.Second, true)
			writeNegativeH26x(t, segmenter, -9*time.Second, false)
			writeNegativeH26x(t, segmenter, -8*time.Second, false)

			// audio arrives first: -11s (pre-roll), -9s, -8s
			writeNegativeAudio(t, segmenter, -11*time.Second)
			writeNegativeAudio(t, segmenter, -9*time.Second)
			writeNegativeAudio(t, segmenter, -8*time.Second)

			// samples that finalize the first segment
			writeNegativeH26x(t, segmenter, 0, true)
			writeNegativeAudio(t, segmenter, 0)

			parts := decodePublishedSegment(t, ready)
			require.NotEmpty(t, parts)
			assertNoUnderflowedBaseTime(t, parts)

			// video timeline is relative to the segment start: the first
			// video BaseTime must be 0 (the segment starts at the first
			// video sample, -10s).
			videoPartTrack := firstPartTrack(parts, 1)
			require.NotNil(t, videoPartTrack)
			require.Equal(t, uint64(0), videoPartTrack.BaseTime)

			// the pre-roll audio sample at -11s is dropped, so the first
			// valid sample is at -9s, i.e. 1s after the segment start,
			// expressed as 44100 units at the 44.1kHz audio time scale.
			audioPartTrack := firstPartTrack(parts, 2)
			require.NotNil(t, audioPartTrack)
			require.Equal(t, uint64(44100), audioPartTrack.BaseTime)
			require.Len(t, audioPartTrack.Samples, 1)
			require.Equal(t, uint32(44100), audioPartTrack.Samples[0].Duration)
		})
	}
}

// TestMuxerSegmenterNegativeVideoOnly verifies that a video-only stream
// starting before the origin produces non-negative BaseTimes.
func TestMuxerSegmenterNegativeVideoOnly(t *testing.T) {
	for _, ca := range []struct {
		name       string
		lowLatency bool
	}{
		{"fmp4", false},
		{"lowLatency", true},
	} {
		t.Run(ca.name, func(t *testing.T) {
			videoTrack, _ := testTracksNegative()
			segmenter, ready := newTestSegmenter(t, ca.lowLatency, videoTrack, nil)
			defer segmenter.close()

			writeNegativeH26x(t, segmenter, -10*time.Second, true)
			writeNegativeH26x(t, segmenter, -9*time.Second, false)
			writeNegativeH26x(t, segmenter, -8*time.Second, false)
			writeNegativeH26x(t, segmenter, 0, true)

			parts := decodePublishedSegment(t, ready)
			require.NotEmpty(t, parts)
			assertNoUnderflowedBaseTime(t, parts)

			videoPartTrack := firstPartTrack(parts, 1)
			require.NotNil(t, videoPartTrack)
			require.Equal(t, uint64(0), videoPartTrack.BaseTime)
		})
	}
}

// TestMuxerSegmenterNegativeAudioOnly verifies that an audio-only stream
// starting before the origin produces non-negative BaseTimes.
func TestMuxerSegmenterNegativeAudioOnly(t *testing.T) {
	for _, ca := range []struct {
		name       string
		lowLatency bool
	}{
		{"fmp4", false},
		{"lowLatency", true},
	} {
		t.Run(ca.name, func(t *testing.T) {
			_, audioTrack := testTracksNegative()
			segmenter, ready := newTestSegmenter(t, ca.lowLatency, nil, audioTrack)
			defer segmenter.close()

			// -11s, -10s create the first segment at -11s, then -9s, -8s, 0
			writeNegativeAudio(t, segmenter, -11*time.Second)
			writeNegativeAudio(t, segmenter, -10*time.Second)
			writeNegativeAudio(t, segmenter, -9*time.Second)
			writeNegativeAudio(t, segmenter, -8*time.Second)
			writeNegativeAudio(t, segmenter, 0)

			parts := decodePublishedSegment(t, ready)
			require.NotEmpty(t, parts)
			assertNoUnderflowedBaseTime(t, parts)

			// audio-only segments start at the first audio sample,
			// so the first BaseTime is 0.
			audioPartTrack := firstPartTrack(parts, 1)
			require.NotNil(t, audioPartTrack)
			require.Equal(t, uint64(0), audioPartTrack.BaseTime)
		})
	}
}

// TestMuxerSegmenterNegativeAudioArrivesLate verifies the case where only
// part of the tracks is available at start: the video track begins first
// with negative timestamps and the audio track only joins later, once the
// timeline is already positive. The late audio must be placed on a
// non-negative, continuous timeline and no stale or negative data may leak
// into the segment.
func TestMuxerSegmenterNegativeAudioArrivesLate(t *testing.T) {
	for _, ca := range []struct {
		name       string
		lowLatency bool
	}{
		{"fmp4", false},
		{"lowLatency", true},
	} {
		t.Run(ca.name, func(t *testing.T) {
			videoTrack, audioTrack := testTracksNegative()
			segmenter, ready := newTestSegmenter(t, ca.lowLatency, videoTrack, audioTrack)
			defer segmenter.close()

			// video starts before the origin, without any audio yet
			writeNegativeH26x(t, segmenter, -10*time.Second, true)
			writeNegativeH26x(t, segmenter, -9*time.Second, false)
			writeNegativeH26x(t, segmenter, -8*time.Second, false)

			// audio joins only after the origin; the non-IDR video frame
			// does not switch segment, so both audio samples end up in the
			// first segment that started at -10s.
			writeNegativeAudio(t, segmenter, 0)
			writeNegativeH26x(t, segmenter, 0, false)
			writeNegativeAudio(t, segmenter, 500*time.Millisecond)

			// an IDR frame far enough in time finalizes the first segment
			writeNegativeH26x(t, segmenter, 2*time.Second, true)

			parts := decodePublishedSegment(t, ready)
			require.NotEmpty(t, parts)
			assertNoUnderflowedBaseTime(t, parts)

			videoPartTrack := firstPartTrack(parts, 1)
			require.NotNil(t, videoPartTrack)
			require.Equal(t, uint64(0), videoPartTrack.BaseTime)

			audioPartTrack := firstPartTrack(parts, 2)
			require.NotNil(t, audioPartTrack)
			// first audio sample is at 0s, i.e. 10s after the segment start
			require.Equal(t, uint64(441000), audioPartTrack.BaseTime)
		})
	}
}

// TestMuxerSegmenterNonNegativeTimestamps verifies that a regular stream
// starting at or after the origin keeps working and that the first
// BaseTime is still 0 (negative timestamps are not simply zeroed).
func TestMuxerSegmenterNonNegativeTimestamps(t *testing.T) {
	for _, ca := range []struct {
		name       string
		lowLatency bool
	}{
		{"fmp4", false},
		{"lowLatency", true},
	} {
		t.Run(ca.name, func(t *testing.T) {
			videoTrack, audioTrack := testTracksNegative()
			segmenter, ready := newTestSegmenter(t, ca.lowLatency, videoTrack, audioTrack)
			defer segmenter.close()

			writeNegativeH26x(t, segmenter, 0, true)
			writeNegativeAudio(t, segmenter, 0)
			writeNegativeH26x(t, segmenter, time.Second, false)
			writeNegativeAudio(t, segmenter, time.Second)
			writeNegativeH26x(t, segmenter, 2*time.Second, true)
			writeNegativeAudio(t, segmenter, 2*time.Second)

			parts := decodePublishedSegment(t, ready)
			require.NotEmpty(t, parts)
			assertNoUnderflowedBaseTime(t, parts)

			// with a non-negative stream starting at 0, both tracks begin
			// exactly at the segment start, so both BaseTimes are 0.
			videoPartTrack := firstPartTrack(parts, 1)
			require.NotNil(t, videoPartTrack)
			require.Equal(t, uint64(0), videoPartTrack.BaseTime)

			audioPartTrack := firstPartTrack(parts, 2)
			require.NotNil(t, audioPartTrack)
			require.Equal(t, uint64(0), audioPartTrack.BaseTime)
		})
	}
}

// TestMuxerSegmenterNegativeTimestampsAcrossSegments verifies that the
// stream-relative timeline keeps increasing across segment boundaries:
// the BaseMediaDecodeTime of the second segment must continue from the end
// of the first one instead of restarting at 0, even when the stream begins
// before the origin.
func TestMuxerSegmenterNegativeTimestampsAcrossSegments(t *testing.T) {
	for _, ca := range []struct {
		name       string
		lowLatency bool
	}{
		{"fmp4", false},
		{"lowLatency", true},
	} {
		t.Run(ca.name, func(t *testing.T) {
			videoTrack, audioTrack := testTracksNegative()
			segmenter, ready := newTestSegmenter(t, ca.lowLatency, videoTrack, audioTrack)
			defer segmenter.close()

			// first segment starts at -2s and is finalized at 0
			writeNegativeH26x(t, segmenter, -2*time.Second, true)
			writeNegativeAudio(t, segmenter, -2*time.Second)
			writeNegativeH26x(t, segmenter, -1*time.Second, false)
			writeNegativeAudio(t, segmenter, -1*time.Second)
			writeNegativeH26x(t, segmenter, 0, true)
			writeNegativeAudio(t, segmenter, 0)

			first := decodePublishedSegment(t, ready)
			require.NotEmpty(t, first)
			assertNoUnderflowedBaseTime(t, first)

			firstVideo := firstPartTrack(first, 1)
			require.NotNil(t, firstVideo)
			// first sample is at -2s, the stream start, so BaseTime is 0
			require.Equal(t, uint64(0), firstVideo.BaseTime)

			firstAudio := firstPartTrack(first, 2)
			require.NotNil(t, firstAudio)
			require.Equal(t, uint64(0), firstAudio.BaseTime)

			// second segment starts at 0 and runs past the 1s target
			writeNegativeH26x(t, segmenter, time.Second, false)
			writeNegativeAudio(t, segmenter, time.Second)
			writeNegativeH26x(t, segmenter, 2*time.Second, true)
			writeNegativeAudio(t, segmenter, 2*time.Second)

			second := decodePublishedSegment(t, ready)
			require.NotEmpty(t, second)
			assertNoUnderflowedBaseTime(t, second)

			secondVideo := firstPartTrack(second, 1)
			require.NotNil(t, secondVideo)
			// the second segment starts 2s after the stream start,
			// i.e. 2 * 90000 units in the 90kHz video time scale.
			require.Equal(t, uint64(180000), secondVideo.BaseTime)

			secondAudio := firstPartTrack(second, 2)
			require.NotNil(t, secondAudio)
			// 2s after the stream start at 44.1kHz
			require.Equal(t, uint64(88200), secondAudio.BaseTime)

			// the whole timeline keeps moving forward
			require.Greater(t, secondVideo.BaseTime, firstVideo.BaseTime)
			require.Greater(t, secondAudio.BaseTime, firstAudio.BaseTime)
		})
	}
}
