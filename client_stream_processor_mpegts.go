package gohlslib

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/bluenviron/mediacommon/v2/pkg/formats/mpegts"
	tscodecs "github.com/bluenviron/mediacommon/v2/pkg/formats/mpegts/codecs"
	"golang.org/x/sync/errgroup"

	"github.com/bluenviron/gohlslib/v2/pkg/codecs"
)

func mpegtsPickLeadingTrack(mpegtsTracks []*mpegts.Track) int {
	// pick first video track
	for i, track := range mpegtsTracks {
		if _, ok := track.Codec.(*tscodecs.H264); ok {
			return i
		}
	}

	// pick first non-KLV track
	for i, track := range mpegtsTracks {
		if _, ok := track.Codec.(*tscodecs.KLV); !ok {
			return i
		}
	}

	// otherwise, pick first track
	return 0
}

type switchableReader struct {
	r io.Reader
}

func (r *switchableReader) Read(p []byte) (int, error) {
	return r.r.Read(p)
}

type clientStreamProcessorMPEGTS struct {
	onDecodeError ClientOnDecodeErrorFunc
	onDataID3     ClientOnDataID3Func
	isLeading     bool
	segmentQueue  *clientSegmentQueue
	errGroup      *errgroup.Group
	parent        clientStreamProcessorParent

	switchableReader  *switchableReader
	reader            *mpegts.Reader
	trackProcessors   map[*Track]*clientTrackProcessorMPEGTS
	curSegment        *segmentData
	leadingTrackFound bool
	dateTimeProcessed bool
	streamTracks      []*clientTrack
	timeConv          *clientTimeConvMPEGTS
	queuedSamples     []func() error

	id3Extractor *clientID3ExtractorMPEGTS
	id3Track     *clientTrack
	id3Proc      *clientTrackProcessorMPEGTS
	id3Seen      map[id3Event]struct{}
	id3SeenOrder []id3Event

	chTrackProcessorDone chan struct{}
}

func (p *clientStreamProcessorMPEGTS) initialize() {
	p.chTrackProcessorDone = make(chan struct{}, clientMaxTracksPerStream)
	p.id3Extractor = &clientID3ExtractorMPEGTS{
		onDecodeError: p.onDecodeError,
	}
	p.id3Extractor.initialize()
	p.id3Seen = make(map[id3Event]struct{})
}

func (p *clientStreamProcessorMPEGTS) run(errGroupCtx context.Context) error {
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

		err := p.processSegment(errGroupCtx, seg)
		if err != nil {
			return err
		}
	}
}

func (p *clientStreamProcessorMPEGTS) processSegment(errGroupCtx context.Context, seg *segmentData) error {
	if p.switchableReader == nil {
		err := p.initializeReader(errGroupCtx, seg.payload)
		if err != nil {
			return err
		}
	} else {
		p.switchableReader.r = bytes.NewReader(seg.payload)
	}

	p.curSegment = seg
	p.leadingTrackFound = false
	p.dateTimeProcessed = false

	for {
		err := p.reader.Read()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return err
		}
	}

	if !p.leadingTrackFound {
		return fmt.Errorf("could not find data of leading track")
	}

	err := p.processID3(errGroupCtx, seg)
	if err != nil {
		return err
	}

	return p.joinTrackProcessors(errGroupCtx)
}

// processID3 extracts ID3 timed metadata from the segment payload
// and dispatches events in PTS order, skipping already-seen ones.
func (p *clientStreamProcessorMPEGTS) processID3(errGroupCtx context.Context, seg *segmentData) error {
	events := p.id3Extractor.processSegment(seg.payload)
	if len(events) == 0 {
		return nil
	}

	// dispatch events in PTS order, taking the 33-bit PTS wrap into account
	sortID3EventsByPTS(events)

	for _, ev := range events {
		// do not dispatch events that have already been seen,
		// for instance after a playlist refresh or a reconnect
		if _, ok := p.id3Seen[ev]; ok {
			continue
		}
		p.id3Seen[ev] = struct{}{}
		p.id3SeenOrder = append(p.id3SeenOrder, ev)
		if len(p.id3SeenOrder) > clientMaxID3SeenEvents {
			delete(p.id3Seen, p.id3SeenOrder[0])
			p.id3SeenOrder = p.id3SeenOrder[1:]
		}

		if p.id3Track.startSystem.IsZero() {
			p.id3Track.startSystem = p.timeConv.startSystem
		}

		err := p.processSample(errGroupCtx, false, p.id3Proc, ev.pts, ev.pts,
			[][]byte{[]byte(ev.title), []byte(ev.artist)})
		if err != nil {
			return err
		}
	}

	return nil
}

func (p *clientStreamProcessorMPEGTS) joinTrackProcessors(errGroupCtx context.Context) error {
	for _, proc := range p.trackProcessors {
		err := proc.push(errGroupCtx, nil)
		if err != nil {
			return err
		}
	}

	for range p.trackProcessors {
		select {
		case <-p.chTrackProcessorDone:
		case <-errGroupCtx.Done():
			return nil
		}
	}

	return nil
}

func (p *clientStreamProcessorMPEGTS) onPartProcessorDone(errGroupCtx context.Context) {
	select {
	case p.chTrackProcessorDone <- struct{}{}:
	case <-errGroupCtx.Done():
	}
}

func (p *clientStreamProcessorMPEGTS) initializeReader(errGroupCtx context.Context, firstPayload []byte) error {
	p.switchableReader = &switchableReader{bytes.NewReader(firstPayload)}

	p.reader = &mpegts.Reader{R: p.switchableReader}
	err := p.reader.Initialize()
	if err != nil {
		return err
	}

	p.reader.OnDecodeError(func(err error) {
		p.onDecodeError(err)
	})

	var supportedTracks []*mpegts.Track

	for _, track := range p.reader.Tracks() {
		switch track.Codec.(type) {
		case *tscodecs.H264, *tscodecs.MPEG4Audio, *tscodecs.KLV:
			supportedTracks = append(supportedTracks, track)
		}
	}

	if len(supportedTracks) == 0 {
		return fmt.Errorf("no supported tracks found")
	}

	leadingTrackID := mpegtsPickLeadingTrack(supportedTracks)

	tracks := make([]*Track, len(supportedTracks))

	for i, mpegtsTrack := range supportedTracks {
		tracks[i] = &Track{
			Codec:     fromMPEGTS(mpegtsTrack.Codec),
			ClockRate: 90000,
		}
	}

	if len(tracks) > clientMaxTracksPerStream {
		return fmt.Errorf("too many tracks per stream")
	}

	var ok bool
	p.streamTracks, ok = p.parent.setTracks(errGroupCtx, tracks)
	if !ok {
		return fmt.Errorf("terminated")
	}

	err = p.initializeTrackProcessors(errGroupCtx)
	if err != nil {
		return err
	}

	for i, mpegtsTrack := range supportedTracks {
		track := p.streamTracks[i]
		isLeadingTrack := (i == leadingTrackID)
		trackProc := p.trackProcessors[track.track]

		switch track.track.Codec.(type) {
		case *codecs.H264:
			p.reader.OnDataH264(mpegtsTrack, func(pts int64, dts int64, au [][]byte) error {
				return p.processSample(errGroupCtx, isLeadingTrack, trackProc, pts, dts, au)
			})

		case *codecs.MPEG4Audio:
			p.reader.OnDataMPEG4Audio(mpegtsTrack, func(pts int64, aus [][]byte) error {
				return p.processSample(errGroupCtx, isLeadingTrack, trackProc, pts, pts, aus)
			})

		case *codecs.KLV:
			p.reader.OnDataKLV(mpegtsTrack, func(pts int64, data []byte) error {
				return p.processSample(errGroupCtx, isLeadingTrack, trackProc, pts, pts, [][]byte{data})
			})
		}
	}

	return nil
}

func (p *clientStreamProcessorMPEGTS) processSample(
	errGroupCtx context.Context,
	isLeadingTrack bool,
	trackProc *clientTrackProcessorMPEGTS,
	rawPTS int64,
	rawDTS int64,
	data [][]byte,
) error {
	if isLeadingTrack {
		p.leadingTrackFound = true
	}

	if p.timeConv == nil {
		if isLeadingTrack {
			err := p.initializeTimeConv(errGroupCtx, rawDTS)
			if err != nil {
				return err
			}

			err = p.processSample2(errGroupCtx, isLeadingTrack, trackProc, rawPTS, rawDTS, data)
			if err != nil {
				return err
			}

			for _, call := range p.queuedSamples {
				err = call()
				if err != nil {
					return err
				}
			}
			p.queuedSamples = nil

			return nil
		}

		if len(p.queuedSamples) >= clientMaxMPEGTSQueuedSamples {
			return fmt.Errorf("too many queued samples")
		}

		p.queuedSamples = append(p.queuedSamples, func() error {
			return p.processSample2(errGroupCtx, isLeadingTrack, trackProc, rawPTS, rawDTS, data)
		})
		return nil
	}

	return p.processSample2(errGroupCtx, isLeadingTrack, trackProc, rawPTS, rawDTS, data)
}

func (p *clientStreamProcessorMPEGTS) processSample2(
	errGroupCtx context.Context,
	isLeadingTrack bool,
	trackProc *clientTrackProcessorMPEGTS,
	rawPTS int64,
	rawDTS int64,
	data [][]byte,
) error {
	pts := p.timeConv.convert(rawPTS)
	dts := p.timeConv.convert(rawDTS)

	if !p.dateTimeProcessed && p.isLeading && isLeadingTrack {
		p.dateTimeProcessed = true

		if p.curSegment.dateTime != nil {
			p.timeConv.setNTP(*p.curSegment.dateTime, dts)
		}
		p.timeConv.setLeadingNTPReceived()
	}

	ntp := p.timeConv.getNTP(errGroupCtx, dts)

	return trackProc.push(errGroupCtx, &procEntryMPEGTS{
		pts:  pts,
		dts:  dts,
		ntp:  ntp,
		data: data,
	})
}

func (p *clientStreamProcessorMPEGTS) initializeTrackProcessors(errGroupCtx context.Context) error {
	p.trackProcessors = make(map[*Track]*clientTrackProcessorMPEGTS)

	for _, track := range p.streamTracks {
		trackProc := &clientTrackProcessorMPEGTS{
			track:           track,
			streamProcessor: p,
		}
		trackProc.initialize()

		p.errGroup.Go(func() error {
			return trackProc.run(errGroupCtx)
		})

		p.trackProcessors[track.track] = trackProc
	}

	// allocate a pseudo-track that paces ID3 events like media samples,
	// so that metadata callbacks fire in sync with playback
	p.id3Track = &clientTrack{
		track: &Track{
			ClockRate: 90000,
		},
		onData: func(pts int64, _ int64, data [][]byte) {
			p.onDataID3(pts, &ID3Metadata{
				Title:  string(data[0]),
				Artist: string(data[1]),
			})
		},
	}

	p.id3Proc = &clientTrackProcessorMPEGTS{
		track:           p.id3Track,
		streamProcessor: p,
	}
	p.id3Proc.initialize()

	p.errGroup.Go(func() error {
		return p.id3Proc.run(errGroupCtx)
	})

	p.trackProcessors[p.id3Track.track] = p.id3Proc

	return nil
}

func (p *clientStreamProcessorMPEGTS) initializeTimeConv(errGroupCtx context.Context, startDTS int64) error {
	if p.isLeading {
		p.timeConv = &clientTimeConvMPEGTS{
			startDTS: startDTS,
		}
		p.timeConv.initialize()

		p.parent.setTimeConv(p.timeConv)
	} else {
		tmp, ok := p.parent.waitTimeConv(errGroupCtx)
		if !ok {
			return fmt.Errorf("terminated")
		}

		p.timeConv, ok = tmp.(*clientTimeConvMPEGTS)
		if !ok {
			return fmt.Errorf("stream playlists are mixed MPEGTS/FMP4")
		}
	}

	return nil
}
