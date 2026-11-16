package gohlslib_test

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
	mp4codecs "github.com/bluenviron/mediacommon/v2/pkg/formats/mp4/codecs"
	"github.com/stretchr/testify/require"

	"github.com/bluenviron/gohlslib/v2"
)

const (
	targetLatencyTestPort         = "5781"
	targetLatencySegmentDuration  = time.Second
	targetLatencyFramesPerSegment = 30
	// large enough that the window never slides during a test unless the
	// edge-moved scenario is explicitly simulated
	targetLatencyPlaylistWindow = 1000
)

// targetLatencyServer is a local, dynamic HLS server that produces one second
// fMP4 segments on a ticker and allows the test to freeze the playlist (a
// short network stall) and to mark a discontinuity.
type targetLatencyServer struct {
	mu sync.Mutex

	t           *testing.T
	httpServ    *http.Server
	lowLatency  bool
	stopLLAfter int

	nextID        int // ID of the next segment to produce
	available     []int
	discontinuity map[int]struct{}
	stallUntil    time.Time
	stopped       bool

	downloadedIDs []int
}

func newTargetLatencyServer(t *testing.T, lowLatency bool, stopLLAfter int) *targetLatencyServer {
	t.Helper()

	s := &targetLatencyServer{
		t:             t,
		lowLatency:    lowLatency,
		stopLLAfter:   stopLLAfter,
		discontinuity: make(map[int]struct{}),
	}

	// pre-produce enough segments to fill the initial buffer
	for range 6 {
		s.produceSegment()
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/index.m3u8", s.handlePlaylist)
	mux.HandleFunc("/init.mp4", s.handleInit)
	mux.HandleFunc("/seg", s.handleSegment)

	s.httpServ = &http.Server{Handler: mux}

	ln, err := net.Listen("tcp", "localhost:"+targetLatencyTestPort)
	require.NoError(t, err)

	go s.httpServ.Serve(ln) //nolint:errcheck

	return s
}

func (s *targetLatencyServer) close() {
	s.httpServ.Shutdown(context.Background())
}

// run produces a new segment every second until stopped.
func (s *targetLatencyServer) run() {
	go func() {
		ticker := time.NewTicker(targetLatencySegmentDuration)
		defer ticker.Stop()

		for range ticker.C {
			s.mu.Lock()
			if s.stopped {
				s.mu.Unlock()
				return
			}
			s.produceSegment()
			s.mu.Unlock()
		}
	}()
}

func (s *targetLatencyServer) produceSegment() {
	s.available = append(s.available, s.nextID)
	s.nextID++
}

// stall blocks every playlist request for d, simulating a short network
// outage. The producer keeps running, so once the stall ends the playlist that
// is returned contains all the media that accumulated in the meantime.
func (s *targetLatencyServer) stall(d time.Duration) {
	s.mu.Lock()
	s.stallUntil = time.Now().Add(d)
	s.mu.Unlock()
}

func (s *targetLatencyServer) addDiscontinuity() {
	s.mu.Lock()
	s.discontinuity[s.nextID] = struct{}{}
	s.mu.Unlock()
}

func (s *targetLatencyServer) snapshot() (ids []int, ll bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.available) > targetLatencyPlaylistWindow {
		ids = append([]int(nil), s.available[len(s.available)-targetLatencyPlaylistWindow:]...)
	} else {
		ids = append([]int(nil), s.available...)
	}

	return ids, s.lowLatency
}

func (s *targetLatencyServer) handlePlaylist(w http.ResponseWriter, r *http.Request) {
	// during a stall the request is held open, like a blocked network call
	for {
		s.mu.Lock()
		stallUntil := s.stallUntil
		s.mu.Unlock()

		if time.Now().Before(stallUntil) {
			select {
			case <-time.After(50 * time.Millisecond):
				continue
			case <-r.Context().Done():
				return
			}
		}
		break
	}

	w.Header().Set("Content-Type", `application/vnd.apple.mpegurl`)

	ids, ll := s.snapshot()

	var out string
	out += "#EXTM3U\n"
	out += "#EXT-X-VERSION:9\n"

	if ll {
		out += "#EXT-X-SERVER-CONTROL:CAN-BLOCK-RELOAD=YES,PART-HOLD-BACK=3.00000\n"
	}

	out += "#EXT-X-TARGETDURATION:1\n"
	out += "#EXT-X-MEDIA-SEQUENCE:" + strconv.FormatInt(int64(ids[0]), 10) + "\n"
	out += "#EXT-X-MAP:URI=\"init.mp4\"\n"

	for _, id := range ids {
		if _, ok := s.discontinuity[id]; ok {
			out += "#EXT-X-DISCONTINUITY\n"
		}
		out += "#EXTINF:1.00000,\n"
		out += "/seg?id=" + strconv.FormatInt(int64(id), 10) + "\n"
	}

	if ll {
		// the preload hint points at the segment being produced, which is not
		// part of the list yet
		lastID := ids[len(ids)-1]
		out += "#EXT-X-PRELOAD-HINT:TYPE=PART,URI=\"/seg?id=" +
			strconv.FormatInt(int64(lastID+1), 10) + "\"\n"
	}

	w.Write([]byte(out))
}

func (s *targetLatencyServer) handleInit(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", `video/mp4`)

	err := mp4ToWriter(&fmp4.Init{
		Tracks: []*fmp4.InitTrack{{
			ID:        1,
			TimeScale: 90000,
			Codec: &mp4codecs.H264{
				SPS: testH264SPS,
				PPS: testH264PPS,
			},
		}},
	}, w)
	require.NoError(s.t, err)
}

func (s *targetLatencyServer) handleSegment(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.URL.Query().Get("id"))
	require.NoError(s.t, err)

	s.mu.Lock()
	s.downloadedIDs = append(s.downloadedIDs, id)
	stopLL := s.lowLatency && s.stopLLAfter != 0 && len(s.downloadedIDs) >= s.stopLLAfter
	s.lowLatency = s.lowLatency && !stopLL
	s.mu.Unlock()

	w.Header().Set("Content-Type", `video/mp4`)

	samples := make([]*fmp4.Sample, targetLatencyFramesPerSegment)
	for i := range samples {
		var payload []byte
		if i == 0 {
			payload = mustMarshalAVCC([][]byte{
				{7, 1, 2, 3}, // SPS
				{8},          // PPS
				{5},          // IDR, every segment is independently decodable
			})
		} else {
			payload = mustMarshalAVCC([][]byte{{1}}) // non-IDR frame
		}

		samples[i] = &fmp4.Sample{
			Duration: 90000 / targetLatencyFramesPerSegment,
			Payload:  payload,
		}
	}

	err = mp4ToWriter(&fmp4.Part{
		Tracks: []*fmp4.PartTrack{{
			ID:       1,
			BaseTime: uint64(id) * 90000,
			Samples:  samples,
		}},
	}, w)
	require.NoError(s.t, err)
}

func (s *targetLatencyServer) getDownloadedIDs() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int(nil), s.downloadedIDs...)
}

// dtsRecorder collects the DTS values delivered to the OnDataH26x callback in
// a concurrency-safe way, so the test can read them after the client is closed.
type dtsRecorder struct {
	mutex  sync.Mutex
	values []int64
}

func (r *dtsRecorder) add(dts int64) {
	r.mutex.Lock()
	r.values = append(r.values, dts)
	r.mutex.Unlock()
}

func (r *dtsRecorder) snapshot() []int64 {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	return append([]int64(nil), r.values...)
}

// runTargetLatencyClient starts a client and records every received DTS.
func runTargetLatencyClient(
	t *testing.T,
	target time.Duration,
	startDistance int,
) (*gohlslib.Client, *dtsRecorder, chan struct{}) {
	t.Helper()

	recorder := &dtsRecorder{}
	ready := make(chan struct{})

	var c *gohlslib.Client
	c = &gohlslib.Client{
		URI:           "http://localhost:" + targetLatencyTestPort + "/index.m3u8",
		TargetLatency: target,
		StartDistance: startDistance,
		OnTracks: func(tracks []*gohlslib.Track) error {
			c.OnDataH26x(tracks[0], func(_ int64, dts int64, _ [][]byte) {
				recorder.add(dts)
			})
			close(ready)
			return nil
		},
	}

	require.NoError(t, c.Start())

	<-ready

	return c, recorder, ready
}

// waitForDownloadedID waits until at least one segment with an ID >= minID has
// been downloaded.
func waitForDownloadedID(t *testing.T, s *targetLatencyServer, minID int) []int {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		ids := s.getDownloadedIDs()
		for _, id := range ids {
			if id >= minID {
				return ids
			}
		}
		time.Sleep(50 * time.Millisecond)
	}

	require.Fail(t, fmt.Sprintf("segment with ID >= %d was not downloaded", minID))
	return nil
}

// TestClientTargetLatencyCatchUp verifies that, after a short stall, the
// client jumps forward to the target latency without delivering segments
// twice, and that the output timeline stays monotonic.
func TestClientTargetLatencyCatchUp(t *testing.T) {
	server := newTargetLatencyServer(t, false, 0)
	defer server.close()
	server.run()

	c, recorder, _ := runTargetLatencyClient(t, 2*time.Second, 0)
	defer c.Close()

	// wait until the initial segments have been played
	waitForDownloadedID(t, server, 4)

	// freeze the playlist for a while: the server keeps producing media in the
	// meantime, so the client falls behind
	beforeStall := server.getDownloadedIDs()
	server.stall(5 * time.Second)

	// once the playlist moves again, the client must catch up with the edge
	ids := waitForDownloadedID(t, server, beforeStall[len(beforeStall)-1]+5)

	// the download sequence must never repeat or go backwards
	for i := 1; i < len(ids); i++ {
		require.Greater(t, ids[i], ids[i-1], "segment downloads must be strictly increasing")
	}

	// a jump (catch up) must have happened
	hasJump := false
	for i := 1; i < len(ids); i++ {
		if ids[i] > ids[i-1]+1 {
			hasJump = true
			break
		}
	}
	require.True(t, hasJump, "the client did not catch up with the live edge")

	// the output DTS must be strictly increasing, i.e. no frame is delivered
	// twice and the audio/video timeline stays monotonic
	dtsValues := recorder.snapshot()
	require.NotEmpty(t, dtsValues)
	for i := 1; i < len(dtsValues); i++ {
		require.Greater(t, dtsValues[i], dtsValues[i-1], "output DTS must be strictly increasing")
	}
}

// TestClientTargetLatencySmallTarget verifies that a target latency smaller
// than the distance of the safest available segment is clamped instead of
// producing an error.
func TestClientTargetLatencySmallTarget(t *testing.T) {
	server := newTargetLatencyServer(t, false, 0)
	defer server.close()
	server.run()

	c, _, _ := runTargetLatencyClient(t, 100*time.Millisecond, 0)
	defer c.Close()

	// the client must keep playing from the safe boundary (the last complete
	// segment), so new segments keep being downloaded
	waitForDownloadedID(t, server, 7)
}

// TestClientTargetLatencyDiscontinuity verifies that a catch-up jump never
// crosses a discontinuity.
func TestClientTargetLatencyDiscontinuity(t *testing.T) {
	server := newTargetLatencyServer(t, false, 0)
	defer server.close()
	server.run()

	c, _, _ := runTargetLatencyClient(t, 2*time.Second, 0)
	defer c.Close()

	waitForDownloadedID(t, server, 4)

	// mark every segment produced from now on as belonging to a new timeline
	server.addDiscontinuity()

	beforeStall := server.getDownloadedIDs()
	server.stall(5 * time.Second)
	ids := waitForDownloadedID(t, server, beforeStall[len(beforeStall)-1]+4)

	// find the first downloaded segment of the new timeline
	var firstNew int
	for i, id := range ids {
		if _, ok := server.discontinuity[id]; ok {
			firstNew = i
			break
		}
	}

	// the first segment of the new timeline must be downloaded sequentially:
	// the download immediately before it must be the previous ID, i.e. the
	// client never jumped over the discontinuity
	require.NotZero(t, firstNew, "no discontinuity segment was downloaded")
	require.Equal(t, ids[firstNew]-1, ids[firstNew-1])
}

// TestClientTargetLatencyLegacyBehavior verifies that, without a target
// latency, the client keeps the legacy sequential behavior and never jumps.
func TestClientTargetLatencyLegacyBehavior(t *testing.T) {
	server := newTargetLatencyServer(t, false, 0)
	defer server.close()
	server.run()

	c, _, _ := runTargetLatencyClient(t, 0, 3)
	defer c.Close()

	// let the legacy client play a number of segments in steady state
	ids := waitForDownloadedID(t, server, 9)

	// every download must be exactly one ID after the previous one: the legacy
	// path never skips segments and never delivers a segment twice
	require.NotEmpty(t, ids)
	for i := 1; i < len(ids); i++ {
		require.Equal(t, ids[i-1]+1, ids[i], "legacy mode must never skip segments")
	}
}

// TestClientTargetLatencyLowLatencyFallback verifies that a playlist that
// loses its low-latency tags is transparently handled as a real-time playlist
// and the client keeps playing.
func TestClientTargetLatencyLowLatencyFallback(t *testing.T) {
	// the server drops the low-latency tags after the third media download
	server := newTargetLatencyServer(t, true, 3)
	defer server.close()
	server.run()

	c, _, _ := runTargetLatencyClient(t, 2*time.Second, 0)
	defer c.Close()

	// playback must continue through the fallback and beyond
	waitForDownloadedID(t, server, 8)
}

// TestClientTargetLatencyInvalid verifies that an invalid target latency is
// rejected at startup.
func TestClientTargetLatencyInvalid(t *testing.T) {
	c := &gohlslib.Client{
		URI:           "http://localhost:" + targetLatencyTestPort + "/index.m3u8",
		TargetLatency: -time.Second,
	}

	require.Error(t, c.Start())
}

// TestClientTargetLatencyCloseDuringStall verifies that closing the client
// while it is blocked waiting on a frozen playlist (a network cancellation)
// returns promptly instead of hanging.
func TestClientTargetLatencyCloseDuringStall(t *testing.T) {
	server := newTargetLatencyServer(t, false, 0)
	defer server.close()
	server.run()

	c, _, _ := runTargetLatencyClient(t, 2*time.Second, 0)

	waitForDownloadedID(t, server, 3)

	// freeze the playlist so the downloader blocks on the reload, then close
	// the client: the cancellation must unblock every internal goroutine
	server.stall(30 * time.Second)

	done := make(chan struct{})
	go func() {
		c.Close()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close() did not return while the playlist was frozen")
	}
}

// TestClientTargetLatencyEdgeMoved verifies that, when the segments the client
// was playing leave the playlist window, the client resumes from the safe
// boundary instead of terminating.
func TestClientTargetLatencyEdgeMoved(t *testing.T) {
	server := newTargetLatencyServer(t, false, 0)
	defer server.close()
	server.run()

	c, _, _ := runTargetLatencyClient(t, 2*time.Second, 0)
	defer c.Close()

	waitForDownloadedID(t, server, 4)

	// shrink the window so the segments the client is playing are removed:
	// the next playlist reload contains only recent segments
	server.mu.Lock()
	server.available = append([]int(nil), server.available[len(server.available)-3:]...)
	server.mu.Unlock()

	// playback must keep going from the new window
	ids := waitForDownloadedID(t, server, server.nextID+1)
	require.NotEmpty(t, ids)
}
