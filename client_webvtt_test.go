package gohlslib_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/bluenviron/gohlslib/v2"
	"github.com/bluenviron/gohlslib/v2/pkg/codecs"
)

type collectedCue struct {
	start time.Duration
	end   time.Duration
	text  string
}

// writeIDRAt writes one IDR access unit at the given media timestamp (seconds).
func writeIDRAt(t *testing.T, m *gohlslib.Muxer, track *gohlslib.Track, secs float64) {
	t.Helper()
	pts := int64(secs * float64(track.ClockRate))
	err := m.WriteH264(track, testTime.Add(time.Duration(secs*float64(time.Second))), pts, [][]byte{
		testH264SPS,
		{8},
		{5}, // IDR
	})
	require.NoError(t, err)
}

// seedMuxer deterministically produces nSegments one-second segments,
// driving rotation purely from PTS differences (no wall-clock dependency).
func seedMuxer(
	t *testing.T,
	m *gohlslib.Muxer,
	videoTrack *gohlslib.Track,
	enTrack *gohlslib.Track,
	esTrack *gohlslib.Track,
	nSegments int,
) {
	t.Helper()

	for i := range nSegments + 1 {
		secs := float64(i)
		writeIDRAt(t, m, videoTrack, secs)

		// short cues, one per language, one per second, 400ms long
		if i >= 1 {
			base := time.Duration(i) * time.Second
			err := m.WriteWebVTT(enTrack, base, base+400*time.Millisecond, fmt.Sprintf("en-%d", i))
			require.NoError(t, err)
			err = m.WriteWebVTT(esTrack, base, base+400*time.Millisecond, fmt.Sprintf("es-%d", i))
			require.NoError(t, err)
		}
	}

	// one long cue crossing three one-second segment boundaries, placed late
	// enough to still be inside the retained segment window when the client joins
	err := m.WriteWebVTT(enTrack, 3500*time.Millisecond, 6500*time.Millisecond, "en-cross")
	require.NoError(t, err)
}

// continueLive keeps appending media after the initial backlog, so the live
// playlist keeps advancing while the client is connected. Wall-clock and PTS
// advance at the same rate to keep real and declared segment durations aligned.
func continueLive(
	t *testing.T,
	m *gohlslib.Muxer,
	videoTrack *gohlslib.Track,
	enTrack *gohlslib.Track,
	esTrack *gohlslib.Track,
	startSec int,
	stop <-chan struct{},
) {
	t.Helper()

	step := 250 * time.Millisecond
	elapsed := time.Duration(startSec) * time.Second
	ticker := time.NewTicker(step)
	defer ticker.Stop()

	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
		}

		elapsed += step
		pts := elapsed.Milliseconds() * int64(videoTrack.ClockRate) / 1000
		err := m.WriteH264(videoTrack, testTime.Add(elapsed), pts, [][]byte{
			testH264SPS,
			{8},
			{5},
		})
		require.NoError(t, err)

		// one short cue per language per frame, 200ms long
		label := fmt.Sprintf("live-%d", elapsed.Milliseconds())
		err = m.WriteWebVTT(enTrack, elapsed, elapsed+200*time.Millisecond, "en-"+label)
		require.NoError(t, err)
		err = m.WriteWebVTT(esTrack, elapsed, elapsed+200*time.Millisecond, "es-"+label)
		require.NoError(t, err)
	}
}

func newSubtitleMuxer(t *testing.T) (*gohlslib.Muxer, *gohlslib.Track, *gohlslib.Track, *gohlslib.Track) {
	t.Helper()

	videoTrack := newTestVideoTrack()
	enTrack := &gohlslib.Track{
		Codec:     &codecs.WebVTT{},
		ClockRate: 1000,
		Name:      "English",
		Language:  "en",
		IsDefault: true,
	}
	esTrack := &gohlslib.Track{
		Codec:     &codecs.WebVTT{},
		ClockRate: 1000,
		Name:      "Español",
		Language:  "es",
	}

	m := &gohlslib.Muxer{
		Variant:            gohlslib.MuxerVariantFMP4,
		SegmentCount:       7,
		SegmentMinDuration: 1 * time.Second,
		Tracks:             []*gohlslib.Track{videoTrack, enTrack, esTrack},
		OnEncodeError:      func(error) {},
	}
	require.NoError(t, m.Start())

	return m, videoTrack, enTrack, esTrack
}

func muxerHandler(m *gohlslib.Muxer) http.Handler {
	return http.HandlerFunc(m.Handle)
}

func silentClient(uri string) *gohlslib.Client {
	return &gohlslib.Client{
		URI:                       uri,
		OnDownloadPrimaryPlaylist: func(string) {},
		OnDownloadStreamPlaylist:  func(string) {},
		OnDownloadSegment:         func(string) {},
		OnDownloadPart:            func(string) {},
		OnDecodeError:             func(error) {},
	}
}

func TestClientWebVTTEndToEnd(t *testing.T) {
	m, videoTrack, enTrack, esTrack := newSubtitleMuxer(t)

	server := httptest.NewServer(muxerHandler(m))
	t.Cleanup(server.Close)
	t.Cleanup(m.Close)

	// deterministically build a backlog before the client joins while keeping
	// the early boundary-crossing cue inside the retained segment window
	seedMuxer(t, m, videoTrack, enTrack, esTrack, 6)

	// keep the live stream advancing so the client never reaches the playlist end
	stop := make(chan struct{})
	go continueLive(t, m, videoTrack, enTrack, esTrack, 7, stop)
	t.Cleanup(func() { close(stop) })

	var (
		mu       sync.Mutex
		enCues   []collectedCue
		esCues   []collectedCue
		tracksOK = make(chan struct{})
		enough   = make(chan struct{})
	)

	c := silentClient(server.URL + "/index.m3u8")
	c.OnTracks = func(tracks []*gohlslib.Track) error {
		var subtitleCount int
		for _, track := range tracks {
			if _, ok := track.Codec.(*codecs.WebVTT); ok {
				subtitleCount++
				cbTrack := track
				c.OnDataWebVTT(cbTrack, func(start, end time.Duration, text string) {
					mu.Lock()
					defer mu.Unlock()
					cue := collectedCue{start: start, end: end, text: text}
					if cbTrack.Language == "es" {
						esCues = append(esCues, cue)
					} else {
						enCues = append(enCues, cue)
					}

					if len(enCues) >= 2 && len(esCues) >= 2 {
						select {
						case <-enough:
						default:
							close(enough)
						}
					}
				})
			}
		}
		require.Equal(t, 2, subtitleCount)
		close(tracksOK)
		return nil
	}

	require.NoError(t, c.Start())

	select {
	case <-tracksOK:
	case <-time.After(8 * time.Second):
		t.Fatal("tracks were not reported")
	}

	select {
	case <-enough:
	case <-time.After(10 * time.Second):
		t.Fatal("not enough cues received")
	}

	c.Close()
	require.EqualError(t, c.Wait2(), "terminated")

	mu.Lock()
	defer mu.Unlock()

	validateCues := func(cues []collectedCue) {
		for _, cue := range cues {
			require.Greater(t, cue.end, cue.start, "cue end must follow start")
			require.GreaterOrEqual(t, cue.start, time.Duration(0))
			require.NotEmpty(t, cue.text)
		}
	}
	validateCues(enCues)
	validateCues(esCues)

	// the long crossing cue must be delivered exactly once (no duplication),
	// preserving its original 3s duration despite the client-relative origin
	var crossCount int
	for _, cue := range enCues {
		if cue.text == "en-cross" {
			crossCount++
			require.Equal(t, 3*time.Second, cue.end-cue.start)
		}
	}
	require.Equal(t, 1, crossCount, "crossing cue was duplicated or lost")

	// ordinary short cues survive, each exactly 400ms long
	shortRe := regexp.MustCompile(`^en-\d+$`)
	shortCount := 0
	for _, cue := range enCues {
		if shortRe.MatchString(cue.text) {
			shortCount++
			require.Equal(t, 400*time.Millisecond, cue.end-cue.start)
		}
	}
	require.GreaterOrEqual(t, shortCount, 1)

	// both languages must have produced cues
	require.GreaterOrEqual(t, len(esCues), 2)
}

func TestClientWebVTTLanguageSelection(t *testing.T) {
	m, videoTrack, enTrack, esTrack := newSubtitleMuxer(t)

	server := httptest.NewServer(muxerHandler(m))
	t.Cleanup(server.Close)
	t.Cleanup(m.Close)

	seedMuxer(t, m, videoTrack, enTrack, esTrack, 6)

	gotLanguages := make(chan string, 4)

	c := silentClient(server.URL + "/index.m3u8")
	c.SubtitleLanguage = "es"
	c.OnTracks = func(tracks []*gohlslib.Track) error {
		for _, track := range tracks {
			if _, ok := track.Codec.(*codecs.WebVTT); ok {
				select {
				case gotLanguages <- track.Language:
				default:
				}
			}
		}
		return nil
	}

	require.NoError(t, c.Start())

	select {
	case lang := <-gotLanguages:
		require.Equal(t, "es", lang)
	case <-time.After(8 * time.Second):
		t.Fatal("no subtitle track selected")
	}

	// give the stream a moment to prove no english rendition is downloaded
	time.Sleep(500 * time.Millisecond)
	select {
	case lang := <-gotLanguages:
		require.Equal(t, "es", lang)
	default:
	}

	c.Close()
	require.EqualError(t, c.Wait2(), "terminated")
}

func TestClientWebVTTLanguageNoMatch(t *testing.T) {
	m, videoTrack, enTrack, esTrack := newSubtitleMuxer(t)

	server := httptest.NewServer(muxerHandler(m))
	t.Cleanup(server.Close)
	t.Cleanup(m.Close)

	seedMuxer(t, m, videoTrack, enTrack, esTrack, 6)

	noSubtitles := make(chan struct{})

	c := silentClient(server.URL + "/index.m3u8")
	c.SubtitleLanguage = "fr" // no rendition matches
	c.OnTracks = func(tracks []*gohlslib.Track) error {
		for _, track := range tracks {
			if _, ok := track.Codec.(*codecs.WebVTT); ok {
				return fmt.Errorf("unexpected subtitle track for non-matching language")
			}
		}
		close(noSubtitles)
		return nil
	}

	require.NoError(t, c.Start())

	select {
	case <-noSubtitles:
	case <-time.After(8 * time.Second):
		t.Fatal("tracks were not reported")
	}

	c.Close()
	require.EqualError(t, c.Wait2(), "terminated")
}

func TestClientWebVTTNone(t *testing.T) {
	videoTrack := newTestVideoTrack()

	m := &gohlslib.Muxer{
		Variant:            gohlslib.MuxerVariantFMP4,
		SegmentCount:       7,
		SegmentMinDuration: 1 * time.Second,
		Tracks:             []*gohlslib.Track{videoTrack},
		OnEncodeError:      func(error) {},
	}
	require.NoError(t, m.Start())
	t.Cleanup(m.Close)

	server := httptest.NewServer(muxerHandler(m))
	t.Cleanup(server.Close)

	// deterministic backlog of video segments
	for i := range 7 {
		writeIDRAt(t, m, videoTrack, float64(i))
	}

	// keep the live stream advancing so the client never reaches the playlist end
	stop := make(chan struct{})
	go func() {
		step := 250 * time.Millisecond
		elapsed := 7 * time.Second
		ticker := time.NewTicker(step)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
			}
			elapsed += step
			pts := elapsed.Milliseconds() * int64(videoTrack.ClockRate) / 1000
			_ = m.WriteH264(videoTrack, testTime.Add(elapsed), pts, [][]byte{
				testH264SPS,
				{8},
				{5},
			})
		}
	}()
	t.Cleanup(func() { close(stop) })

	noSubtitles := make(chan struct{})

	c := silentClient(server.URL + "/index.m3u8")
	c.OnTracks = func(tracks []*gohlslib.Track) error {
		for _, track := range tracks {
			if _, ok := track.Codec.(*codecs.WebVTT); ok {
				return fmt.Errorf("unexpected subtitle track")
			}
		}
		close(noSubtitles)
		return nil
	}

	require.NoError(t, c.Start())

	select {
	case <-noSubtitles:
	case <-time.After(8 * time.Second):
		t.Fatal("tracks not reported")
	}

	c.Close()
	require.EqualError(t, c.Wait2(), "terminated")
}

func TestClientWebVTTNetworkFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	server.Close() // immediately unavailable

	c := silentClient(server.URL + "/index.m3u8")
	require.NoError(t, c.Start())

	done := make(chan error, 1)
	go func() { done <- c.Wait2() }()

	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("client did not terminate on network failure")
	}
}
