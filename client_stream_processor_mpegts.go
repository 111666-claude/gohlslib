package gohlslib

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/bluenviron/mediacommon/v2/pkg/formats/mpegts"
	tscodecs "github.com/bluenviron/mediacommon/v2/pkg/formats/mpegts/codecs"

	"github.com/bluenviron/gohlslib/v2/pkg/codecs"
)

func mpegtsPickLeadingTrack(mpegtsTracks []*mpegts.Track) int {
	// pick first video track
	for i, track := range mpegtsTracks {
		if _, ok := track.Codec.(*tscodecs.H264); ok {
			return i
		}
	}

	// otherwise, pick first track
	return 0
}

func leadingTimeConvMPEGTS(client clientStreamDownloaderClient) *clientTimeConvMPEGTS {
	return client.getLeadingTimeConv().(*clientTimeConvMPEGTS)
}

type switchableReader struct {
	r io.Reader
}

func (r *switchableReader) Read(p []byte) (int, error) {
	return r.r.Read(p)
}

type clientStreamProcessorStreamDownloader interface {
	setTracks(ctx context.Context, tracks []*Track) ([]*clientTrack, bool)
	onProcessorError(ctx context.Context, err error)
}

type pendingSampleMPEGTS struct {
	track  *clientTrack
	rawPTS int64
	rawDTS int64
	data   [][]byte
}

type clientStreamProcessorMPEGTS struct {
	onDecodeError    ClientOnDecodeErrorFunc
	isLeading        bool
	segmentQueue     *clientSegmentQueue
	rp               *clientRoutinePool
	streamDownloader clientStreamProcessorStreamDownloader
	client           clientStreamDownloaderClient

	switchableReader   *switchableReader
	reader             *mpegts.Reader
	trackProcessors    map[*Track]*clientTrackProcessorMPEGTS
	clientStreamTracks []*clientTrack
	curSegment         *segmentData
	leadingTrackFound  bool
	dateTimeProcessed  bool

	// samples received before the first sample of the leading track,
	// kept in their original interleaved order so that no packet is lost.
	pendingSamples []*pendingSampleMPEGTS

	chTrackProcessorDone chan struct{}
}

func (p *clientStreamProcessorMPEGTS) initialize() {
	p.chTrackProcessorDone = make(chan struct{}, clientMaxTracksPerStream)
}

func (p *clientStreamProcessorMPEGTS) run(ctx context.Context) error {
	for {
		seg, ok := p.segmentQueue.pull(ctx)
		if !ok {
			return fmt.Errorf("terminated")
		}

		if seg.err != nil {
			p.streamDownloader.onProcessorError(ctx, seg.err)
			<-ctx.Done()
			return fmt.Errorf("terminated")
		}

		err := p.processSegment(ctx, seg)
		if err != nil {
			return err
		}
	}
}

func (p *clientStreamProcessorMPEGTS) processSegment(ctx context.Context, seg *segmentData) error {
	if p.switchableReader == nil {
		err := p.initializeReader(ctx, seg.payload)
		if err != nil {
			return err
		}
	} else {
		p.switchableReader.r = bytes.NewReader(seg.payload)
	}

	p.curSegment = seg
	p.leadingTrackFound = false
	p.dateTimeProcessed = false
	p.pendingSamples = nil

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

	// deliver samples of non-leading tracks that arrived before the first
	// sample of the leading track and were not consumed while reading
	err := p.flushPendingSamples(ctx)
	if err != nil {
		return err
	}

	return p.joinTrackProcessors(ctx)
}

func (p *clientStreamProcessorMPEGTS) joinTrackProcessors(ctx context.Context) error {
	for _, proc := range p.trackProcessors {
		err := proc.push(ctx, nil)
		if err != nil {
			return err
		}
	}

	for range p.trackProcessors {
		select {
		case <-p.chTrackProcessorDone:
		case <-ctx.Done():
			return nil
		}
	}

	return nil
}

func (p *clientStreamProcessorMPEGTS) onPartProcessorDone(ctx context.Context) {
	select {
	case p.chTrackProcessorDone <- struct{}{}:
	case <-ctx.Done():
	}
}

func (p *clientStreamProcessorMPEGTS) initializeReader(ctx context.Context, firstPayload []byte) error {
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
		case *tscodecs.H264, *tscodecs.MPEG4Audio:
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
	p.clientStreamTracks, ok = p.streamDownloader.setTracks(ctx, tracks)
	if !ok {
		return fmt.Errorf("terminated")
	}

	for i, mpegtsTrack := range supportedTracks {
		track := p.clientStreamTracks[i]
		isLeadingTrack := (i == leadingTrackID)

		processSample := func(rawPTS int64, rawDTS int64, data [][]byte) error {
			if p.trackProcessors == nil {
				if !isLeadingTrack {
					// the leading track has not produced data yet: keep the
					// sample instead of discarding it, it will be delivered
					// once the leading track is available
					p.pendingSamples = append(p.pendingSamples, &pendingSampleMPEGTS{
						track:  track,
						rawPTS: rawPTS,
						rawDTS: rawDTS,
						data:   data,
					})
					return nil
				}

				p.leadingTrackFound = true

				err2 := p.initializeTrackProcessors(ctx, rawDTS)
				if err2 != nil {
					return err2
				}

				// deliver the first sample of the leading track first, so that
				// playback can start immediately, then drain the backlog of
				// samples from the other tracks in its original interleaved order
				err2 = p.processSample(ctx, track, rawPTS, rawDTS, data, true)
				if err2 != nil {
					return err2
				}

				return p.flushPendingSamples(ctx)
			}

			if isLeadingTrack {
				p.leadingTrackFound = true
			}

			return p.processSample(ctx, track, rawPTS, rawDTS, data, isLeadingTrack)
		}

		switch track.track.Codec.(type) {
		case *codecs.H264:
			p.reader.OnDataH264(mpegtsTrack, func(pts int64, dts int64, au [][]byte) error {
				return processSample(pts, dts, au)
			})

		case *codecs.MPEG4Audio:
			p.reader.OnDataMPEG4Audio(mpegtsTrack, func(pts int64, aus [][]byte) error {
				return processSample(pts, pts, aus)
			})
		}
	}

	return nil
}

// flushPendingSamples delivers, in the original interleaved order, all the
// samples that arrived before the first sample of the leading track.
func (p *clientStreamProcessorMPEGTS) flushPendingSamples(ctx context.Context) error {
	for len(p.pendingSamples) != 0 {
		entry := p.pendingSamples[0]
		p.pendingSamples = p.pendingSamples[1:]

		err := p.processSample(ctx, entry.track, entry.rawPTS, entry.rawDTS, entry.data, false)
		if err != nil {
			return err
		}
	}

	return nil
}

func (p *clientStreamProcessorMPEGTS) processSample(
	ctx context.Context,
	track *clientTrack,
	rawPTS int64,
	rawDTS int64,
	data [][]byte,
	isLeadingTrack bool,
) error {
	pts := leadingTimeConvMPEGTS(p.client).convert(rawPTS)
	dts := leadingTimeConvMPEGTS(p.client).convert(rawDTS)

	if !p.dateTimeProcessed && p.isLeading && isLeadingTrack {
		p.dateTimeProcessed = true

		if p.curSegment.dateTime != nil {
			leadingTimeConvMPEGTS(p.client).setNTP(*p.curSegment.dateTime, dts)
		}
		leadingTimeConvMPEGTS(p.client).setLeadingNTPReceived()
	}

	ntp := leadingTimeConvMPEGTS(p.client).getNTP(ctx, dts)

	return p.trackProcessors[track.track].push(ctx, &procEntryMPEGTS{
		pts:  pts,
		dts:  dts,
		ntp:  ntp,
		data: data,
	})
}

func (p *clientStreamProcessorMPEGTS) initializeTrackProcessors(
	ctx context.Context,
	dts int64,
) error {
	if p.isLeading {
		timeConv := &clientTimeConvMPEGTS{
			startDTS: dts,
		}
		timeConv.initialize()

		p.client.setLeadingTimeConv(timeConv)
	} else {
		ok := p.client.waitLeadingTimeConv(ctx)
		if !ok {
			return fmt.Errorf("terminated")
		}

		_, ok = p.client.getLeadingTimeConv().(*clientTimeConvMPEGTS)
		if !ok {
			return fmt.Errorf("stream playlists are mixed MPEGTS/FMP4")
		}
	}

	p.trackProcessors = make(map[*Track]*clientTrackProcessorMPEGTS)

	for _, track := range p.clientStreamTracks {
		proc := &clientTrackProcessorMPEGTS{
			track:           track,
			streamProcessor: p,
		}
		proc.initialize()
		p.rp.add(proc)
		p.trackProcessors[track.track] = proc
	}

	return nil
}
