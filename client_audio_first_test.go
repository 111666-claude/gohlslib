package gohlslib

import (
	"context"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/bluenviron/gohlslib/v2/pkg/codecs"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/mpeg4audio"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/mpegts"
	tscodecs "github.com/bluenviron/mediacommon/v2/pkg/formats/mpegts/codecs"
	"github.com/stretchr/testify/require"
)

const (
	// enough audio to cover a realistic pre-roll before the first video frame,
	// but kept small so the segments together contain well under a second of
	// media and the VOD test drains quickly
	audioFirstReproPackets = 60
	// more packets than the MPEG-TS sample queue can hold, used to prove that a
	// pre-leading-track backlog no longer deadlocks the reader
	audioFirstOverflowPackets = clientMPEGTSSampleQueueSize + 5
)

var audioFirstAACConfig = mpeg4audio.AudioSpecificConfig{
	Type:          2,
	SampleRate:    44100,
	ChannelConfig: 2,
	ChannelCount:  2,
}

// writeDualTrackSegment writes a dual-track MPEG-TS segment. When audioFirst is
// true, all audio packets are written before the first video packet.
func writeDualTrackSegment(t *testing.T, w http.ResponseWriter, baseDTS int64, audioFirst bool) {
	h264Track := &mpegts.Track{Codec: &tscodecs.H264{}}
	mpeg4audioTrack := &mpegts.Track{Codec: &tscodecs.MPEG4Audio{Config: audioFirstAACConfig}}
	mw := &mpegts.Writer{W: w, Tracks: []*mpegts.Track{h264Track, mpeg4audioTrack}}
	err := mw.Initialize()
	require.NoError(t, err)

	writeAudio := func() {
		for i := 0; i < audioFirstReproPackets; i++ {
			err2 := mw.WriteMPEG4Audio(
				mpeg4audioTrack,
				baseDTS+int64(i)*1024*90000/44100,
				[][]byte{{byte(i + 1), 2, 3, 4}},
			)
			require.NoError(t, err2)
		}
	}

	writeVideo := func() {
		err2 := mw.WriteH264(
			h264Track,
			baseDTS+90000,
			baseDTS+90000,
			[][]byte{{7, 1, 2, 3}, {8}, {5}}, // SPS, PPS, IDR
		)
		require.NoError(t, err2)

		err2 = mw.WriteH264(
			h264Track,
			baseDTS+90000+90000/30,
			baseDTS+90000+90000/30,
			[][]byte{{1, 4, 5, 6}},
		)
		require.NoError(t, err2)
	}

	if audioFirst {
		writeAudio()
		writeVideo()
	} else {
		writeVideo()
		writeAudio()
	}
}

func writeAudioOnlySegment(t *testing.T, w http.ResponseWriter, baseDTS int64) {
	mpeg4audioTrack := &mpegts.Track{Codec: &tscodecs.MPEG4Audio{Config: audioFirstAACConfig}}
	mw := &mpegts.Writer{W: w, Tracks: []*mpegts.Track{mpeg4audioTrack}}
	err := mw.Initialize()
	require.NoError(t, err)

	for i := 0; i < audioFirstReproPackets; i++ {
		err2 := mw.WriteMPEG4Audio(
			mpeg4audioTrack,
			baseDTS+int64(i)*1024*90000/44100,
			[][]byte{{byte(i + 1), 2, 3, 4}},
		)
		require.NoError(t, err2)
	}
}

func writeVideoOnlySegment(t *testing.T, w http.ResponseWriter, baseDTS int64) {
	h264Track := &mpegts.Track{Codec: &tscodecs.H264{}}
	mw := &mpegts.Writer{W: w, Tracks: []*mpegts.Track{h264Track}}
	err := mw.Initialize()
	require.NoError(t, err)

	err = mw.WriteH264(
		h264Track,
		baseDTS,
		baseDTS,
		[][]byte{{7, 1, 2, 3}, {8}, {5}},
	)
	require.NoError(t, err)
}

func TestClientTrackOrderingMPEGTS(t *testing.T) {
	for _, ca := range []struct {
		name   string
		tracks string // "video+audio", "audio", "video"
		order  string // "audio-first", "video-first"
	}{
		{"dual_audio_first", "video+audio", "audio-first"},
		{"dual_video_first", "video+audio", "video-first"},
		{"audio_only", "audio", ""},
		{"video_only", "video", ""},
	} {
		for _, mode := range []string{"live", "vod"} {
			t.Run(ca.name+"_"+mode, func(t *testing.T) {
				httpServ := &http.Server{
					Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						switch {
						case r.Method == http.MethodGet && r.URL.Path == "/stream.m3u8":
							w.Header().Set("Content-Type", `application/vnd.apple.mpegurl`)
							playlist := "#EXTM3U\n" +
								"#EXT-X-VERSION:3\n" +
								"#EXT-X-TARGETDURATION:2\n" +
								"#EXT-X-MEDIA-SEQUENCE:0\n"
							if mode == "vod" {
								playlist += "#EXT-X-PLAYLIST-TYPE:VOD\n"
							}
							playlist += "#EXTINF:1,\nsegment1.ts\n" +
								"#EXTINF:1,\nsegment2.ts\n"
							if mode == "vod" {
								playlist += "#EXT-X-ENDLIST\n"
							}
							w.Write([]byte(playlist))

						case r.Method == http.MethodGet && r.URL.Path == "/segment1.ts":
							w.Header().Set("Content-Type", `video/MP2T`)
							switch ca.tracks {
							case "video+audio":
								writeDualTrackSegment(t, w, 0, ca.order == "audio-first")
							case "audio":
								writeAudioOnlySegment(t, w, 0)
							case "video":
								writeVideoOnlySegment(t, w, 0)
							}

						case r.Method == http.MethodGet && r.URL.Path == "/segment2.ts":
							w.Header().Set("Content-Type", `video/MP2T`)
							// continuous timestamps strictly after segment 1, whose
							// last audio sample is at ~123251
							switch ca.tracks {
							case "video+audio":
								writeDualTrackSegment(t, w, 124000, ca.order == "audio-first")
							case "audio":
								writeAudioOnlySegment(t, w, 124000)
							case "video":
								writeVideoOnlySegment(t, w, 94000)
							}
						}
					}),
				}

				ln, err := net.Listen("tcp", "localhost:5781")
				require.NoError(t, err)

				go httpServ.Serve(ln)
				defer httpServ.Shutdown(context.Background())

				gotVideo := make(chan struct{})
				gotAudio := make(chan struct{})
				firstAudio := make(chan [][]byte, 1)

				var mu sync.Mutex
				audioPTSOrder := make([]int64, 0, audioFirstReproPackets*2)
				videoDTSOrder := make([]int64, 0)
				videoCount := 0
				audioCount := 0

				var c *Client
				c = &Client{
					URI:           "http://localhost:5781/stream.m3u8",
					StartDistance: 1,
					OnTracks: func(tracks []*Track) error {
						for _, track := range tracks {
							switch track.Codec.(type) {
							case *codecs.H264:
								c.OnDataH26x(track, func(_ int64, dts int64, _ [][]byte) {
									mu.Lock()
									videoDTSOrder = append(videoDTSOrder, dts)
									videoCount++
									mu.Unlock()
									select {
									case <-gotVideo:
									default:
										close(gotVideo)
									}
								})

							case *codecs.MPEG4Audio:
								c.OnDataMPEG4Audio(track, func(pts int64, aus [][]byte) {
									mu.Lock()
									if audioCount == 0 {
										firstAudio <- aus
									}
									audioPTSOrder = append(audioPTSOrder, pts)
									audioCount++
									mu.Unlock()
									select {
									case <-gotAudio:
									default:
										close(gotAudio)
									}
								})
							}
						}
						return nil
					},
				}

				err = c.Start()
				require.NoError(t, err)

				start := time.Now()

				if ca.tracks != "audio" {
					select {
					case <-gotVideo:
					case <-time.After(5 * time.Second):
						t.Fatal("timeout waiting for first video frame")
					}
				}

				if ca.tracks != "video" {
					select {
					case <-gotAudio:
					case <-time.After(5 * time.Second):
						t.Fatal("timeout waiting for first audio packet")
					}

					// the first audio packet must be preserved, even when it
					// arrived before the first video frame
					select {
					case aus := <-firstAudio:
						require.Equal(t, [][]byte{{1, 2, 3, 4}}, aus)
					default:
						t.Fatal("first audio packet was dropped")
					}
				}

				if mode == "vod" {
					done := make(chan error, 1)
					go func() { done <- c.Wait2() }()
					select {
					case err2 := <-done:
						require.Equal(t, ErrClientEOS, err2)
					case <-time.After(15 * time.Second):
						t.Fatal("VOD stream did not terminate")
					}
				} else {
					c.Close()
				}

				// snapshot all callback state after the stream has quiesced
				mu.Lock()
				finalVideoCount := videoCount
				finalAudioCount := audioCount
				finalAudioPTS := append([]int64(nil), audioPTSOrder...)
				finalVideoDTS := append([]int64(nil), videoDTSOrder...)
				mu.Unlock()

				t.Logf("%s/%s: video=%d audio=%d first-samples-in=%v",
					ca.name, mode, finalVideoCount, finalAudioCount, time.Since(start))

				if mode == "vod" {
					// once the whole VOD stream has been consumed, every packet
					// of both segments must have been delivered: nothing dropped
					switch ca.tracks {
					case "video+audio":
						require.Equal(t, audioFirstReproPackets*2, finalAudioCount)
						require.Equal(t, 4, finalVideoCount) // 2 frames per segment
					case "audio":
						require.Equal(t, audioFirstReproPackets*2, finalAudioCount)
					case "video":
						require.Equal(t, 2, finalVideoCount)
					}
				}

				// timestamp checks on everything received so far
				if ca.tracks != "video" {
					// audio PTS must be monotonically non-decreasing
					for i := 1; i < len(finalAudioPTS); i++ {
						require.GreaterOrEqual(t, finalAudioPTS[i], finalAudioPTS[i-1],
							"audio PTS went backwards at index %d", i)
					}
				}

				if ca.tracks != "audio" {
					// video DTS must be monotonically increasing
					for i := 1; i < len(finalVideoDTS); i++ {
						require.Greater(t, finalVideoDTS[i], finalVideoDTS[i-1],
							"video DTS did not increase at index %d", i)
					}
				}
			})
		}
	}
}

// TestClientAudioFirstQueueOverflowMPEGTS feeds more audio packets than the
// MPEG-TS sample queue can hold before the first video frame. Before the fix
// this deadlocked the reader (the segment could never finish being read); now
// playback starts, every packet is delivered and the VOD stream terminates.
func TestClientAudioFirstQueueOverflowMPEGTS(t *testing.T) {
	writeSegment := func(w http.ResponseWriter, baseDTS int64) {
		h264Track := &mpegts.Track{Codec: &tscodecs.H264{}}
		mpeg4audioTrack := &mpegts.Track{Codec: &tscodecs.MPEG4Audio{Config: audioFirstAACConfig}}
		mw := &mpegts.Writer{W: w, Tracks: []*mpegts.Track{h264Track, mpeg4audioTrack}}
		require.NoError(t, mw.Initialize())

		for i := 0; i < audioFirstOverflowPackets; i++ {
			require.NoError(t, mw.WriteMPEG4Audio(
				mpeg4audioTrack,
				baseDTS+int64(i)*1024*90000/44100,
				[][]byte{{byte(i + 1), 2, 3, 4}},
			))
		}

		require.NoError(t, mw.WriteH264(
			h264Track,
			baseDTS+90000,
			baseDTS+90000,
			[][]byte{{7, 1, 2, 3}, {8}, {5}},
		))
	}

	httpServ := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodGet && r.URL.Path == "/stream.m3u8":
				w.Header().Set("Content-Type", `application/vnd.apple.mpegurl`)
				w.Write([]byte("#EXTM3U\n" +
					"#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:0\n" +
					"#EXT-X-PLAYLIST-TYPE:VOD\n#EXTINF:1,\nsegment1.ts\n#EXT-X-ENDLIST\n"))

			case r.Method == http.MethodGet && r.URL.Path == "/segment1.ts":
				w.Header().Set("Content-Type", `video/MP2T`)
				writeSegment(w, 0)
			}
		}),
	}

	ln, err := net.Listen("tcp", "localhost:5783")
	require.NoError(t, err)
	go httpServ.Serve(ln)
	defer httpServ.Shutdown(context.Background())

	gotVideo := make(chan struct{})
	gotAudio := make(chan struct{})
	firstAudio := make(chan [][]byte, 1)

	var mu sync.Mutex
	audioCount := 0
	videoCount := 0

	var c *Client
	c = &Client{
		URI: "http://localhost:5783/stream.m3u8",
		OnTracks: func(tracks []*Track) error {
			for _, track := range tracks {
				switch track.Codec.(type) {
				case *codecs.H264:
					c.OnDataH26x(track, func(_ int64, _ int64, _ [][]byte) {
						mu.Lock()
						videoCount++
						mu.Unlock()
						select {
						case <-gotVideo:
						default:
							close(gotVideo)
						}
					})

				case *codecs.MPEG4Audio:
					c.OnDataMPEG4Audio(track, func(_ int64, aus [][]byte) {
						mu.Lock()
						if audioCount == 0 {
							firstAudio <- aus
						}
						audioCount++
						mu.Unlock()
						select {
						case <-gotAudio:
						default:
							close(gotAudio)
						}
					})
				}
			}
			return nil
		},
	}

	require.NoError(t, c.Start())

	// both tracks must start; before the fix this never happened
	select {
	case <-gotVideo:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for first video frame")
	}
	select {
	case <-gotAudio:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for first audio packet")
	}

	// and the first of the >100 backlogged packets must survive
	select {
	case aus := <-firstAudio:
		require.Equal(t, [][]byte{{1, 2, 3, 4}}, aus)
	default:
		t.Fatal("first audio packet was dropped")
	}

	// the VOD stream must terminate instead of hanging
	done := make(chan error, 1)
	go func() { done <- c.Wait2() }()
	select {
	case err2 := <-done:
		require.Equal(t, ErrClientEOS, err2)
	case <-time.After(15 * time.Second):
		t.Fatal("VOD stream did not terminate")
	}

	mu.Lock()
	finalAudio := audioCount
	finalVideo := videoCount
	mu.Unlock()

	require.Equal(t, audioFirstOverflowPackets, finalAudio)
	require.Equal(t, 1, finalVideo)
}

// TestClientInterleavedMPEGTS writes audio both before and between video
// frames, then verifies that audio samples stay in timestamp order (none of
// the pre-video backlog is delivered after a later audio sample) and that
// every packet reaches the callback.
func TestClientInterleavedMPEGTS(t *testing.T) {
	const perBurst = 30

	httpServ := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/stream.m3u8":
				w.Header().Set("Content-Type", `application/vnd.apple.mpegurl`)
				w.Write([]byte("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:2\n" +
					"#EXT-X-MEDIA-SEQUENCE:0\n#EXT-X-PLAYLIST-TYPE:VOD\n" +
					"#EXTINF:1,\nsegment1.ts\n#EXT-X-ENDLIST\n"))

			case "/segment1.ts":
				w.Header().Set("Content-Type", `video/MP2T`)
				h264Track := &mpegts.Track{Codec: &tscodecs.H264{}}
				mpeg4audioTrack := &mpegts.Track{Codec: &tscodecs.MPEG4Audio{Config: audioFirstAACConfig}}
				mw := &mpegts.Writer{W: w, Tracks: []*mpegts.Track{h264Track, mpeg4audioTrack}}
				require.NoError(t, mw.Initialize())

				writeBurst := func(offset int) {
					for i := 0; i < perBurst; i++ {
						idx := offset + i
						require.NoError(t, mw.WriteMPEG4Audio(
							mpeg4audioTrack,
							int64(idx)*1024*90000/44100,
							[][]byte{{byte(idx + 1), 2, 3, 4}},
						))
					}
				}

				writeBurst(0) // audio before first video
				require.NoError(t, mw.WriteH264(h264Track, 90000, 90000,
					[][]byte{{7, 1, 2, 3}, {8}, {5}}))
				writeBurst(perBurst) // audio between video frames
				require.NoError(t, mw.WriteH264(h264Track, 93000, 93000,
					[][]byte{{1, 4, 5, 6}}))
				writeBurst(2 * perBurst) // audio after video
			}
		}),
	}

	ln, err := net.Listen("tcp", "localhost:5784")
	require.NoError(t, err)
	go httpServ.Serve(ln)
	defer httpServ.Shutdown(context.Background())

	gotVideo := make(chan struct{})
	gotAudio := make(chan struct{})
	firstAudio := make(chan [][]byte, 1)

	var mu sync.Mutex
	audioPTS := make([]int64, 0, perBurst*3)
	audioCount := 0
	videoCount := 0

	var c *Client
	c = &Client{
		URI: "http://localhost:5784/stream.m3u8",
		OnTracks: func(tracks []*Track) error {
			for _, track := range tracks {
				switch track.Codec.(type) {
				case *codecs.H264:
					c.OnDataH26x(track, func(_ int64, _ int64, _ [][]byte) {
						mu.Lock()
						videoCount++
						mu.Unlock()
						select {
						case <-gotVideo:
						default:
							close(gotVideo)
						}
					})

				case *codecs.MPEG4Audio:
					c.OnDataMPEG4Audio(track, func(pts int64, aus [][]byte) {
						mu.Lock()
						if audioCount == 0 {
							firstAudio <- aus
						}
						audioPTS = append(audioPTS, pts)
						audioCount++
						mu.Unlock()
						select {
						case <-gotAudio:
						default:
							close(gotAudio)
						}
					})
				}
			}
			return nil
		},
	}

	require.NoError(t, c.Start())

	select {
	case <-gotVideo:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for first video frame")
	}
	select {
	case <-gotAudio:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for first audio packet")
	}
	select {
	case aus := <-firstAudio:
		require.Equal(t, [][]byte{{1, 2, 3, 4}}, aus)
	default:
		t.Fatal("first audio packet was dropped")
	}

	done := make(chan error, 1)
	go func() { done <- c.Wait2() }()
	select {
	case err2 := <-done:
		require.Equal(t, ErrClientEOS, err2)
	case <-time.After(15 * time.Second):
		t.Fatal("VOD stream did not terminate")
	}

	mu.Lock()
	defer mu.Unlock()

	require.Equal(t, perBurst*3, audioCount)
	require.Equal(t, 2, videoCount)

	// every audio sample delivered exactly once, in strictly increasing PTS order
	require.Len(t, audioPTS, perBurst*3)
	for i := 1; i < len(audioPTS); i++ {
		require.Greater(t, audioPTS[i], audioPTS[i-1],
			"audio PTS not increasing at index %d", i)
	}
}
