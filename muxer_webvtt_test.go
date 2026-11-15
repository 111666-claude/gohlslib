package gohlslib_test

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/bluenviron/gohlslib/v2"
	"github.com/bluenviron/gohlslib/v2/pkg/codecs"
)

var testSubtitleTrackEN = &gohlslib.Track{
	Codec:     &codecs.WebVTT{},
	ClockRate: 1000,
	Name:      "English",
	Language:  "en",
}

var testSubtitleTrackES = &gohlslib.Track{
	Codec:     &codecs.WebVTT{},
	ClockRate: 1000,
	Name:      "Español",
	Language:  "es",
}

// newTestVideoTrack returns a fresh video track, avoiding state leakage
// of the package-level testVideoTrack across Muxer instances.
func newTestVideoTrack() *gohlslib.Track {
	return &gohlslib.Track{
		Codec: &codecs.H264{
			SPS: testH264SPS,
			PPS: []byte{0x08},
		},
		ClockRate: 90000,
	}
}

// writeH264IDR writes an IDR access unit at the given second on the timeline.
func writeH264IDR(t *testing.T, m *gohlslib.Muxer, track *gohlslib.Track, secs int64) {
	t.Helper()
	err := m.WriteH264(track, testTime.Add(time.Duration(secs)*time.Second),
		secs*int64(track.ClockRate),
		[][]byte{
			testH264SPS,
			{8},
			{5}, // IDR
		})
	require.NoError(t, err)
}

func containsLine(haystack string, needle string) bool {
	return regexp.MustCompile(`(^|\n)` + regexp.QuoteMeta(needle) + `\n`).MatchString(haystack)
}

func TestMuxerWebVTTMultivariantAndMediaPlaylists(t *testing.T) {
	videoTrack := newTestVideoTrack()

	m := &gohlslib.Muxer{
		Variant:            gohlslib.MuxerVariantFMP4,
		SegmentCount:       7,
		SegmentMinDuration: 1 * time.Second,
		Tracks: []*gohlslib.Track{
			videoTrack,
			testSubtitleTrackEN,
			testSubtitleTrackES,
		},
	}

	err := m.Start()
	require.NoError(t, err)
	defer m.Close()

	// live-style timing: drive one segment per second and emit cues as time advances
	//
	// second 0 -> first segment, no cue yet
	writeH264IDR(t, m, videoTrack, 0)

	// short cue fully inside the [0s,1s) segment
	err = m.WriteWebVTT(testSubtitleTrackEN, 200*time.Millisecond, 400*time.Millisecond, "short")
	require.NoError(t, err)
	err = m.WriteWebVTT(testSubtitleTrackES, 300*time.Millisecond, 500*time.Millisecond, "corto")
	require.NoError(t, err)

	// rotate into the [1s,2s) segment
	writeH264IDR(t, m, videoTrack, 1)

	// long cue crossing the boundary between segment 1 and 2 (0.7s -> 1.5s)
	err = m.WriteWebVTT(testSubtitleTrackEN, 700*time.Millisecond, 1500*time.Millisecond, "cross")
	require.NoError(t, err)

	// keep producing segments so content becomes available
	writeH264IDR(t, m, videoTrack, 2)
	writeH264IDR(t, m, videoTrack, 3)

	multi, _, err := doRequest(m, "index.m3u8")
	require.NoError(t, err)

	multiStr := string(multi)
	require.Contains(t, multiStr, `#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID="subtitles",LANGUAGE="en",NAME="English",AUTOSELECT=YES,DEFAULT=YES,URI="subtitles2_stream.m3u8"`)
	require.Contains(t, multiStr, `#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID="subtitles",LANGUAGE="es",NAME="Español",AUTOSELECT=YES,URI="subtitles3_stream.m3u8"`)
	require.Contains(t, multiStr, `SUBTITLES="subtitles"`)
	require.Contains(t, multiStr, "wvtt")

	// english media playlist
	pl, _, err := doRequest(m, "subtitles2_stream.m3u8")
	require.NoError(t, err)

	re := regexp.MustCompile(`#EXT-X-VERSION:6\n` +
		`#EXT-X-TARGETDURATION:\d+\n` +
		`#EXT-X-MEDIA-SEQUENCE:\d+\n` +
		`(?:#EXT-X-PROGRAM-DATE-TIME:.*?\n` +
		`#EXTINF:.*?\n` +
		`.*?_seg\d+\.vtt\n)+`)
	require.Regexp(t, re, string(pl))

	// every referenced segment must be downloadable and must be a WebVTT file
	segRe := regexp.MustCompile(`\S+_seg\d+\.vtt`)
	paths := segRe.FindAllString(string(pl), -1)
	require.NotEmpty(t, paths)

	foundShort := false
	foundCross := 0
	var crossBodies []string

	for _, path := range paths {
		body, _, err := doRequest(m, path)
		require.NoError(t, err)
		bodyStr := string(body)
		require.Contains(t, bodyStr, "WEBVTT")
		require.Contains(t, bodyStr, "X-TIMESTAMP-MAP")

		if containsLine(bodyStr, "short") {
			foundShort = true
		}

		if containsLine(bodyStr, "cross") {
			foundCross++
			crossBodies = append(crossBodies, bodyStr)
		}
	}

	require.True(t, foundShort, "short cue was not found in any segment")
	// the crossing cue must be present in two segments, but never lost
	require.GreaterOrEqual(t, foundCross, 2)

	// the crossing cue must keep its exact global timing (10.7s -> 11.5s)
	// in every segment that contains it
	for _, body := range crossBodies {
		require.Contains(t, body, "00:00:10.700 --> 00:00:11.500")
	}

	// the spanish rendition must carry its own cue, independent of english
	plES, _, err := doRequest(m, "subtitles3_stream.m3u8")
	require.NoError(t, err)
	foundCorto := false
	for _, path := range segRe.FindAllString(string(plES), -1) {
		body, _, err := doRequest(m, path)
		require.NoError(t, err)
		if containsLine(string(body), "corto") {
			foundCorto = true
		}
	}
	require.True(t, foundCorto, "spanish cue was not found")
}

func TestMuxerWebVTTTimelineAlignment(t *testing.T) {
	videoTrack := newTestVideoTrack()

	m := &gohlslib.Muxer{
		Variant:            gohlslib.MuxerVariantFMP4,
		SegmentCount:       3,
		SegmentMinDuration: 1 * time.Second,
		Tracks:             []*gohlslib.Track{videoTrack, testSubtitleTrackEN},
	}

	err := m.Start()
	require.NoError(t, err)
	defer m.Close()

	writeH264IDR(t, m, videoTrack, 0)
	writeH264IDR(t, m, videoTrack, 1)

	// cue on the user timeline: 0.5s -> 0.9s
	err = m.WriteWebVTT(testSubtitleTrackEN, 500*time.Millisecond, 900*time.Millisecond, "aligned")
	require.NoError(t, err)

	// rotate enough for the leading stream to expose content
	writeH264IDR(t, m, videoTrack, 2)
	writeH264IDR(t, m, videoTrack, 3)

	pl, _, err := doRequest(m, "subtitles2_stream.m3u8")
	require.NoError(t, err)

	firstSeg := regexp.MustCompile(`\S+_seg\d+\.vtt`).FindString(string(pl))
	require.NotEmpty(t, firstSeg)

	body, _, err := doRequest(m, firstSeg)
	require.NoError(t, err)

	require.Contains(t, string(body), "00:00:10.500 --> 00:00:10.900")
	require.Contains(t, string(body), "aligned")
}

func TestMuxerWebVTTRejectsMPEGTSAndBadTrack(t *testing.T) {
	m := &gohlslib.Muxer{
		Variant: gohlslib.MuxerVariantMPEGTS,
		Tracks:  []*gohlslib.Track{newTestVideoTrack(), testSubtitleTrackEN},
	}
	err := m.Start()
	require.Error(t, err)

	m2 := &gohlslib.Muxer{
		Variant: gohlslib.MuxerVariantFMP4,
		Tracks:  []*gohlslib.Track{newTestVideoTrack(), testSubtitleTrackEN},
	}
	require.NoError(t, m2.Start())
	defer m2.Close()

	// writing a cue on a non-subtitle track must fail
	err = m2.WriteWebVTT(newTestVideoTrack(), 0, time.Second, "x")
	require.Error(t, err)

	// an inverted cue must fail
	err = m2.WriteWebVTT(testSubtitleTrackEN, 2*time.Second, time.Second, "x")
	require.Error(t, err)
}

func TestMuxerWebVTTWithoutAVTrack(t *testing.T) {
	m := &gohlslib.Muxer{
		Variant: gohlslib.MuxerVariantFMP4,
		Tracks:  []*gohlslib.Track{testSubtitleTrackEN},
	}
	err := m.Start()
	require.Error(t, err)
}

func TestMuxerWebVTTMultipleDefaultRejected(t *testing.T) {
	t1 := &gohlslib.Track{Codec: &codecs.WebVTT{}, ClockRate: 1000, IsDefault: true}
	t2 := &gohlslib.Track{Codec: &codecs.WebVTT{}, ClockRate: 1000, IsDefault: true}

	m := &gohlslib.Muxer{
		Variant: gohlslib.MuxerVariantFMP4,
		Tracks:  []*gohlslib.Track{newTestVideoTrack(), t1, t2},
	}
	err := m.Start()
	require.Error(t, err)
}

// sanity check that plain audio/video muxing still works with the new branching
func TestMuxerWebVTTAudioVideoStillWorks(t *testing.T) {
	for _, variant := range []gohlslib.MuxerVariant{
		gohlslib.MuxerVariantFMP4,
		gohlslib.MuxerVariantLowLatency,
	} {
		t.Run(fmt.Sprintf("%v", variant), func(t *testing.T) {
			videoTrack := newTestVideoTrack()

			m := &gohlslib.Muxer{
				Variant:            variant,
				SegmentCount:       7,
				SegmentMinDuration: 1 * time.Second,
				Tracks:             []*gohlslib.Track{videoTrack, testAudioTrack},
			}
			require.NoError(t, m.Start())
			defer m.Close()

			writeH264IDR(t, m, videoTrack, 0)
			writeH264IDR(t, m, videoTrack, 1)
			writeH264IDR(t, m, videoTrack, 2)

			_, _, err := doRequest(m, "index.m3u8")
			require.NoError(t, err)
		})
	}
}

func TestMuxerWebVTTLowLatencyVariant(t *testing.T) {
	videoTrack := newTestVideoTrack()

	m := &gohlslib.Muxer{
		Variant:            gohlslib.MuxerVariantLowLatency,
		SegmentCount:       7,
		SegmentMinDuration: 1 * time.Second,
		Tracks:             []*gohlslib.Track{videoTrack, testSubtitleTrackEN},
	}

	require.NoError(t, m.Start())
	defer m.Close()

	writeH264IDR(t, m, videoTrack, 0)
	err := m.WriteWebVTT(testSubtitleTrackEN, 200*time.Millisecond, 800*time.Millisecond, "ll-cue")
	require.NoError(t, err)

	for _, secs := range []int64{1, 2, 3} {
		writeH264IDR(t, m, videoTrack, secs)
	}

	// subtitle rendition is still advertised in the multivariant playlist
	multi, _, err := doRequest(m, "index.m3u8")
	require.NoError(t, err)
	require.Contains(t, string(multi), `TYPE=SUBTITLES`)
	require.Contains(t, string(multi), "subtitles2_stream.m3u8")

	// the subtitle media playlist is a traditional (non-LL) WebVTT playlist
	pl, _, err := doRequest(m, "subtitles2_stream.m3u8")
	require.NoError(t, err)
	plStr := string(pl)
	require.Contains(t, plStr, "#EXT-X-VERSION:6")
	require.NotContains(t, plStr, "#EXT-X-PRELOAD-HINT")

	// and its segments are downloadable WebVTT files
	paths := regexp.MustCompile(`\S+_seg\d+\.vtt`).FindAllString(plStr, -1)
	require.NotEmpty(t, paths)

	found := false
	for _, path := range paths {
		body, _, err := doRequest(m, path)
		require.NoError(t, err)
		if containsLine(string(body), "ll-cue") {
			found = true
		}
	}
	require.True(t, found, "cue missing in low-latency muxer output")
}

func TestMuxerWebVTTSaveToDisk(t *testing.T) {
	dir, err := os.MkdirTemp("", "gohlslib-vtt")
	require.NoError(t, err)
	defer os.RemoveAll(dir)

	videoTrack := newTestVideoTrack()

	m := &gohlslib.Muxer{
		Variant:            gohlslib.MuxerVariantFMP4,
		SegmentCount:       3,
		SegmentMinDuration: 1 * time.Second,
		Tracks:             []*gohlslib.Track{videoTrack, testSubtitleTrackEN},
		Directory:          dir,
	}
	require.NoError(t, m.Start())

	writeH264IDR(t, m, videoTrack, 0)
	require.NoError(t, m.WriteWebVTT(testSubtitleTrackEN, 200*time.Millisecond, 500*time.Millisecond, "disk-cue"))
	writeH264IDR(t, m, videoTrack, 1)
	writeH264IDR(t, m, videoTrack, 2)
	m.Close()

	// subtitle media playlist was written to disk
	pl, err := os.ReadFile(filepath.Join(dir, "subtitles2_stream.m3u8"))
	require.NoError(t, err)
	require.Contains(t, string(pl), "#EXT-X-VERSION:6")

	// every referenced WebVTT segment exists on disk
	for _, path := range regexp.MustCompile(`\S+_seg\d+\.vtt`).FindAllString(string(pl), -1) {
		body, err := os.ReadFile(filepath.Join(dir, path))
		require.NoError(t, err)
		require.Contains(t, string(body), "WEBVTT")
	}

	// the multivariant playlist references the subtitle group
	index, err := os.ReadFile(filepath.Join(dir, "index.m3u8"))
	require.NoError(t, err)
	require.Contains(t, string(index), `TYPE=SUBTITLES`)
}
