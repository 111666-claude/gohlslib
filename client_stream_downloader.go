package gohlslib

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/bluenviron/mediacommon/v2/pkg/limitedreader"
	"golang.org/x/sync/errgroup"

	"github.com/bluenviron/gohlslib/v2/pkg/playlist"
)

var errLowLatencyUnavailable = errors.New("low-latency tags are no longer available")

func findSegmentWithInvPosition(segments []*playlist.MediaSegment, invPos int) (*playlist.MediaSegment, int) {
	index := len(segments) - invPos
	if index < 0 {
		return nil, 0
	}

	return segments[index], index
}

func findSegmentWithID(seqNo int, segments []*playlist.MediaSegment, id int) (*playlist.MediaSegment, int, int) {
	index := int(int64(id) - int64(seqNo))
	if index < 0 || index >= len(segments) {
		return nil, 0, 0
	}

	return segments[index], index, len(segments) - index
}

// segmentByteRangeStart returns the start of the byte range of the segment at position pos.
// When the offset is not present, the byte range begins at the next byte
// following the byte range of the previous segment.
func segmentByteRangeStart(segments []*playlist.MediaSegment, pos int) uint64 {
	var start uint64

	for segments[pos].ByteRangeStart == nil && pos > 0 && segments[pos-1].ByteRangeLength != nil {
		pos--
		start += *segments[pos].ByteRangeLength
	}

	if segments[pos].ByteRangeStart != nil {
		start += *segments[pos].ByteRangeStart
	}

	return start
}

func dateTimeOfPreloadHint(pl *playlist.Media) *time.Time {
	if len(pl.Segments) == 0 {
		return nil
	}

	lastSeg := pl.Segments[len(pl.Segments)-1]
	if lastSeg.DateTime == nil {
		return nil
	}

	d := lastSeg.DateTime.Add(lastSeg.Duration)

	for _, part := range pl.Parts {
		d = d.Add(part.Duration)
	}

	return &d
}

type clientStreamDownloaderParent interface {
	onProcessorError(errGroupCtx context.Context, err error)
	setTimeConv(ts clientTimeConv)
	waitTimeConv(errGroupCtx context.Context) (clientTimeConv, bool)
}

type clientStreamDownloader struct {
	isLeading                bool
	startDistance            int
	maxDistance              int
	targetLatency            time.Duration
	httpClient               *http.Client
	onRequest                ClientOnRequestFunc
	onDownloadStreamPlaylist ClientOnDownloadStreamPlaylistFunc
	onDownloadSegment        ClientOnDownloadSegmentFunc
	onDownloadPart           ClientOnDownloadPartFunc
	onDecodeError            ClientOnDecodeErrorFunc
	playlistURL              *url.URL
	rendition                *playlist.MultivariantRendition
	firstPlaylist            *playlist.Media
	errGroup                 *errgroup.Group
	parent                   clientStreamDownloaderParent

	segmentQueue *clientSegmentQueue
	curSegmentID *int

	// lastPreloadURI is the URI of the media last delivered through the
	// preload hint. It is used when falling back from low-latency to the
	// real-time loop, so the same media is not delivered a second time.
	lastPreloadURI     string
	skipPreloadSegment bool

	// out
	chTracks chan []*Track

	// in
	chStartStreaming chan map[*Track]*clientTrack
}

func (d *clientStreamDownloader) initialize() {
	d.chTracks = make(chan []*Track)
	d.chStartStreaming = make(chan map[*Track]*clientTrack)
}

func (d *clientStreamDownloader) run(errGroupCtx context.Context) error {
	if d.firstPlaylist == nil {
		var err error
		d.firstPlaylist, err = d.downloadPlaylist(errGroupCtx, false)
		if err != nil {
			return err
		}
	}

	d.segmentQueue = &clientSegmentQueue{}
	d.segmentQueue.initialize()

	if d.firstPlaylist.Map != nil && d.firstPlaylist.Map.URI != "" {
		initFile, err := d.downloadSegment(
			errGroupCtx,
			d.firstPlaylist.Map.URI,
			d.firstPlaylist.Map.ByteRangeStart,
			d.firstPlaylist.Map.ByteRangeLength)
		if err != nil {
			return err
		}

		proc := &clientStreamProcessorFMP4{
			errGroupCtx:  errGroupCtx,
			isLeading:    d.isLeading,
			rendition:    d.rendition,
			initFile:     initFile,
			segmentQueue: d.segmentQueue,
			errGroup:     d.errGroup,
			parent:       d,
		}
		proc.initialize()

		d.errGroup.Go(func() error {
			return proc.run(errGroupCtx)
		})
	} else {
		proc := &clientStreamProcessorMPEGTS{
			onDecodeError: d.onDecodeError,
			isLeading:     d.isLeading,
			segmentQueue:  d.segmentQueue,
			errGroup:      d.errGroup,
			parent:        d,
		}
		proc.initialize()

		d.errGroup.Go(func() error {
			return proc.run(errGroupCtx)
		})
	}

	var err error

	if d.firstPlaylist.ServerControl != nil &&
		d.firstPlaylist.ServerControl.CanBlockReload &&
		d.firstPlaylist.PreloadHint != nil {
		err = d.runLowLatency(errGroupCtx)
		// a playlist that lost the low-latency tags is handled as a
		// real-time (traditional) playlist fallback
		if errors.Is(err, errLowLatencyUnavailable) {
			err = d.runTraditional(errGroupCtx)
		}
	} else {
		err = d.runTraditional(errGroupCtx)
	}

	d.segmentQueue.push(&segmentData{
		err: err,
	})
	<-errGroupCtx.Done()

	return fmt.Errorf("terminated")
}

func (d *clientStreamDownloader) runLowLatency(errGroupCtx context.Context) error {
	pl := d.firstPlaylist

	for {
		byts, err := d.downloadPreloadHint(errGroupCtx, pl.PreloadHint)
		if err != nil {
			return err
		}

		d.lastPreloadURI = pl.PreloadHint.URI

		d.segmentQueue.push(&segmentData{
			dateTime: dateTimeOfPreloadHint(pl),
			payload:  byts,
		})

		pl, err = d.downloadPlaylist(errGroupCtx, d.firstPlaylist.ServerControl.CanSkipUntil != nil)
		if err != nil {
			return err
		}

		// the server stopped providing low-latency tags: fall back to the
		// real-time playlist loop. The cursor is anchored on the last segment
		// in the list; the segment delivered through the preload hint is
		// skipped on the first traditional iteration so media is not delivered
		// twice.
		if pl.ServerControl == nil ||
			!pl.ServerControl.CanBlockReload ||
			pl.PreloadHint == nil {
			anchorPos := len(pl.Segments) - 1
			d.curSegmentID = new(pl.MediaSequence + anchorPos)
			// the segment delivered through the preload hint must be skipped by
			// the real-time loop to avoid delivering it twice
			d.skipPreloadSegment = true
			return errLowLatencyUnavailable
		}
	}
}

func (d *clientStreamDownloader) runTraditional(errGroupCtx context.Context) error {
	pl := d.firstPlaylist

	if d.targetLatency != 0 {
		return d.runTraditionalWithTarget(errGroupCtx, pl)
	}

	for {
		seg, payload, err := d.downloadNextSegment(errGroupCtx, pl)
		if err != nil {
			return err
		}

		d.segmentQueue.push(&segmentData{
			dateTime: seg.DateTime,
			payload:  payload,
		})

		ok := d.segmentQueue.waitUntilPendingIsBelow(errGroupCtx, 1)
		if !ok {
			return fmt.Errorf("terminated")
		}

		pl, err = d.downloadPlaylist(errGroupCtx, false)
		if err != nil {
			return err
		}
	}
}

// runTraditionalWithTarget drives the playlist with a target latency.
//
// The downloader keeps the number of downloaded-but-unprocessed segments
// bounded by the target latency, so the processing pipeline is never flooded
// while still allowing a small backlog. After every segment the playlist is
// reloaded: when the playback position has fallen behind the target, the
// cursor jumps forward, the stale backlog is discarded (never delivered
// twice) and the jump segment is processed immediately so the output clock is
// re-anchored and the client converges back to the target. Discontinuities are
// never crossed.
func (d *clientStreamDownloader) runTraditionalWithTarget(
	errGroupCtx context.Context,
	pl *playlist.Media,
) error {
	maxPending := d.maxTargetQueueSize(pl)

	for {
		seg, segPos, jumped, err := d.downloadNextSegmentWithTarget(pl)
		if err != nil {
			if errors.Is(err, ErrClientEOS) {
				return err
			}

			// the next segment is not available yet (the playlist is frozen,
			// for example during a short network stall): wait for the next
			// reload window and keep polling instead of terminating, so
			// playback can resume and catch up once new media is published
			select {
			case <-time.After(d.playlistReloadInterval(pl)):
			case <-errGroupCtx.Done():
				return fmt.Errorf("terminated")
			}

			pl, err = d.downloadPlaylist(errGroupCtx, false)
			if err != nil {
				return err
			}
			continue
		}

		if jumped {
			// every segment still queued for playback has been superseded.
			// The segment currently being processed is left untouched so the
			// output timeline stays contiguous.
			d.segmentQueue.discardQueued()
		}

		// when coming from a low-latency fallback, the media already delivered
		// through the preload hint is skipped without being downloaded again
		if d.skipPreloadSegment && seg.URI == d.lastPreloadURI {
			d.skipPreloadSegment = false
			d.curSegmentID = new(pl.MediaSequence + segPos)
			pl, err = d.downloadPlaylist(errGroupCtx, false)
			if err != nil {
				return err
			}
			continue
		}
		d.skipPreloadSegment = false

		payload, err := d.downloadSegment(errGroupCtx, seg.URI,
			new(segmentByteRangeStart(pl.Segments, segPos)), seg.ByteRangeLength)
		if err != nil {
			return err
		}

		d.curSegmentID = new(pl.MediaSequence + segPos)

		d.segmentQueue.push(&segmentData{
			dateTime: seg.DateTime,
			payload:  payload,
			catchUp:  jumped,
		})

		// Bound the backlog by the target latency. A catch-up segment is
		// processed without real-time pacing, so the backlog drains promptly
		// and the downloader can resume reloading the playlist.
		ok := d.segmentQueue.waitUntilPendingIsBelow(errGroupCtx, maxPending)
		if !ok {
			return fmt.Errorf("terminated")
		}

		pl, err = d.downloadPlaylist(errGroupCtx, false)
		if err != nil {
			return err
		}
	}
}

// maxTargetQueueSize returns the maximum number of downloaded but not yet
// fully processed segments, derived from the target latency.
func (d *clientStreamDownloader) maxTargetQueueSize(pl *playlist.Media) int {
	segmentDuration := time.Duration(pl.TargetDuration) * time.Second

	size := int(d.targetLatency/segmentDuration) + 1

	return max(size, 2)
}

// playlistReloadInterval returns the time to wait before reloading a playlist
// that does not yet contain the next segment. It follows the target duration
// advertised by the playlist, capped to a reasonable range.
func (d *clientStreamDownloader) playlistReloadInterval(pl *playlist.Media) time.Duration {
	interval := time.Duration(pl.TargetDuration) * time.Second
	interval = min(interval, time.Second)
	if interval <= 0 {
		interval = time.Second
	}
	return interval
}

func (d *clientStreamDownloader) downloadPlaylist(
	errGroupCtx context.Context,
	skipUntil bool,
) (*playlist.Media, error) {
	ur := d.playlistURL

	if skipUntil {
		newUR := cloneURL(ur)
		q := newUR.Query()
		q.Add("_HLS_skip", "YES")
		newUR.RawQuery = q.Encode()
		ur = newUR
	}

	d.onDownloadStreamPlaylist(ur.String())

	finalURL, pl, err := downloadPlaylist(errGroupCtx, d.httpClient, d.onRequest, ur)
	if err != nil {
		return nil, err
	}

	if skipUntil {
		q := finalURL.Query()
		q.Del("_HLS_skip")
		finalURL.RawQuery = q.Encode()
	}

	d.playlistURL = finalURL

	plt, ok := pl.(*playlist.Media)
	if !ok {
		return nil, fmt.Errorf("invalid playlist")
	}

	return plt, nil
}

func (d *clientStreamDownloader) downloadPreloadHint(
	errGroupCtx context.Context,
	preloadHint *playlist.MediaPreloadHint,
) ([]byte, error) {
	u, err := clientAbsoluteURL(d.playlistURL, preloadHint.URI)
	if err != nil {
		return nil, err
	}

	d.onDownloadPart(u.String())

	req, err := http.NewRequestWithContext(errGroupCtx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}

	if preloadHint.ByteRangeLength != nil {
		req.Header.Add("Range", "bytes="+strconv.FormatUint(preloadHint.ByteRangeStart, 10)+
			"-"+strconv.FormatUint(preloadHint.ByteRangeStart+*preloadHint.ByteRangeLength-1, 10))
	}

	d.onRequest(req)

	res, err := d.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusPartialContent {
		return nil, fmt.Errorf("bad status code: %d", res.StatusCode)
	}

	byts, err := io.ReadAll(&limitedreader.Reader{R: res.Body, N: clientMaxInboundPartSize})
	if err != nil {
		return nil, err
	}

	return byts, nil
}

func (d *clientStreamDownloader) downloadSegment(
	errGroupCtx context.Context,
	uri string,
	start *uint64,
	length *uint64,
) ([]byte, error) {
	u, err := clientAbsoluteURL(d.playlistURL, uri)
	if err != nil {
		return nil, err
	}

	d.onDownloadSegment(u.String())

	req, err := http.NewRequestWithContext(errGroupCtx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}

	if length != nil {
		if start == nil {
			start = new(uint64(0))
		}
		req.Header.Add("Range", "bytes="+strconv.FormatUint(*start, 10)+
			"-"+strconv.FormatUint(*start+*length-1, 10))
	}

	d.onRequest(req)

	res, err := d.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusPartialContent {
		return nil, fmt.Errorf("bad status code: %d", res.StatusCode)
	}

	byts, err := io.ReadAll(&limitedreader.Reader{R: res.Body, N: clientMaxInboundSegmentSize})
	if err != nil {
		return nil, err
	}

	return byts, nil
}

func (d *clientStreamDownloader) downloadNextSegment(
	errGroupCtx context.Context,
	pl *playlist.Media,
) (*playlist.MediaSegment, []byte, error) {
	var seg *playlist.MediaSegment
	var segPos int

	isVOD := (d.firstPlaylist.PlaylistType != nil &&
		*d.firstPlaylist.PlaylistType == playlist.MediaPlaylistTypeVOD) || d.firstPlaylist.Endlist

	if d.curSegmentID == nil {
		if isVOD {
			// VOD stream: start from the beginning
			if len(pl.Segments) == 0 {
				return nil, nil, fmt.Errorf("no segments found")
			}
			seg = pl.Segments[0]
		} else {
			// live stream: start from clientLiveInitialDistance
			seg, segPos = findSegmentWithInvPosition(pl.Segments, d.startDistance)
			if seg == nil {
				return nil, nil, fmt.Errorf("there aren't enough segments to fill the buffer")
			}
		}
	} else {
		var invPos int
		seg, segPos, invPos = findSegmentWithID(pl.MediaSequence, pl.Segments, *d.curSegmentID+1)
		if seg == nil {
			if pl.Endlist {
				return nil, nil, ErrClientEOS
			}
			return nil, nil, fmt.Errorf("next segment not found or not ready yet")
		}

		if !pl.Endlist && invPos > d.maxDistance {
			return nil, nil, fmt.Errorf("playback is too late")
		}
	}

	d.curSegmentID = new(pl.MediaSequence + segPos)

	byts, err := d.downloadSegment(errGroupCtx, seg.URI,
		new(segmentByteRangeStart(pl.Segments, segPos)), seg.ByteRangeLength)
	if err != nil {
		return nil, nil, err
	}

	return seg, byts, nil
}

// downloadNextSegmentWithTarget implements the target-latency policy: the
// first segment is chosen so that its edge distance matches the target, while
// later iterations keep playing sequentially unless the distance grows beyond
// the target by more than the current segment duration. In that case the
// cursor jumps forward to the segment closest to the target, without ever
// crossing a discontinuity or requesting a segment twice; the returned
// jumped flag is true when such a jump happened.
func (d *clientStreamDownloader) downloadNextSegmentWithTarget(
	pl *playlist.Media,
) (*playlist.MediaSegment, int, bool, error) {
	if len(pl.Segments) == 0 {
		return nil, 0, false, fmt.Errorf("no segments found")
	}

	if d.curSegmentID == nil {
		segPos := initialSegmentForTarget(pl, d.targetLatency)
		return pl.Segments[segPos], segPos, false, nil
	}

	curSeg, curPos, _ := findSegmentWithID(pl.MediaSequence, pl.Segments, *d.curSegmentID)
	if curSeg == nil {
		// the segment being played has been removed from the playlist:
		// restart from the safe boundary instead of failing, while still
		// refusing to cross a discontinuity
		segPos := maxSafeSegmentPos(pl)
		for pos := 0; pos <= segPos; pos++ {
			if pl.Segments[pos].Discontinuity {
				segPos = pos
				break
			}
		}
		return pl.Segments[segPos], segPos, true, nil
	}

	nextPos := curPos + 1
	if nextPos >= len(pl.Segments) {
		if pl.Endlist {
			return nil, 0, false, ErrClientEOS
		}
		return nil, 0, false, fmt.Errorf("next segment not found or not ready yet")
	}

	// keep playing sequentially while within one segment duration of the
	// target latency
	if segmentEdgeDistance(pl, nextPos) <= d.targetLatency+curSeg.Duration {
		return pl.Segments[nextPos], nextPos, false, nil
	}

	// playback is lagging behind: jump forward to the segment closest to the
	// target, clamped by the safe boundary and discontinuities
	catchupPos := catchupSegmentPos(pl, curPos, d.targetLatency)
	if catchupPos == curPos {
		return pl.Segments[nextPos], nextPos, false, nil
	}

	return pl.Segments[catchupPos], catchupPos, true, nil
}

func (d *clientStreamDownloader) startStreaming(
	errGroupCtx context.Context,
	allTracks map[*Track]*clientTrack,
) bool {
	select {
	case d.chStartStreaming <- allTracks:
		return true
	case <-errGroupCtx.Done():
		return false
	}
}

func (d *clientStreamDownloader) onProcessorError(errGroupCtx context.Context, err error) {
	d.parent.onProcessorError(errGroupCtx, err)
}

func (d *clientStreamDownloader) setTimeConv(ts clientTimeConv) {
	d.parent.setTimeConv(ts)
}

func (d *clientStreamDownloader) waitTimeConv(errGroupCtx context.Context) (clientTimeConv, bool) {
	return d.parent.waitTimeConv(errGroupCtx)
}

func (d *clientStreamDownloader) setTracks(errGroupCtx context.Context, tracks []*Track) ([]*clientTrack, bool) {
	select {
	case d.chTracks <- tracks:
	case <-errGroupCtx.Done():
		return nil, false
	}

	var allTracks map[*Track]*clientTrack

	select {
	case allTracks = <-d.chStartStreaming:
	case <-errGroupCtx.Done():
		return nil, false
	}

	streamTracks := make([]*clientTrack, len(tracks))
	for i, track := range tracks {
		streamTracks[i] = allTracks[track]
	}

	return streamTracks, true
}
