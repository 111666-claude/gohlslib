package gohlslib

import (
	"bytes"
	"io"
	"time"

	"github.com/bluenviron/gohlslib/v2/pkg/webvtt"
)

type muxerSegmentWebVTT struct {
	prefix   string
	streamID string
	id       uint64
	startNTP time.Time
	startDTS time.Duration
	endDTS   time.Duration

	path string
	cues []*webvtt.Cue
	// rendered caches the marshaled content; it is produced lazily so that
	// cues arriving after the segment rotation are still included.
	rendered []byte
}

func (s *muxerSegmentWebVTT) initialize() {
	s.path = webvttSegmentPath(s.prefix, s.streamID, s.id)
}

func (s *muxerSegmentWebVTT) close() {
}

func (s *muxerSegmentWebVTT) getPath() string {
	return s.path
}

func (s *muxerSegmentWebVTT) getDuration() time.Duration {
	return s.endDTS - s.startDTS
}

func (s *muxerSegmentWebVTT) getSize() uint64 {
	return uint64(len(s.render()))
}

func (s *muxerSegmentWebVTT) reader() (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(s.render())), nil
}

// appendCue adds a cue to the segment and invalidates the cached content.
func (s *muxerSegmentWebVTT) appendCue(cue *webvtt.Cue) {
	s.cues = append(s.cues, cue)
	s.rendered = nil
}

// render marshals the segment, caching the result.
func (s *muxerSegmentWebVTT) render() []byte {
	if s.rendered != nil {
		return s.rendered
	}

	seg := &webvtt.Segment{
		// identity X-TIMESTAMP-MAP: cue timestamps keep their shifted-DTS value,
		// which the client maps back onto the relative media timeline.
		LocalOffset: 0,
		Cues:        s.cues,
	}

	byts, err := seg.Marshal()
	if err != nil {
		// Marshal only fails on builder errors which cannot occur here.
		return []byte("WEBVTT\n")
	}

	s.rendered = byts
	return s.rendered
}

// finalize implements muxerSegment. Content is rendered lazily by render(),
// so this only records the segment end time; late cues can still be appended
// until the segment is evicted.
func (s *muxerSegmentWebVTT) finalize(endDTS time.Duration) error {
	s.endDTS = endDTS
	return nil
}
