package gohlslib

import (
	"time"

	"github.com/bluenviron/gohlslib/v2/pkg/codecs"
)

// Track is a HLS track.
type Track struct {
	// Codec
	Codec codecs.Codec

	// Clock rate
	ClockRate int

	// Name
	// For audio and subtitle renditions only.
	Name string

	// Language
	// For audio and subtitle renditions only.
	Language string

	// whether this is the default track.
	// For audio and subtitle renditions only.
	IsDefault bool
}

// SubtitleCue is a subtitle cue, expressed on the media timeline.
// Start and End are durations relative to the first sample delivered by
// the client, the same origin used by the other OnData* callbacks.
type SubtitleCue struct {
	// Start is the cue start time on the media timeline.
	Start time.Duration
	// End is the cue end time on the media timeline.
	End time.Duration
	// Text is the cue payload. It may span multiple lines.
	Text string
}
