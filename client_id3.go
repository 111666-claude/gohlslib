package gohlslib

import (
	"encoding/binary"
	"fmt"
	"unicode/utf16"
)

const (
	clientMaxID3TagSize    = 1 * 1024 * 1024
	clientMaxID3PESSize    = 1 * 1024 * 1024
	clientMaxID3SeenEvents = 256
)

// ID3Metadata contains timed metadata extracted from an ID3 tag
// found inside a MPEG-TS stream.
type ID3Metadata struct {
	// song title (TIT2 frame).
	Title string

	// artist name (TPE1 frame).
	Artist string
}

// ClientOnDataID3Func is the prototype of the function passed to OnDataID3.
type ClientOnDataID3Func func(pts int64, meta *ID3Metadata)

func id3SyncsafeSize(b []byte) (int, error) {
	if (b[0]&0x80) != 0 || (b[1]&0x80) != 0 || (b[2]&0x80) != 0 || (b[3]&0x80) != 0 {
		return 0, fmt.Errorf("invalid syncsafe integer")
	}
	return int(b[0])<<21 | int(b[1])<<14 | int(b[2])<<7 | int(b[3]), nil
}

// id3RemoveUnsynchronisation removes 0x00 bytes inserted after 0xFF bytes.
func id3RemoveUnsynchronisation(b []byte) []byte {
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); i++ {
		out = append(out, b[i])
		if b[i] == 0xFF && i+1 < len(b) && b[i+1] == 0x00 {
			i++
		}
	}
	return out
}

func id3DecodeUTF16(b []byte, bigEndian bool) string {
	u16 := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		if bigEndian {
			u16 = append(u16, uint16(b[i])<<8|uint16(b[i+1]))
		} else {
			u16 = append(u16, uint16(b[i+1])<<8|uint16(b[i]))
		}
	}
	return string(utf16.Decode(u16))
}

func id3DecodeText(b []byte) string {
	if len(b) == 0 {
		return ""
	}

	enc := b[0]
	b = b[1:]

	var ret string

	switch enc {
	case 0x00: // ISO-8859-1
		runes := make([]rune, len(b))
		for i, c := range b {
			runes[i] = rune(c)
		}
		ret = string(runes)

	case 0x01: // UTF-16 with BOM
		if len(b) >= 2 {
			if b[0] == 0xFF && b[1] == 0xFE {
				ret = id3DecodeUTF16(b[2:], false)
			} else if b[0] == 0xFE && b[1] == 0xFF {
				ret = id3DecodeUTF16(b[2:], true)
			}
		}

	case 0x02: // UTF-16BE without BOM
		ret = id3DecodeUTF16(b, true)

	case 0x03: // UTF-8
		ret = string(b)

	default: // unknown encoding
		return ""
	}

	// strip trailing NULL characters
	end := len(ret)
	for end > 0 && ret[end-1] == 0 {
		end--
	}
	return ret[:end]
}

// parseID3Tag parses an ID3v2.3 or ID3v2.4 tag and extracts text metadata.
// Unknown frames are skipped.
func parseID3Tag(data []byte) (*ID3Metadata, error) {
	if len(data) < 10 {
		return nil, fmt.Errorf("ID3 tag is too short")
	}

	if data[0] != 'I' || data[1] != 'D' || data[2] != '3' {
		return nil, fmt.Errorf("invalid ID3 magic")
	}

	major := data[3]
	if major != 3 && major != 4 {
		return nil, fmt.Errorf("unsupported ID3 version: 2.%d", major)
	}

	flags := data[5]

	size, err := id3SyncsafeSize(data[6:10])
	if err != nil {
		return nil, fmt.Errorf("invalid ID3 size: %w", err)
	}

	if size > clientMaxID3TagSize {
		return nil, fmt.Errorf("ID3 tag size %d exceeds maximum of %d", size, clientMaxID3TagSize)
	}

	end := 10 + size
	if major == 4 && (flags&0x10) != 0 { // footer present
		end += 10
	}

	if end > len(data) {
		return nil, fmt.Errorf("invalid ID3 length: tag declares %d bytes, but only %d are available", end, len(data))
	}

	body := data[10 : 10+size]

	if (flags & 0x40) != 0 { // extended header
		if len(body) < 4 {
			return nil, fmt.Errorf("invalid ID3 extended header")
		}

		if major == 3 {
			ehSize := int(binary.BigEndian.Uint32(body))
			if 4+ehSize > len(body) {
				return nil, fmt.Errorf("invalid ID3 extended header size")
			}
			body = body[4+ehSize:]
		} else {
			var ehSize int
			ehSize, err = id3SyncsafeSize(body[:4])
			if err != nil || ehSize < 4 || ehSize > len(body) {
				return nil, fmt.Errorf("invalid ID3 extended header size")
			}
			body = body[ehSize:]
		}
	}

	meta := &ID3Metadata{}

	for len(body) >= 10 {
		if body[0] == 0 { // padding
			break
		}

		id := string(body[0:4])

		var frameSize int
		if major == 3 {
			frameSize = int(binary.BigEndian.Uint32(body[4:8]))
		} else {
			frameSize, err = id3SyncsafeSize(body[4:8])
			if err != nil {
				return nil, fmt.Errorf("invalid ID3 frame size: %w", err)
			}
		}

		frameFlags := uint16(body[8])<<8 | uint16(body[9])

		if 10+frameSize > len(body) {
			// truncated frame: stop parsing, keep frames parsed so far
			break
		}

		frameData := body[10 : 10+frameSize]
		body = body[10+frameSize:]

		// unsynchronisation: tag-level (v2.3 and v2.4) or per-frame (v2.4)
		if (flags&0x80) != 0 || (major == 4 && (frameFlags&0x0002) != 0) {
			frameData = id3RemoveUnsynchronisation(frameData)
		}

		switch id {
		case "TIT2":
			meta.Title = id3DecodeText(frameData)

		case "TPE1":
			meta.Artist = id3DecodeText(frameData)

		default:
			// unknown frame: skip
		}
	}

	return meta, nil
}
