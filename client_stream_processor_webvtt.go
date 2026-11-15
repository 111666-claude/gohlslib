package gohlslib

import (
	"context"
	"fmt"

	"golang.org/x/sync/errgroup"

	"github.com/bluenviron/gohlslib/v2/pkg/codecs"
	"github.com/bluenviron/gohlslib/v2/pkg/playlist"
	"github.com/bluenviron/gohlslib/v2/pkg/webvtt"
)

// clientTimeConverter is the subset of the time converters used
// by rendition streams to align their timestamps with the leading stream.
type clientTimeConverter interface {
	convert(v int64, clockRate int) int64
}

type clientStreamProcessorWebVTT struct {
	rendition    *playlist.MultivariantRendition
	segmentQueue *clientSegmentQueue
	errGroup     *errgroup.Group
	parent       clientStreamProcessorParent

	clientStreamTracks []*clientTrack
	timeConv           clientTimeConverter

	// emittedKeys de-duplicates cues repeated across segment boundaries.
	// A set is required (rather than only the previous key) because the
	// same cue can appear in two non-adjacent segments once short cues
	// lying between them are filtered by the playlist window.
	emittedKeys map[string]struct{}
}

func (p *clientStreamProcessorWebVTT) run(errGroupCtx context.Context) error {
	tracks := []*Track{{
		Codec:     &codecs.WebVTT{},
		ClockRate: webvtt.ClockRate,
		Name:      p.rendition.Name,
		Language:  p.rendition.Language,
		IsDefault: p.rendition.Default,
	}}

	var ok bool
	p.clientStreamTracks, ok = p.parent.setTracks(errGroupCtx, tracks)
	if !ok {
		return fmt.Errorf("terminated")
	}

	tmp, ok := p.parent.waitTimeConv(errGroupCtx)
	if !ok {
		return fmt.Errorf("terminated")
	}

	p.timeConv, ok = tmp.(clientTimeConverter)
	if !ok {
		return fmt.Errorf("unsupported time converter for WebVTT stream")
	}

	p.emittedKeys = make(map[string]struct{})

	track := p.clientStreamTracks[0]

	for {
		seg, ok := p.segmentQueue.pull(errGroupCtx)
		if !ok {
			return fmt.Errorf("terminated")
		}

		if seg.err != nil {
			p.parent.onProcessorError(errGroupCtx, seg.err)
			<-errGroupCtx.Done()
			return fmt.Errorf("terminated")
		}

		err := p.processSegment(errGroupCtx, track, seg)
		if err != nil {
			return err
		}
	}
}

func (p *clientStreamProcessorWebVTT) processSegment(
	errGroupCtx context.Context,
	track *clientTrack,
	seg *segmentData,
) error {
	var parsed webvtt.Segment
	err := parsed.Unmarshal(seg.payload)
	if err != nil {
		return fmt.Errorf("unable to decode WebVTT segment: %w", err)
	}

	for _, cue := range parsed.Cues {
		// cues crossing a segment boundary are repeated: emit each cue once.
		key := fmt.Sprintf("%d|%d|%s", cue.Start, cue.End, cue.Text)
		if _, seen := p.emittedKeys[key]; seen {
			continue
		}
		p.emittedKeys[key] = struct{}{}

		// cue timestamps are on the same relative timeline as the leading stream
		start := p.timeConv.convert(cue.Start, webvtt.ClockRate)
		end := p.timeConv.convert(cue.End, webvtt.ClockRate)

		out := &SubtitleCue{
			Start: timestampToDuration(start, webvtt.ClockRate),
			End:   timestampToDuration(end, webvtt.ClockRate),
			Text:  cue.Text,
		}

		err = track.handleWebVTTCue(errGroupCtx, out)
		if err != nil {
			return err
		}
	}

	return nil
}
