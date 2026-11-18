package gohlslib

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/asticode/go-astits"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
	"github.com/stretchr/testify/require"

	"github.com/bluenviron/gohlslib/v2/pkg/codecs"
)

func buildID3v23Tag(frames ...[2]string) []byte {
	var body []byte
	for _, f := range frames {
		v := append([]byte{3}, []byte(f[1])...) // UTF-8 text
		body = append(body, []byte(f[0])...)
		body = append(body, byte(len(v)>>24), byte(len(v)>>16), byte(len(v)>>8), byte(len(v)))
		body = append(body, 0, 0)
		body = append(body, v...)
	}

	tag := []byte{'I', 'D', '3', 3, 0, 0,
		byte(len(body) >> 21), byte(len(body) >> 14), byte(len(body) >> 7 & 0x7F), byte(len(body) & 0x7F)}
	return append(tag, body...)
}

func TestParseID3Tag(t *testing.T) {
	t.Run("v2.3 title and artist", func(t *testing.T) {
		meta, err := parseID3Tag(buildID3v23Tag(
			[2]string{"TIT2", "Song A"},
			[2]string{"TPE1", "Artist A"},
		))
		require.NoError(t, err)
		require.Equal(t, &ID3Metadata{Title: "Song A", Artist: "Artist A"}, meta)
	})

	t.Run("v2.4 syncsafe frame size", func(t *testing.T) {
		v := append([]byte{3}, []byte("Song B")...)
		body := []byte("TIT2")
		body = append(body, 0, 0, 0, byte(len(v))) // syncsafe size
		body = append(body, 0, 0)
		body = append(body, v...)
		tag := []byte{'I', 'D', '3', 4, 0, 0, 0, 0, 0, byte(len(body))}
		tag = append(tag, body...)

		meta, err := parseID3Tag(tag)
		require.NoError(t, err)
		require.Equal(t, &ID3Metadata{Title: "Song B"}, meta)
	})

	t.Run("unknown frames are skipped", func(t *testing.T) {
		meta, err := parseID3Tag(buildID3v23Tag(
			[2]string{"APIC", "not-really-a-picture"},
			[2]string{"TIT2", "Song C"},
			[2]string{"TXXX", "something"},
		))
		require.NoError(t, err)
		require.Equal(t, &ID3Metadata{Title: "Song C"}, meta)
	})

	t.Run("invalid declared length", func(t *testing.T) {
		tag := buildID3v23Tag([2]string{"TIT2", "Song A"})
		tag[9] = 0x7F // declared size much bigger than available data

		_, err := parseID3Tag(tag)
		require.EqualError(t, err, fmt.Sprintf(
			"invalid ID3 length: tag declares %d bytes, but only %d are available", 10+0x7F, len(tag)))
	})

	t.Run("invalid syncsafe size", func(t *testing.T) {
		tag := buildID3v23Tag([2]string{"TIT2", "Song A"})
		tag[6] = 0x80

		_, err := parseID3Tag(tag)
		require.EqualError(t, err, "invalid ID3 size: invalid syncsafe integer")
	})

	t.Run("unsupported version", func(t *testing.T) {
		tag := buildID3v23Tag([2]string{"TIT2", "Song A"})
		tag[3] = 2

		_, err := parseID3Tag(tag)
		require.EqualError(t, err, "unsupported ID3 version: 2.2")
	})

	t.Run("invalid magic", func(t *testing.T) {
		_, err := parseID3Tag([]byte("not an id3 tag at all"))
		require.EqualError(t, err, "invalid ID3 magic")
	})

	t.Run("truncated frame keeps previous frames", func(t *testing.T) {
		tag := buildID3v23Tag(
			[2]string{"TIT2", "Song A"},
			[2]string{"TPE1", "Artist A"},
		)
		// cut the tag in the middle of the TPE1 frame and fix the declared size
		tag = tag[:len(tag)-5]
		size := len(tag) - 10
		tag[6] = byte(size >> 21)
		tag[7] = byte(size >> 14)
		tag[8] = byte(size >> 7)
		tag[9] = byte(size)

		meta, err := parseID3Tag(tag)
		require.NoError(t, err)
		require.Equal(t, &ID3Metadata{Title: "Song A"}, meta)
	})

	t.Run("unsynchronisation", func(t *testing.T) {
		v := []byte{0, 0xFF, 0x00, 0xE0, 'A'} // ISO-8859-1 text containing 0xFF 0xE0, unsynchronised
		body := []byte("TIT2")
		body = append(body, 0, 0, 0, byte(len(v)))
		body = append(body, 0, 0)
		body = append(body, v...)

		tag := []byte{'I', 'D', '3', 3, 0, 0x80, 0, 0, 0, byte(len(body))}
		tag = append(tag, body...)

		meta, err := parseID3Tag(tag)
		require.NoError(t, err)
		require.Equal(t, "ÿàA", meta.Title)
	})
}

func TestSortID3EventsByPTS(t *testing.T) {
	events := []id3Event{
		{pts: 90000, title: "B"},
		{pts: 45000, title: "A"},
		{pts: 180000, title: "D"},
		{pts: 135000, title: "C"},
	}
	sortID3EventsByPTS(events)
	require.Equal(t, []id3Event{
		{pts: 45000, title: "A"},
		{pts: 90000, title: "B"},
		{pts: 135000, title: "C"},
		{pts: 180000, title: "D"},
	}, events)
}

func TestSortID3EventsByPTSWrap(t *testing.T) {
	events := []id3Event{
		{pts: 45000, title: "after wrap"},
		{pts: mpegtsMaxPTS - 44999, title: "before wrap"},
	}
	sortID3EventsByPTS(events)
	require.Equal(t, []id3Event{
		{pts: mpegtsMaxPTS - 44999, title: "before wrap"},
		{pts: 45000, title: "after wrap"},
	}, events)
}

// hand-craft a 188-byte TS packet with payload only (no adaptation field).
func testTSPacket(pid uint16, pusi bool, continuity uint8, payload []byte) []byte {
	pkt := make([]byte, mpegtsPacketSize)
	pkt[0] = mpegtsSyncByte
	pkt[1] = byte(pid>>8) & 0x1F
	if pusi {
		pkt[1] |= 0x40
	}
	pkt[2] = byte(pid)
	pkt[3] = 0x10 | continuity
	copy(pkt[4:], payload)
	return pkt
}

func testTSPAT(pmtPID uint16) []byte {
	sec := []byte{
		0x00,       // table id
		0xB0, 0x0D, // section length = 13
		0x00, 0x01, // transport stream id
		0xC1, 0x00, 0x00, // version, section numbers
		0x00, 0x01, // program number
		byte(0xE0 | (pmtPID >> 8)), byte(pmtPID), // PMT PID
		0x00, 0x00, 0x00, 0x00, // CRC (not checked)
	}
	return testTSPacket(0, true, 0, append([]byte{0}, sec...))
}

func testTSPMT(pmtPID uint16, id3PID uint16, streamType byte) []byte {
	sec := []byte{
		0x02,       // table id
		0xB0, 0x12, // section length = 18
		0x00, 0x01, // program number
		0xC1, 0x00, 0x00, // version, section numbers
		0xE0, 0x00, // PCR PID
		0xF0, 0x00, // program info length
		streamType, byte(0xE0 | (id3PID >> 8)), byte(id3PID), // elementary stream
		0xF0, 0x00, // ES info length
		0x00, 0x00, 0x00, 0x00, // CRC (not checked)
	}
	return testTSPacket(pmtPID, true, 0, append([]byte{0}, sec...))
}

func testPESHeader(streamID byte, pesLen int, pts int64) []byte {
	out := []byte{0x00, 0x00, 0x01, streamID, byte(pesLen >> 8), byte(pesLen),
		0x80, 0x80, 0x05, // marker, PTS only, header data length
		0x20 | byte((pts>>30)&0x07)<<1 | 1,
		byte(pts >> 22),
		byte((pts>>15)&0x7F)<<1 | 1,
		byte(pts >> 7),
		byte(pts&0x7F)<<1 | 1,
	}
	return out
}

// build a sequence of TS packets carrying a single ID3 PES.
func testID3PESPackets(id3PID uint16, pts int64, tag []byte, firstContinuity uint8) []byte {
	pes := testPESHeader(0xBD, 3+5+len(tag), pts)
	pes = append(pes, tag...)

	var out []byte
	cc := firstContinuity
	for len(pes) > 0 {
		n := min(184, len(pes))
		out = append(out, testTSPacket(id3PID, len(out) == 0, cc, pes[:n])...)
		pes = pes[n:]
		cc = (cc + 1) & 0x0F
	}
	return out
}

func testID3Extractor(t *testing.T) (*clientID3ExtractorMPEGTS, *[]error) {
	errs := &[]error{}
	e := &clientID3ExtractorMPEGTS{
		onDecodeError: func(err error) {
			*errs = append(*errs, err)
		},
	}
	e.initialize()
	return e, errs
}

func TestClientID3Extractor(t *testing.T) {
	const (
		pmtPID = 0x101
		id3PID = 0x102
	)

	t.Run("basic", func(t *testing.T) {
		e, errs := testID3Extractor(t)

		var payload []byte
		payload = append(payload, testTSPAT(pmtPID)...)
		payload = append(payload, testTSPMT(pmtPID, id3PID, streamTypeMetadata)...)
		payload = append(payload, testID3PESPackets(id3PID, 45000,
			buildID3v23Tag([2]string{"TIT2", "Song A"}), 0)...)

		events := e.processSegment(payload)
		require.Equal(t, []id3Event{{pts: 45000, title: "Song A"}}, events)
		require.Empty(t, *errs)
	})

	t.Run("private data stream with registration descriptor", func(t *testing.T) {
		e, errs := testID3Extractor(t)

		sec := []byte{
			0x02,       // table id
			0xB0, 0x19, // section length = 25
			0x00, 0x01, // program number
			0xC1, 0x00, 0x00,
			0xE0, 0x00, // PCR PID
			0xF0, 0x00, // program info length
			streamTypePrivateData, 0xE0 | byte(id3PID>>8), byte(id3PID & 0xFF), // elementary stream
			0xF0, 0x06, // ES info length
			0x05, 0x04, 'I', 'D', '3', ' ', // registration descriptor
			0x00, 0x00, 0x00, 0x00, // CRC
		}

		var payload []byte
		payload = append(payload, testTSPAT(pmtPID)...)
		payload = append(payload, testTSPacket(pmtPID, true, 0, append([]byte{0}, sec...))...)
		payload = append(payload, testID3PESPackets(id3PID, 90000,
			buildID3v23Tag([2]string{"TIT2", "Song B"}), 0)...)

		events := e.processSegment(payload)
		require.Equal(t, []id3Event{{pts: 90000, title: "Song B"}}, events)
		require.Empty(t, *errs)
	})

	t.Run("across TS packets", func(t *testing.T) {
		e, errs := testID3Extractor(t)

		// a tag bigger than a single TS packet payload
		tag := buildID3v23Tag([2]string{"TIT2", strings.Repeat("a", 1000)})

		var payload []byte
		payload = append(payload, testTSPAT(pmtPID)...)
		payload = append(payload, testTSPMT(pmtPID, id3PID, streamTypeMetadata)...)
		payload = append(payload, testID3PESPackets(id3PID, 45000, tag, 0)...)

		events := e.processSegment(payload)
		require.Equal(t, []id3Event{{pts: 45000, title: strings.Repeat("a", 1000)}}, events)
		require.Empty(t, *errs)
	})

	t.Run("across segments", func(t *testing.T) {
		e, errs := testID3Extractor(t)

		tag := buildID3v23Tag([2]string{"TIT2", strings.Repeat("b", 1000)})

		var payload []byte
		payload = append(payload, testTSPAT(pmtPID)...)
		payload = append(payload, testTSPMT(pmtPID, id3PID, streamTypeMetadata)...)
		payload = append(payload, testID3PESPackets(id3PID, 45000, tag, 0)...)

		// split in the middle of the ID3 PES
		split := 4 * mpegtsPacketSize
		events := e.processSegment(payload[:split])
		require.Empty(t, events)

		events = e.processSegment(payload[split:])
		require.Equal(t, []id3Event{{pts: 45000, title: strings.Repeat("b", 1000)}}, events)
		require.Empty(t, *errs)
	})

	t.Run("truncated PES", func(t *testing.T) {
		e, errs := testID3Extractor(t)

		tag := buildID3v23Tag([2]string{"TIT2", strings.Repeat("c", 1000)})

		var payload []byte
		payload = append(payload, testTSPAT(pmtPID)...)
		payload = append(payload, testTSPMT(pmtPID, id3PID, streamTypeMetadata)...)
		pesPackets := testID3PESPackets(id3PID, 45000, tag, 0)
		// drop the last packet of the PES
		payload = append(payload, pesPackets[:len(pesPackets)-mpegtsPacketSize]...)

		events := e.processSegment(payload)
		require.Empty(t, events)
		require.Empty(t, *errs) // nothing wrong yet: the PES may continue in the next segment

		// a new PES starts before the previous one is complete
		payload = testID3PESPackets(id3PID, 90000, buildID3v23Tag([2]string{"TIT2", "next"}), 5)
		events = e.processSegment(payload)
		require.Equal(t, []id3Event{{pts: 90000, title: "next"}}, events)
		require.NotEmpty(t, *errs)
		require.Contains(t, (*errs)[0].Error(), "truncated PES dropped")
	})

	t.Run("invalid ID3 length", func(t *testing.T) {
		e, errs := testID3Extractor(t)

		tag := buildID3v23Tag([2]string{"TIT2", "Song A"})
		tag[9] = 0x7F

		var payload []byte
		payload = append(payload, testTSPAT(pmtPID)...)
		payload = append(payload, testTSPMT(pmtPID, id3PID, streamTypeMetadata)...)
		payload = append(payload, testID3PESPackets(id3PID, 45000, tag, 0)...)

		events := e.processSegment(payload)
		require.Empty(t, events)
		require.NotEmpty(t, *errs)
		require.Contains(t, (*errs)[0].Error(), "invalid ID3 length")
	})

	t.Run("PES without PTS", func(t *testing.T) {
		e, errs := testID3Extractor(t)

		tag := buildID3v23Tag([2]string{"TIT2", "Song A"})
		pes := []byte{0x00, 0x00, 0x01, 0xBD, byte((3 + len(tag)) >> 8), byte(3 + len(tag)),
			0x80, 0x00, 0x00} // no PTS
		pes = append(pes, tag...)

		var payload []byte
		payload = append(payload, testTSPAT(pmtPID)...)
		payload = append(payload, testTSPMT(pmtPID, id3PID, streamTypeMetadata)...)
		payload = append(payload, testTSPacket(id3PID, true, 0, pes)...)

		events := e.processSegment(payload)
		require.Empty(t, events)
		require.NotEmpty(t, *errs)
		require.Contains(t, (*errs)[0].Error(), "PES without PTS")
	})

	t.Run("PES size limit", func(t *testing.T) {
		e, errs := testID3Extractor(t)

		var payload []byte
		payload = append(payload, testTSPAT(pmtPID)...)
		payload = append(payload, testTSPMT(pmtPID, id3PID, streamTypeMetadata)...)

		// unbounded PES (length = 0) that grows past the limit
		pes := []byte{0x00, 0x00, 0x01, 0xBD, 0x00, 0x00, 0x80, 0x80, 0x05}
		pes = append(pes, testPESHeader(0xBD, 0, 0)[9:]...)
		payload = append(payload, testTSPacket(id3PID, true, 0, pes)...)

		var filler []byte
		cc := uint8(1)
		for len(filler) < clientMaxID3PESSize+clientMaxID3PESSize/10 {
			filler = append(filler, testTSPacket(id3PID, false, cc, make([]byte, 184))...)
			cc = (cc + 1) & 0x0F
		}
		payload = append(payload, filler...)

		events := e.processSegment(payload)
		require.Empty(t, events)
		require.NotEmpty(t, *errs)
		require.Contains(t, (*errs)[0].Error(), "exceeds maximum")

		// the extractor recovers at the next PES
		events = e.processSegment(testID3PESPackets(id3PID, 90000,
			buildID3v23Tag([2]string{"TIT2", "recovered"}), cc))
		require.Equal(t, []id3Event{{pts: 90000, title: "recovered"}}, events)
	})

	t.Run("continuity error drops partial PES", func(t *testing.T) {
		e, errs := testID3Extractor(t)

		tag := buildID3v23Tag([2]string{"TIT2", strings.Repeat("d", 1000)})

		var payload []byte
		payload = append(payload, testTSPAT(pmtPID)...)
		payload = append(payload, testTSPMT(pmtPID, id3PID, streamTypeMetadata)...)
		pesPackets := testID3PESPackets(id3PID, 45000, tag, 0)
		// keep only the first two packets, then continue with a wrong continuity counter
		payload = append(payload, pesPackets[:2*mpegtsPacketSize]...)
		payload = append(payload, testID3PESPackets(id3PID, 90000,
			buildID3v23Tag([2]string{"TIT2", "next"}), 9)...)

		events := e.processSegment(payload)
		require.Equal(t, []id3Event{{pts: 90000, title: "next"}}, events)
		require.NotEmpty(t, *errs)
		require.Contains(t, (*errs)[0].Error(), "continuity error")
	})

	t.Run("encrypted segment", func(t *testing.T) {
		e, errs := testID3Extractor(t)

		// garbage payload (e.g. an AES-128 encrypted segment)
		garbage := make([]byte, 10*mpegtsPacketSize)
		for i := range garbage {
			garbage[i] = byte(i*37 + 11)
		}
		// make sure no sync byte is present
		for i := range garbage {
			if garbage[i] == mpegtsSyncByte {
				garbage[i] = 0x46
			}
		}

		events := e.processSegment(garbage)
		require.Empty(t, events)
		require.NotEmpty(t, *errs)
		require.Contains(t, (*errs)[len(*errs)-1].Error(), "no valid MPEG-TS packets found")
	})

	t.Run("scrambled packets are skipped", func(t *testing.T) {
		e, errs := testID3Extractor(t)

		var payload []byte
		payload = append(payload, testTSPAT(pmtPID)...)
		payload = append(payload, testTSPMT(pmtPID, id3PID, streamTypeMetadata)...)
		pkt := testID3PESPackets(id3PID, 45000, buildID3v23Tag([2]string{"TIT2", "Song A"}), 0)
		pkt[3] |= 0x80 // transport_scrambling_control != 0
		payload = append(payload, pkt...)

		events := e.processSegment(payload)
		require.Empty(t, events)
		require.NotEmpty(t, *errs)
		require.Contains(t, (*errs)[0].Error(), "scrambled")
	})
}

func TestClientID3EndToEnd(t *testing.T) {
	const id3PID = 257

	writeSegment := func(w http.ResponseWriter, h264PTS int64, id3PTS int64, id3Tag []byte) {
		w.Header().Set("Content-Type", `video/MP2T`)

		var buf bytes.Buffer
		mux := astits.NewMuxer(context.Background(), &buf)

		err := mux.AddElementaryStream(astits.PMTElementaryStream{
			ElementaryPID: 256,
			StreamType:    astits.StreamTypeH264Video,
		})
		require.NoError(t, err)

		err = mux.AddElementaryStream(astits.PMTElementaryStream{
			ElementaryPID: id3PID,
			StreamType:    astits.StreamTypeMetadata,
		})
		require.NoError(t, err)

		mux.SetPCRPID(256)

		au, err := h264.AnnexB([][]byte{
			{7, 1, 2, 3}, // SPS
			{8},          // PPS
			{5},          // IDR
		}).Marshal()
		require.NoError(t, err)

		_, err = mux.WriteData(&astits.MuxerData{
			PID: 256,
			AdaptationField: &astits.PacketAdaptationField{
				RandomAccessIndicator: true,
			},
			PES: &astits.PESData{
				Header: &astits.PESHeader{
					OptionalHeader: &astits.PESOptionalHeader{
						MarkerBits:      2,
						PTSDTSIndicator: astits.PTSDTSIndicatorOnlyPTS,
						PTS:             &astits.ClockReference{Base: h264PTS},
					},
					StreamID: 224,
				},
				Data: au,
			},
		})
		require.NoError(t, err)

		_, err = mux.WriteData(&astits.MuxerData{
			PID: id3PID,
			PES: &astits.PESData{
				Header: &astits.PESHeader{
					OptionalHeader: &astits.PESOptionalHeader{
						MarkerBits:      2,
						PTSDTSIndicator: astits.PTSDTSIndicatorOnlyPTS,
						PTS:             &astits.ClockReference{Base: id3PTS},
					},
					StreamID: 189,
				},
				Data: id3Tag,
			},
		})
		require.NoError(t, err)

		w.Write(buf.Bytes())
	}

	httpServ := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodGet && r.URL.Path == "/index.m3u8":
				w.Header().Set("Content-Type", `application/vnd.apple.mpegurl`)
				w.Write([]byte("#EXTM3U\n" +
					"#EXT-X-VERSION:3\n" +
					"#EXT-X-ALLOW-CACHE:NO\n" +
					"#EXT-X-TARGETDURATION:2\n" +
					"#EXT-X-MEDIA-SEQUENCE:0\n" +
					"#EXT-X-PLAYLIST-TYPE:VOD\n" +
					"#EXTINF:1,\n" +
					"segment1.ts\n" +
					"#EXTINF:1,\n" +
					"segment2.ts\n" +
					"#EXTINF:1,\n" +
					"segment1.ts\n" + // repeated on purpose, to test de-duplication
					"#EXT-X-ENDLIST\n"))

			case r.Method == http.MethodGet && r.URL.Path == "/segment1.ts":
				writeSegment(w, 0, 45000, buildID3v23Tag(
					[2]string{"TIT2", "Song A"},
					[2]string{"TPE1", "Artist A"},
				))

			case r.Method == http.MethodGet && r.URL.Path == "/segment2.ts":
				writeSegment(w, 90000, 135000, buildID3v23Tag(
					[2]string{"TIT2", "Song B"},
				))
			}
		}),
	}

	ln, err := net.Listen("tcp", "localhost:5780")
	require.NoError(t, err)

	go httpServ.Serve(ln)
	defer httpServ.Shutdown(context.Background())

	tr := &http.Transport{}
	defer tr.CloseIdleConnections()

	type id3Result struct {
		pts  int64
		meta *ID3Metadata
	}

	var mutex sync.Mutex
	var id3Events []id3Result
	h264Recv := make(chan struct{})

	var c *Client
	c = &Client{
		URI:        "http://localhost:5780/index.m3u8",
		HTTPClient: &http.Client{Transport: tr},
		OnTracks: func(tracks []*Track) error {
			require.Equal(t, []*Track{{
				Codec:     &codecs.H264{},
				ClockRate: 90000,
			}}, tracks)

			// the original sample callback must keep working unchanged
			c.OnDataH26x(tracks[0], func(_, _ int64, au [][]byte) {
				require.Equal(t, [][]byte{{7, 1, 2, 3}, {8}, {5}}, au)
				select {
				case <-h264Recv:
				default:
					close(h264Recv)
				}
			})
			return nil
		},
		OnDataID3: func(pts int64, meta *ID3Metadata) {
			mutex.Lock()
			defer mutex.Unlock()
			id3Events = append(id3Events, id3Result{pts: pts, meta: meta})
		},
	}

	err = c.Start()
	require.NoError(t, err)
	defer c.Close()

	<-h264Recv

	err = c.Wait2()
	require.Equal(t, ErrClientEOS, err)

	mutex.Lock()
	defer mutex.Unlock()

	// events must be in media-time order and must not be duplicated,
	// even though segment1.ts appears twice in the playlist
	require.Equal(t, []id3Result{
		{pts: 45000, meta: &ID3Metadata{Title: "Song A", Artist: "Artist A"}},
		{pts: 135000, meta: &ID3Metadata{Title: "Song B"}},
	}, id3Events)
}

func TestClientID3EventTiming(t *testing.T) {
	const id3PID = 257

	httpServ := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodGet && r.URL.Path == "/index.m3u8":
				w.Header().Set("Content-Type", `application/vnd.apple.mpegurl`)
				w.Write([]byte("#EXTM3U\n" +
					"#EXT-X-VERSION:3\n" +
					"#EXT-X-ALLOW-CACHE:NO\n" +
					"#EXT-X-TARGETDURATION:2\n" +
					"#EXT-X-MEDIA-SEQUENCE:0\n" +
					"#EXT-X-PLAYLIST-TYPE:VOD\n" +
					"#EXTINF:2,\n" +
					"segment1.ts\n" +
					"#EXT-X-ENDLIST\n"))

			case r.Method == http.MethodGet && r.URL.Path == "/segment1.ts":
				w.Header().Set("Content-Type", `video/MP2T`)

				var buf bytes.Buffer
				mux := astits.NewMuxer(context.Background(), &buf)

				err := mux.AddElementaryStream(astits.PMTElementaryStream{
					ElementaryPID: 256,
					StreamType:    astits.StreamTypeH264Video,
				})
				require.NoError(t, err)

				err = mux.AddElementaryStream(astits.PMTElementaryStream{
					ElementaryPID: id3PID,
					StreamType:    astits.StreamTypeMetadata,
				})
				require.NoError(t, err)

				mux.SetPCRPID(256)

				au, err := h264.AnnexB([][]byte{{7, 1, 2, 3}, {8}, {5}}).Marshal()
				require.NoError(t, err)

				_, err = mux.WriteData(&astits.MuxerData{
					PID: 256,
					AdaptationField: &astits.PacketAdaptationField{
						RandomAccessIndicator: true,
					},
					PES: &astits.PESData{
						Header: &astits.PESHeader{
							OptionalHeader: &astits.PESOptionalHeader{
								MarkerBits:      2,
								PTSDTSIndicator: astits.PTSDTSIndicatorOnlyPTS,
								PTS:             &astits.ClockReference{Base: 0},
							},
							StreamID: 224,
						},
						Data: au,
					},
				})
				require.NoError(t, err)

				// ID3 event 1 second after the start of the stream
				_, err = mux.WriteData(&astits.MuxerData{
					PID: id3PID,
					PES: &astits.PESData{
						Header: &astits.PESHeader{
							OptionalHeader: &astits.PESOptionalHeader{
								MarkerBits:      2,
								PTSDTSIndicator: astits.PTSDTSIndicatorOnlyPTS,
								PTS:             &astits.ClockReference{Base: 90000},
							},
							StreamID: 189,
						},
						Data: buildID3v23Tag([2]string{"TIT2", "Song A"}),
					},
				})
				require.NoError(t, err)

				w.Write(buf.Bytes())
			}
		}),
	}

	ln, err := net.Listen("tcp", "localhost:5781")
	require.NoError(t, err)

	go httpServ.Serve(ln)
	defer httpServ.Shutdown(context.Background())

	tr := &http.Transport{}
	defer tr.CloseIdleConnections()

	start := time.Now()
	eventTime := make(chan time.Time, 1)

	c := &Client{
		URI:        "http://localhost:5781/index.m3u8",
		HTTPClient: &http.Client{Transport: tr},
		OnDataID3: func(_ int64, _ *ID3Metadata) {
			eventTime <- time.Now()
		},
	}

	err = c.Start()
	require.NoError(t, err)
	defer c.Close()

	select {
	case tm := <-eventTime:
		// the event must be dispatched in sync with playback,
		// not at download time
		require.Greater(t, tm.Sub(start), 500*time.Millisecond)
		require.Less(t, tm.Sub(start), 5*time.Second)

	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for ID3 event")
	}
}
