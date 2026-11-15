package webvtt

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSegmentMarshalUnmarshal(t *testing.T) {
	seg := &Segment{
		Cues: []*Cue{
			{Start: 10500, End: 12500, Text: "hello"},
			{Start: 13000, End: 14000, Identifier: "id2", Settings: "align:left", Text: "line1\nline2"},
		},
	}

	byts, err := seg.Marshal()
	require.NoError(t, err)

	expected := "WEBVTT\n" +
		"X-TIMESTAMP-MAP=LOCAL:00:00:00.000,MPEGTS:0\n" +
		"\n" +
		"00:00:10.500 --> 00:00:12.500\n" +
		"hello\n" +
		"\n" +
		"id2\n" +
		"00:00:13.000 --> 00:00:14.000 align:left\n" +
		"line1\n" +
		"line2\n" +
		"\n"

	require.Equal(t, expected, string(byts))

	var decoded Segment
	err = decoded.Unmarshal(byts)
	require.NoError(t, err)

	require.Equal(t, seg.Cues, decoded.Cues)
}

func TestSegmentUnmarshalWithLocalOffset(t *testing.T) {
	// LOCAL offset of 11s: local cue at 00:00:11.500 maps back to global 0.500s
	input := "WEBVTT\n" +
		"X-TIMESTAMP-MAP=LOCAL:00:00:11.000,MPEGTS:0\n" +
		"\n" +
		"00:00:11.500 --> 00:00:12.500\n" +
		"crossing cue\n" +
		"\n"

	var decoded Segment
	err := decoded.Unmarshal([]byte(input))
	require.NoError(t, err)

	require.Len(t, decoded.Cues, 1)
	require.Equal(t, int64(500), decoded.Cues[0].Start)
	require.Equal(t, int64(1500), decoded.Cues[0].End)
	require.Equal(t, "crossing cue", decoded.Cues[0].Text)
}

func TestTimestampRoundTrip(t *testing.T) {
	for _, d := range []time.Duration{
		0,
		500 * time.Millisecond,
		12*time.Second + 345*time.Millisecond,
		3723 * time.Second,
	} {
		s := formatTimestamp(d)
		back, err := parseTimestamp(s)
		require.NoError(t, err)
		require.Equal(t, d, back)
	}
}

func TestUnmarshalInvalid(t *testing.T) {
	var decoded Segment
	require.Error(t, decoded.Unmarshal([]byte("not a vtt file")))

	// truncated cue
	require.Error(t, decoded.Unmarshal([]byte("WEBVTT\n\n00:00:01.000 -->\n")))
}
