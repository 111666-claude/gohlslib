// Package webvtt contains utilities to marshal and unmarshal WebVTT content.
package webvtt

import (
	"bufio"
	"bytes"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Cue is a single WebVTT cue.
// Start and End are expressed on the global media timeline, in track clock-rate units.
type Cue struct {
	Start int64
	End   int64
	// Identifier is the optional cue identifier.
	Identifier string
	// Settings contains the optional cue settings (e.g. "align:center position:50%").
	Settings string
	// Text is the cue payload. It may span multiple lines.
	Text string
}

// ClockRate is the reference clock rate of WebVTT tracks: one millisecond per tick.
const ClockRate = 1000

// formatTimestamp converts a duration into a WebVTT timestamp (HH:MM:SS.mmm).
func formatTimestamp(d time.Duration) string {
	if d < 0 {
		d = 0
	}

	ms := d.Milliseconds()
	hours := ms / (60 * 60 * 1000)
	ms -= hours * 60 * 60 * 1000
	minutes := ms / (60 * 1000)
	ms -= minutes * 60 * 1000
	seconds := ms / 1000
	millis := ms % 1000

	return fmt.Sprintf("%02d:%02d:%02d.%03d", hours, minutes, seconds, millis)
}

// parseTimestamp parses a WebVTT timestamp (MM:SS.mmm or HH:MM:SS.mmm).
func parseTimestamp(v string) (time.Duration, error) {
	parts := strings.Split(v, ":")
	if len(parts) != 2 && len(parts) != 3 {
		return 0, fmt.Errorf("invalid timestamp: %s", v)
	}

	var hours, minutes int64
	var secPart string

	if len(parts) == 3 {
		var err error
		hours, err = strconv.ParseInt(parts[0], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid timestamp: %s", v)
		}
		minutes, err = strconv.ParseInt(parts[1], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid timestamp: %s", v)
		}
		secPart = parts[2]
	} else {
		var err error
		minutes, err = strconv.ParseInt(parts[0], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid timestamp: %s", v)
		}
		secPart = parts[1]
	}

	secParts := strings.Split(secPart, ".")
	if len(secParts) != 2 {
		return 0, fmt.Errorf("invalid timestamp: %s", v)
	}

	seconds, err := strconv.ParseInt(secParts[0], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid timestamp: %s", v)
	}

	fracStr := secParts[1]
	// fraction can have 1-3 digits
	if len(fracStr) > 3 {
		fracStr = fracStr[:3]
	}
	millis, err := strconv.ParseInt(fracStr, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid timestamp: %s", v)
	}
	for i := len(fracStr); i < 3; i++ {
		millis *= 10
	}

	total := (((hours*60+minutes)*60+seconds)*1000 + millis)
	return time.Duration(total) * time.Millisecond, nil
}

// Segment is the content of a WebVTT media segment.
// LocalOffset is the value of the LOCAL parameter of the X-TIMESTAMP-MAP header.
// Cues contain timestamps on the global media timeline (in milliseconds).
type Segment struct {
	LocalOffset time.Duration
	Cues        []*Cue
}

// Marshal encodes the segment as a standalone WebVTT file.
// Cue timestamps are shifted by the segment LocalOffset so that
// cues keep their position on the global media timeline.
func (s *Segment) Marshal() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString("WEBVTT\n")
	buf.WriteString("X-TIMESTAMP-MAP=LOCAL:")
	buf.WriteString(formatTimestamp(s.LocalOffset))
	buf.WriteString(",MPEGTS:0\n")
	buf.WriteString("\n")

	for _, cue := range s.Cues {
		if cue.Identifier != "" {
			buf.WriteString(cue.Identifier)
			buf.WriteString("\n")
		}

		buf.WriteString(formatTimestamp(timestampToDuration(cue.Start)))
		buf.WriteString(" --> ")
		buf.WriteString(formatTimestamp(timestampToDuration(cue.End)))
		if cue.Settings != "" {
			buf.WriteString(" ")
			buf.WriteString(cue.Settings)
		}
		buf.WriteString("\n")

		buf.WriteString(cue.Text)
		if !strings.HasSuffix(cue.Text, "\n") {
			buf.WriteString("\n")
		}
		buf.WriteString("\n")
	}

	return buf.Bytes(), nil
}

func timestampToDuration(ms int64) time.Duration {
	return time.Duration(ms) * time.Millisecond
}

// TimestampMap is the parsed X-TIMESTAMP-MAP header of a WebVTT segment.
type TimestampMap struct {
	Local  time.Duration
	MPEGTS int64
}

// parseTimestampMap parses the X-TIMESTAMP-MAP header value.
func parseTimestampMap(v string) (*TimestampMap, error) {
	ret := &TimestampMap{}

	for _, part := range strings.Split(v, ",") {
		kv := strings.SplitN(part, ":", 2)
		if len(kv) != 2 {
			return nil, fmt.Errorf("invalid X-TIMESTAMP-MAP: %s", v)
		}

		switch kv[0] {
		case "LOCAL":
			d, err := parseTimestamp(kv[1])
			if err != nil {
				return nil, fmt.Errorf("invalid X-TIMESTAMP-MAP: %s", v)
			}
			ret.Local = d

		case "MPEGTS":
			n, err := strconv.ParseInt(kv[1], 10, 64)
			if err != nil {
				return nil, fmt.Errorf("invalid X-TIMESTAMP-MAP: %s", v)
			}
			ret.MPEGTS = n

		default:
			return nil, fmt.Errorf("invalid X-TIMESTAMP-MAP: %s", v)
		}
	}

	return ret, nil
}

// Unmarshal decodes a standalone WebVTT file and returns the cues,
// with timestamps converted to the global media timeline (in milliseconds).
// The returned cues keep the order in which they appear in the file.
func (s *Segment) Unmarshal(byts []byte) error {
	scanner := bufio.NewScanner(bytes.NewReader(byts))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var lines []string
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		return err
	}

	if len(lines) == 0 || !strings.HasPrefix(lines[0], "WEBVTT") {
		return fmt.Errorf("not a WebVTT file")
	}

	var localOffset time.Duration

	i := 1
	for ; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			break
		}
		if strings.HasPrefix(line, "X-TIMESTAMP-MAP=") {
			tsMap, err := parseTimestampMap(strings.TrimPrefix(line, "X-TIMESTAMP-MAP="))
			if err != nil {
				return err
			}
			localOffset = tsMap.Local
		}
	}

	s.Cues = nil

	for i < len(lines) {
		// skip blank lines
		for i < len(lines) && strings.TrimSpace(lines[i]) == "" {
			i++
		}
		if i >= len(lines) {
			break
		}

		identifier := ""
		timingLine := lines[i]

		// a line without "-->" is an identifier; the timing line follows it
		if !strings.Contains(timingLine, "-->") {
			identifier = timingLine
			i++
			if i >= len(lines) {
				return fmt.Errorf("truncated WebVTT cue")
			}
			timingLine = lines[i]
		}

		start, end, settings, err := parseTimingLine(timingLine)
		if err != nil {
			return err
		}
		i++

		var textLines []string
		for i < len(lines) && strings.TrimSpace(lines[i]) != "" {
			textLines = append(textLines, lines[i])
			i++
		}

		globalStart := start - localOffset
		globalEnd := end - localOffset

		s.Cues = append(s.Cues, &Cue{
			Start:      globalStart.Milliseconds(),
			End:        globalEnd.Milliseconds(),
			Identifier: identifier,
			Settings:   settings,
			Text:       strings.Join(textLines, "\n"),
		})
	}

	s.LocalOffset = localOffset

	return nil
}

func parseTimingLine(line string) (time.Duration, time.Duration, string, error) {
	arrow := strings.Index(line, "-->")
	if arrow < 0 {
		return 0, 0, "", fmt.Errorf("invalid cue timing line: %s", line)
	}

	startStr := strings.TrimSpace(line[:arrow])
	rest := strings.TrimSpace(line[arrow+len("-->"):])

	// the rest is "end [settings...]"
	endStr, settings, _ := strings.Cut(rest, " ")

	start, err := parseTimestamp(startStr)
	if err != nil {
		return 0, 0, "", err
	}

	end, err := parseTimestamp(endStr)
	if err != nil {
		return 0, 0, "", err
	}

	return start, end, strings.TrimSpace(settings), nil
}
