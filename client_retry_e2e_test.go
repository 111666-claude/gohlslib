package gohlslib_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4/seekablebuffer"
	mp4codecs "github.com/bluenviron/mediacommon/v2/pkg/formats/mp4/codecs"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/mpegts"
	tscodecs "github.com/bluenviron/mediacommon/v2/pkg/formats/mpegts/codecs"
	"github.com/stretchr/testify/require"

	"github.com/bluenviron/gohlslib/v2"
)

const (
	retryTestInitialBackoff = 20 * time.Millisecond
	retryTestMaxBackoff     = 100 * time.Millisecond
	retryTestTimeout        = 5 * time.Second
)

// retryTestSegment builds a small, valid MPEG-TS segment containing one H264
// access unit, so a successful download produces exactly one data callback.
func retryTestSegment(t *testing.T) []byte {
	t.Helper()

	var buf bytes.Buffer
	h264Track := &mpegts.Track{Codec: &tscodecs.H264{}}
	mw := &mpegts.Writer{W: &buf, Tracks: []*mpegts.Track{h264Track}}
	require.NoError(t, mw.Initialize())
	require.NoError(t, mw.WriteH264(
		h264Track,
		90000,
		90000,
		[][]byte{
			{7, 1, 2, 3}, // SPS
			{8},          // PPS
			{5},          // IDR
		},
	))

	return buf.Bytes()
}

// retryTestPlaylist is a single-segment VOD playlist. A single segment is
// enough because the behavior under test is the download of that segment.
func retryTestPlaylist() []byte {
	return []byte("#EXTM3U\n" +
		"#EXT-X-VERSION:3\n" +
		"#EXT-X-PLAYLIST-TYPE:VOD\n" +
		"#EXT-X-TARGETDURATION:1\n" +
		"#EXT-X-MEDIA-SEQUENCE:0\n" +
		"#EXTINF:1,\n" +
		"segment1.ts\n" +
		"#EXT-X-ENDLIST\n")
}

// retryTestServer runs an httptest server whose segment handler is fully
// controlled by the caller. The returned counter is safe to read while the
// client is running.
func retryTestServer(
	t *testing.T,
	segmentHandler func(w http.ResponseWriter, r *http.Request, count int64),
) (*httptest.Server, *atomic.Int64) {
	t.Helper()

	var segmentRequests atomic.Int64

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/index.m3u8":
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			_, _ = w.Write(retryTestPlaylist())

		case "/segment1.ts":
			count := segmentRequests.Add(1)
			segmentHandler(w, r, count)

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)

	return server, &segmentRequests
}

// retryTestClient starts a client delivering every received H26x access unit
// onto the returned channel, and returns the client together with the channel.
func retryTestClient(
	t *testing.T,
	uri string,
	retry *gohlslib.ClientRetryPolicy,
	httpClient *http.Client,
) (*gohlslib.Client, chan [][]byte) {
	t.Helper()

	data := make(chan [][]byte, 16)
	var c *gohlslib.Client
	c = &gohlslib.Client{
		URI:        uri,
		HTTPClient: httpClient,
		Retry:      retry,
		OnTracks: func(tracks []*gohlslib.Track) error {
			c.OnDataH26x(tracks[0], func(_ int64, _ int64, au [][]byte) {
				data <- au
			})
			return nil
		},
	}
	require.NoError(t, c.Start())
	t.Cleanup(c.Close)

	return c, data
}

// retryTestPolicy returns a fast policy suitable for unit tests.
func retryTestPolicy(maxAttempts int) *gohlslib.ClientRetryPolicy {
	return &gohlslib.ClientRetryPolicy{
		MaxAttempts:    maxAttempts,
		InitialBackoff: retryTestInitialBackoff,
		MaxBackoff:     retryTestMaxBackoff,
	}
}

func TestClientRetrySegmentTransientThenSuccess(t *testing.T) {
	payload := retryTestSegment(t)

	server, segmentRequests := retryTestServer(t, func(w http.ResponseWriter, _ *http.Request, count int64) {
		if count <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}

		w.Header().Set("Content-Type", "video/MP2T")
		_, _ = w.Write(payload)
	})

	c, data := retryTestClient(t, server.URL+"/index.m3u8", retryTestPolicy(4), nil)

	select {
	case au := <-data:
		require.Equal(t, [][]byte{{7, 1, 2, 3}, {8}, {5}}, au)
	case <-time.After(retryTestTimeout):
		t.Fatal("timed out waiting for segment data")
	}

	// exactly three HTTP attempts, one successful payload delivery
	require.Equal(t, int64(3), segmentRequests.Load())
	require.Empty(t, data)

	// the single segment is followed by EOS, not by a second delivery
	require.Equal(t, gohlslib.ErrClientEOS, c.Wait2())
}

func TestClientRetrySegmentPersistentFailure(t *testing.T) {
	server, segmentRequests := retryTestServer(t, func(w http.ResponseWriter, _ *http.Request, _ int64) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})

	c, data := retryTestClient(t, server.URL+"/index.m3u8", retryTestPolicy(3), nil)

	start := time.Now()
	err := c.Wait2()
	elapsed := time.Since(start)

	require.Error(t, err)
	require.Contains(t, err.Error(), "503")
	require.Equal(t, int64(3), segmentRequests.Load())
	require.Empty(t, data)

	// two backoffs separate the three attempts; allow slack for scheduling.
	require.GreaterOrEqual(t, elapsed, 2*retryTestInitialBackoff-5*time.Millisecond)
	require.Less(t, elapsed, retryTestTimeout)
}

func TestClientRetryPermanentErrorIsNotRetried(t *testing.T) {
	server, segmentRequests := retryTestServer(t, func(w http.ResponseWriter, _ *http.Request, _ int64) {
		w.WriteHeader(http.StatusNotFound)
	})

	c, data := retryTestClient(t, server.URL+"/index.m3u8", retryTestPolicy(5), nil)

	err := c.Wait2()
	require.Error(t, err)
	require.Contains(t, err.Error(), "404")
	require.Equal(t, int64(1), segmentRequests.Load())
	require.Empty(t, data)
}

func TestClientRetryDisabledByDefault(t *testing.T) {
	server, segmentRequests := retryTestServer(t, func(w http.ResponseWriter, _ *http.Request, _ int64) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})

	// no Retry policy: a single attempt, no backoff, immediate error
	c, data := retryTestClient(t, server.URL+"/index.m3u8", nil, nil)

	start := time.Now()
	err := c.Wait2()
	elapsed := time.Since(start)

	require.Error(t, err)
	require.Contains(t, err.Error(), "503")
	require.Equal(t, int64(1), segmentRequests.Load())
	require.Less(t, elapsed, retryTestInitialBackoff)
	require.Empty(t, data)
}

func TestClientRetryCancelledDuringBackoff(t *testing.T) {
	server, segmentRequests := retryTestServer(t, func(w http.ResponseWriter, _ *http.Request, _ int64) {
		// never succeeds while the test is running
		w.WriteHeader(http.StatusServiceUnavailable)
	})

	c, data := retryTestClient(t, server.URL+"/index.m3u8", &gohlslib.ClientRetryPolicy{
		MaxAttempts:    10,
		InitialBackoff: 500 * time.Millisecond,
		MaxBackoff:     time.Second,
	}, nil)

	// wait for the first failed attempt, then cancel while the backoff is pending
	require.Eventually(t, func() bool {
		return segmentRequests.Load() == 1
	}, retryTestTimeout, time.Millisecond)

	start := time.Now()
	c.Close()
	elapsed := time.Since(start)

	// Close returns promptly and no further attempt is performed after it
	require.Less(t, elapsed, 200*time.Millisecond)
	require.Equal(t, int64(1), segmentRequests.Load())
	require.Empty(t, data)
}

func TestClientRetryCancelledDuringInFlightRequest(t *testing.T) {
	requested := make(chan struct{})
	released := make(chan struct{})
	t.Cleanup(func() { close(released) })

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/index.m3u8":
			_, _ = w.Write(retryTestPlaylist())

		case "/segment1.ts":
			close(requested)
			select {
			case <-r.Context().Done():
				return
			case <-released:
				w.WriteHeader(http.StatusServiceUnavailable)
			}

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)

	c, _ := retryTestClient(t, server.URL+"/index.m3u8", &gohlslib.ClientRetryPolicy{
		MaxAttempts:    10,
		InitialBackoff: retryTestInitialBackoff,
	}, nil)

	select {
	case <-requested:
	case <-time.After(retryTestTimeout):
		t.Fatal("timed out waiting for first request")
	}

	// cancel while the request body is still open: the context must abort it
	done := make(chan struct{})
	go func() {
		c.Close()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(retryTestTimeout):
		t.Fatal("Close did not return while a request was in flight")
	}
}

// failingRoundTripper fails the first failuresLeft requests whose URL path
// matches failPath with err, then delegates to the wrapped transport.
type failingRoundTripper struct {
	transport    http.RoundTripper
	failuresLeft atomic.Int64
	failPath     string
	err          error
	attempts     atomic.Int64
}

func (rt *failingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Path == rt.failPath && rt.failuresLeft.Add(-1) >= 0 {
		rt.attempts.Add(1)
		return nil, rt.err
	}

	return rt.transport.RoundTrip(req)
}

func TestClientRetryRetriableNetworkError(t *testing.T) {
	payload := retryTestSegment(t)

	server, segmentRequests := retryTestServer(t, func(w http.ResponseWriter, _ *http.Request, _ int64) {
		w.Header().Set("Content-Type", "video/MP2T")
		_, _ = w.Write(payload)
	})

	baseTransport := http.DefaultTransport.(*http.Transport).Clone()
	t.Cleanup(baseTransport.CloseIdleConnections)

	transport := &failingRoundTripper{
		transport: baseTransport,
		failPath:  "/segment1.ts",
		err:       errors.New("connection reset by peer"),
	}
	transport.failuresLeft.Store(2)
	httpClient := &http.Client{Transport: transport}

	c, data := retryTestClient(t, server.URL+"/index.m3u8", retryTestPolicy(4), httpClient)

	select {
	case <-data:
	case <-time.After(retryTestTimeout):
		t.Fatal("timed out waiting for segment data")
	}

	require.Equal(t, int64(2), transport.attempts.Load())
	require.Equal(t, gohlslib.ErrClientEOS, c.Wait2())
	require.Empty(t, data)

	// the server only saw the successful, third HTTP attempt
	require.Equal(t, int64(1), segmentRequests.Load())
}

func TestClientRetryNonRetriableContextError(t *testing.T) {
	baseTransport := http.DefaultTransport.(*http.Transport).Clone()
	t.Cleanup(baseTransport.CloseIdleConnections)

	// a RoundTripper that reports context.Canceled must not be retried
	rt := &failingRoundTripper{
		transport: baseTransport,
		failPath:  "/segment1.ts",
		err:       context.Canceled,
	}
	rt.failuresLeft.Store(10)

	server, segmentRequests := retryTestServer(t, func(w http.ResponseWriter, _ *http.Request, _ int64) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})

	c, _ := retryTestClient(t, server.URL+"/index.m3u8", retryTestPolicy(5), &http.Client{Transport: rt})

	err := c.Wait2()
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, int64(1), rt.attempts.Load())
	require.Equal(t, int64(0), segmentRequests.Load())
}

func TestClientRetryAfterHeader(t *testing.T) {
	for _, ca := range []struct {
		name       string
		header     string
		minElapsed time.Duration
	}{
		{
			// a 1 s hint is capped at MaxBackoff (100 ms)
			name:       "delta seconds",
			header:     "1",
			minElapsed: retryTestMaxBackoff,
		},
		{
			name:       "malformed value falls back to exponential",
			header:     "not-a-number",
			minElapsed: retryTestInitialBackoff,
		},
		{
			name:       "missing falls back to exponential",
			header:     "",
			minElapsed: retryTestInitialBackoff,
		},
	} {
		t.Run(ca.name, func(t *testing.T) {
			server, segmentRequests := retryTestServer(t,
				func(w http.ResponseWriter, _ *http.Request, count int64) {
					if count == 1 && ca.header != "" {
						w.Header().Set("Retry-After", ca.header)
					}

					if count <= 2 {
						w.WriteHeader(http.StatusServiceUnavailable)
						return
					}

					w.Header().Set("Content-Type", "video/MP2T")
					_, _ = w.Write(retryTestSegment(t))
				})

			c, data := retryTestClient(t, server.URL+"/index.m3u8", retryTestPolicy(4), nil)

			start := time.Now()
			select {
			case <-data:
			case <-time.After(retryTestTimeout):
				t.Fatal("timed out waiting for segment data")
			}
			elapsed := time.Since(start)

			require.Equal(t, int64(3), segmentRequests.Load())
			require.GreaterOrEqual(t, elapsed, ca.minElapsed-10*time.Millisecond)

			require.Equal(t, gohlslib.ErrClientEOS, c.Wait2())
		})
	}
}

func TestClientRetryLowLatencyPart(t *testing.T) {
	// audio-only fMP4 low-latency playlist with a preload hint.
	var initBuffer seekablebuffer.Buffer
	require.NoError(t, (&fmp4.Init{Tracks: []*fmp4.InitTrack{{
		ID:        1,
		TimeScale: 44100,
		Codec: &mp4codecs.MPEG4Audio{
			Config: testAACConfig,
		},
	}}}).Marshal(&initBuffer))

	var partBuffer seekablebuffer.Buffer
	require.NoError(t, (&fmp4.Part{Tracks: []*fmp4.PartTrack{{
		ID: 1,
		Samples: []*fmp4.Sample{{
			Duration: 4410,
			Payload:  []byte{1, 2, 3, 4},
		}},
	}}}).Marshal(&partBuffer))

	commonHeader := []byte("#EXTM3U\n" +
		"#EXT-X-VERSION:9\n" +
		"#EXT-X-SERVER-CONTROL:CAN-BLOCK-RELOAD=YES,PART-HOLD-BACK=1.00000\n" +
		"#EXT-X-MEDIA-SEQUENCE:0\n" +
		"#EXT-X-TARGETDURATION:1\n" +
		"#EXT-X-MAP:URI=\"init.mp4\"\n" +
		"#EXT-X-PROGRAM-DATE-TIME:2026-11-17T10:00:00.000Z\n")

	// the initial playlist advertises the part as a preload hint; once the
	// part has been produced, subsequent reloads drop the hint so the client
	// terminates cleanly instead of requesting the part again.
	playlistWithHint := append(append([]byte(nil), commonHeader...),
		[]byte("#EXTINF:1,\nsegment.mp4\n"+
			"#EXT-X-PRELOAD-HINT:TYPE=PART,URI=part1.mp4\n")...)
	playlistFinal := append(append([]byte(nil), commonHeader...),
		[]byte("#EXTINF:1,\nsegment.mp4\n"+
			"#EXT-X-PART:DURATION=0.100000,URI=\"part1.mp4\",INDEPENDENT=YES\n"+
			"#EXT-X-ENDLIST\n")...)

	var (
		mu            sync.Mutex
		partProduced  bool
		partRequests  atomic.Int64
		partDelivered = make(chan struct{})
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/index.m3u8":
			mu.Lock()
			body := playlistWithHint
			if partProduced {
				body = playlistFinal
			}
			mu.Unlock()
			_, _ = w.Write(body)

		case "/init.mp4":
			_, _ = w.Write(initBuffer.Bytes())

		case "/segment.mp4":
			// the segment is never requested in the preload-hint path
			w.WriteHeader(http.StatusNotFound)

		case "/part1.mp4":
			count := partRequests.Add(1)
			if count == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}

			_, _ = w.Write(partBuffer.Bytes())
			mu.Lock()
			partProduced = true
			mu.Unlock()
			close(partDelivered)

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)

	data := make(chan struct{}, 4)
	var c *gohlslib.Client
	c = &gohlslib.Client{
		URI: server.URL + "/index.m3u8",
		Retry: &gohlslib.ClientRetryPolicy{
			MaxAttempts:    3,
			InitialBackoff: retryTestInitialBackoff,
			MaxBackoff:     retryTestMaxBackoff,
		},
		OnTracks: func(tracks []*gohlslib.Track) error {
			c.OnDataMPEG4Audio(tracks[0], func(_ int64, _ [][]byte) {
				data <- struct{}{}
			})
			return nil
		},
	}
	require.NoError(t, c.Start())

	select {
	case <-partDelivered:
	case <-time.After(retryTestTimeout):
		t.Fatal("part was never downloaded successfully")
	}

	select {
	case <-data:
	case <-time.After(retryTestTimeout):
		t.Fatal("timed out waiting for part data")
	}

	// one failed attempt followed by one successful attempt, one delivery
	require.Equal(t, int64(2), partRequests.Load())
	require.Empty(t, data)

	// the hint disappears on the next reload, ending the client with a
	// deterministic error rather than another download
	err := c.Wait2()
	require.Error(t, err)
	require.EqualError(t, err, "preload hint disappeared")
}
