package gohlslib

import (
	"fmt"
	"time"

	"github.com/bluenviron/gohlslib/v2/pkg/codecs"
	"github.com/bluenviron/gohlslib/v2/pkg/webvtt"
)

// WriteWebVTT writes a WebVTT subtitle cue.
// start and end are expressed on the global media timeline and must be
// consistent with the timestamps of the audio and video tracks.
//
// The cue is immediately distributed to every already-allocated segment
// it overlaps, including segments that have already been finalized (the
// common live case where video segments are produced before cues arrive)
// and segments starting inside the cue. As a result no cue is lost and
// cues crossing a segment boundary are present on both sides of it.
func (s *muxerSegmenter) writeWebVTT(
	track *muxerTrack,
	_ time.Time,
	start time.Duration,
	end time.Duration,
	text string,
) error {
	if _, ok := track.Codec.(*codecs.WebVTT); !ok {
		return fmt.Errorf("WriteWebVTT called with a non-WebVTT track")
	}

	if end < start {
		return fmt.Errorf("cue end (%v) precedes cue start (%v)", end, start)
	}

	// cues are stored in the same DTS domain as fMP4 segments,
	// which are shifted by fmp4StartDTS to avoid negative BaseTime values.
	cue := &webvtt.Cue{
		Start: (start + fmp4StartDTS).Milliseconds(),
		End:   (end + fmp4StartDTS).Milliseconds(),
		Text:  text,
	}

	return track.stream.addWebVTTCue(cue)
}
