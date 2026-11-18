package gohlslib

import (
	"fmt"
	"sort"
)

const (
	mpegtsPacketSize          = 188
	mpegtsSyncByte            = 0x47
	mpegtsMaxPTS              = 0x1FFFFFFFF // 33 bits
	mpegtsHalfPTSRange        = 0x100000000
	streamTypePrivateData     = 0x06
	streamTypeMetadata        = 0x15
	descriptorTagRegistration = 0x05
)

type id3Event struct {
	pts    int64
	title  string
	artist string
}

// sortID3EventsByPTS sorts events by PTS, taking the 33-bit PTS wrap into account.
func sortID3EventsByPTS(events []id3Event) {
	sort.SliceStable(events, func(i, j int) bool {
		return (events[j].pts-events[i].pts)&mpegtsMaxPTS < mpegtsHalfPTSRange
	})
}

// id3PESState is the reassembly state of a single ID3 PID.
// It persists across segments, allowing PES packets to span segment boundaries.
type id3PESState struct {
	buf          []byte
	expected     int // 0 = header not parsed yet, -1 = unbounded, >0 = total PES size
	headerParsed bool
	headerLen    int
	pts          int64
	hasPTS       bool
	dropping     bool // drop packets until the next PUSI

	hasContinuity bool
	continuity    uint8
}

func (s *id3PESState) reset() {
	s.buf = s.buf[:0]
	s.expected = 0
	s.headerParsed = false
	s.headerLen = 0
	s.pts = 0
	s.hasPTS = false
	s.dropping = false
}

// clientID3ExtractorMPEGTS extracts ID3 timed metadata from MPEG-TS segment payloads.
type clientID3ExtractorMPEGTS struct {
	onDecodeError ClientOnDecodeErrorFunc

	pmtPID    uint16
	hasPMTPID bool
	id3PIDs   map[uint16]struct{}
	pes       map[uint16]*id3PESState

	scrambledReported bool
}

func (e *clientID3ExtractorMPEGTS) initialize() {
	e.id3PIDs = make(map[uint16]struct{})
	e.pes = make(map[uint16]*id3PESState)
}

// processSegment scans a segment payload and returns parsed ID3 events.
// State is kept between calls, therefore PES packets can span multiple segments.
func (e *clientID3ExtractorMPEGTS) processSegment(payload []byte) []id3Event {
	e.scrambledReported = false

	var events []id3Event
	found := false

	pos := 0
	for pos+mpegtsPacketSize <= len(payload) {
		pkt := payload[pos : pos+mpegtsPacketSize]

		if pkt[0] != mpegtsSyncByte {
			// resynchronize
			skipped := 0
			for pos < len(payload) && payload[pos] != mpegtsSyncByte {
				pos++
				skipped++
			}
			e.onDecodeError(fmt.Errorf("ID3: skipped %d bytes while synchronizing", skipped))
			continue
		}

		found = true
		pos += mpegtsPacketSize
		e.processPacket(pkt, &events)
	}

	if !found && len(payload) >= mpegtsPacketSize {
		e.onDecodeError(fmt.Errorf("ID3: no valid MPEG-TS packets found (segment may be encrypted)"))
	}

	return events
}

func (e *clientID3ExtractorMPEGTS) processPacket(pkt []byte, events *[]id3Event) {
	pusi := (pkt[1] & 0x40) != 0
	pid := uint16(pkt[1]&0x1F)<<8 | uint16(pkt[2])
	scrambling := pkt[3] >> 6
	adaptationFieldControl := (pkt[3] >> 4) & 0x03
	continuity := pkt[3] & 0x0F

	if scrambling != 0 {
		// scrambled packets cannot be parsed: skip them
		if !e.scrambledReported {
			e.scrambledReported = true
			e.onDecodeError(fmt.Errorf("ID3: skipping scrambled packets"))
		}
		return
	}

	if adaptationFieldControl == 0 || adaptationFieldControl == 2 {
		// no payload
		return
	}

	off := 4
	if adaptationFieldControl == 3 {
		off += 1 + int(pkt[4])
		if off > mpegtsPacketSize {
			e.onDecodeError(fmt.Errorf("ID3: invalid adaptation field length"))
			return
		}
	}

	payload := pkt[off:]
	if len(payload) == 0 {
		return
	}

	switch {
	case pid == 0:
		if pusi {
			e.processPAT(payload)
		}

	case e.hasPMTPID && pid == e.pmtPID:
		if pusi {
			e.processPMT(payload)
		}

	default:
		if _, ok := e.id3PIDs[pid]; ok {
			e.processID3Packet(pid, pusi, continuity, payload, events)
		}
	}
}

func (e *clientID3ExtractorMPEGTS) processPAT(payload []byte) {
	pointer := int(payload[0])
	if 1+pointer >= len(payload) {
		e.onDecodeError(fmt.Errorf("ID3: invalid PAT pointer"))
		return
	}

	sec := payload[1+pointer:]
	if len(sec) < 12 {
		e.onDecodeError(fmt.Errorf("ID3: PAT is too short"))
		return
	}

	if sec[0] != 0x00 {
		e.onDecodeError(fmt.Errorf("ID3: invalid PAT table ID"))
		return
	}

	secLen := int(sec[1]&0x0F)<<8 | int(sec[2])
	if 3+secLen > len(sec) {
		e.onDecodeError(fmt.Errorf("ID3: PAT spans multiple TS packets, unsupported"))
		return
	}

	programs := sec[8 : 3+secLen-4]
	for i := 0; i+4 <= len(programs); i += 4 {
		programNumber := uint16(programs[i])<<8 | uint16(programs[i+1])
		pid := uint16(programs[i+2]&0x1F)<<8 | uint16(programs[i+3])
		if programNumber != 0 {
			e.pmtPID = pid
			e.hasPMTPID = true
			break
		}
	}
}

func (e *clientID3ExtractorMPEGTS) processPMT(payload []byte) {
	pointer := int(payload[0])
	if 1+pointer >= len(payload) {
		e.onDecodeError(fmt.Errorf("ID3: invalid PMT pointer"))
		return
	}

	sec := payload[1+pointer:]
	if len(sec) < 16 {
		e.onDecodeError(fmt.Errorf("ID3: PMT is too short"))
		return
	}

	if sec[0] != 0x02 {
		e.onDecodeError(fmt.Errorf("ID3: invalid PMT table ID"))
		return
	}

	secLen := int(sec[1]&0x0F)<<8 | int(sec[2])
	if 3+secLen > len(sec) {
		e.onDecodeError(fmt.Errorf("ID3: PMT spans multiple TS packets, unsupported"))
		return
	}

	programInfoLength := int(sec[10]&0x0F)<<8 | int(sec[11])
	pos := 12 + programInfoLength
	end := 3 + secLen - 4

	for pos+5 <= end {
		streamType := sec[pos]
		elemPID := uint16(sec[pos+1]&0x1F)<<8 | uint16(sec[pos+2])
		esInfoLength := int(sec[pos+3]&0x0F)<<8 | int(sec[pos+4])

		descEnd := pos + 5 + esInfoLength
		if descEnd > end {
			descEnd = end
		}
		descriptors := sec[pos+5 : descEnd]

		// ID3 is carried either in a metadata stream (0x15), as per Apple HLS specification,
		// or in a private data stream (0x06) with a "ID3 " registration descriptor.
		if streamType == streamTypeMetadata ||
			(streamType == streamTypePrivateData && hasID3RegistrationDescriptor(descriptors)) {
			e.id3PIDs[elemPID] = struct{}{}
		}

		pos += 5 + esInfoLength
	}
}

func hasID3RegistrationDescriptor(descriptors []byte) bool {
	for i := 0; i+2 <= len(descriptors); {
		tag := descriptors[i]
		length := int(descriptors[i+1])
		if i+2+length > len(descriptors) {
			break
		}
		if tag == descriptorTagRegistration && length >= 4 &&
			string(descriptors[i+2:i+6]) == "ID3 " {
			return true
		}
		i += 2 + length
	}
	return false
}

func (e *clientID3ExtractorMPEGTS) processID3Packet(
	pid uint16,
	pusi bool,
	continuity uint8,
	payload []byte,
	events *[]id3Event,
) {
	st := e.pes[pid]
	if st == nil {
		st = &id3PESState{}
		e.pes[pid] = st
	}

	// continuity check.
	// It is performed only while a partial PES is being assembled:
	// in other cases, counter jumps are legit (e.g. counters restart at
	// segment boundaries) and do not corrupt any data.
	if st.hasContinuity && (len(st.buf) > 0 || st.dropping) {
		switch {
		case continuity == st.continuity:
			// duplicated packet
			return

		case continuity != (st.continuity+1)&0x0F:
			e.onDecodeError(fmt.Errorf("ID3: continuity error on PID %d", pid))
			if len(st.buf) > 0 {
				e.onDecodeError(fmt.Errorf("ID3: truncated PES dropped"))
			}
			st.reset()
		}
	}
	st.hasContinuity = true
	st.continuity = continuity

	if pusi {
		if !st.dropping && len(st.buf) > 0 {
			if st.expected == -1 {
				// unbounded PES: it ends when the next PES starts
				e.finalizePES(st, events)
			} else {
				e.onDecodeError(fmt.Errorf("ID3: truncated PES dropped (%d of %d bytes)", len(st.buf), st.expected))
			}
		}
		st.reset()
	}

	if st.dropping {
		return
	}

	st.buf = append(st.buf, payload...)

	if len(st.buf) > clientMaxID3PESSize {
		e.onDecodeError(fmt.Errorf("ID3: PES size exceeds maximum of %d bytes", clientMaxID3PESSize))
		st.reset()
		st.dropping = true
		return
	}

	if !st.headerParsed {
		e.parsePESHeader(st)
		if st.dropping {
			return
		}
	}

	if st.expected > 0 && len(st.buf) >= st.expected {
		e.finalizePES(st, events)
		st.reset()
	}
}

func (e *clientID3ExtractorMPEGTS) parsePESHeader(st *id3PESState) {
	if len(st.buf) < 6 {
		return
	}

	if st.buf[0] != 0x00 || st.buf[1] != 0x00 || st.buf[2] != 0x01 {
		e.onDecodeError(fmt.Errorf("ID3: invalid PES start code"))
		st.reset()
		st.dropping = true
		return
	}

	pesLen := int(st.buf[4])<<8 | int(st.buf[5])
	if pesLen == 0 {
		st.expected = -1
	} else {
		st.expected = 6 + pesLen
		if st.expected > clientMaxID3PESSize {
			e.onDecodeError(fmt.Errorf("ID3: PES size %d exceeds maximum of %d bytes", st.expected, clientMaxID3PESSize))
			st.reset()
			st.dropping = true
			return
		}
	}

	if len(st.buf) < 9 {
		return
	}

	if st.buf[6]>>6 != 0b10 {
		e.onDecodeError(fmt.Errorf("ID3: invalid PES header"))
		st.reset()
		st.dropping = true
		return
	}

	ptsDtsIndicator := st.buf[7] >> 6
	if ptsDtsIndicator == 0b01 {
		e.onDecodeError(fmt.Errorf("ID3: invalid PTS/DTS indicator"))
		st.reset()
		st.dropping = true
		return
	}

	headerDataLength := int(st.buf[8])
	st.headerLen = 9 + headerDataLength

	if st.expected > 0 && st.headerLen > st.expected {
		e.onDecodeError(fmt.Errorf("ID3: invalid PES header length"))
		st.reset()
		st.dropping = true
		return
	}

	if (ptsDtsIndicator & 0b10) != 0 {
		if headerDataLength < 5 {
			e.onDecodeError(fmt.Errorf("ID3: invalid PES header data length"))
			st.reset()
			st.dropping = true
			return
		}

		if len(st.buf) < 14 {
			// wait for more data
			return
		}

		st.pts = int64(st.buf[9]>>1&0x07)<<30 |
			int64(st.buf[10])<<22 |
			int64(st.buf[11]>>1&0x7F)<<15 |
			int64(st.buf[12])<<7 |
			int64(st.buf[13]>>1&0x7F)
		st.hasPTS = true
	}

	st.headerParsed = true
}

func (e *clientID3ExtractorMPEGTS) finalizePES(st *id3PESState, events *[]id3Event) {
	if !st.hasPTS {
		e.onDecodeError(fmt.Errorf("ID3: PES without PTS, discarded"))
		return
	}

	if len(st.buf) < st.headerLen {
		e.onDecodeError(fmt.Errorf("ID3: truncated PES dropped"))
		return
	}

	data := st.buf[st.headerLen:]
	if st.expected > 0 && st.expected < len(st.buf) {
		data = st.buf[st.headerLen:st.expected]
	}

	meta, err := parseID3Tag(data)
	if err != nil {
		e.onDecodeError(err)
		return
	}

	if meta.Title == "" && meta.Artist == "" {
		// tag does not contain any known text frame: nothing to dispatch
		return
	}

	*events = append(*events, id3Event{
		pts:    st.pts,
		title:  meta.Title,
		artist: meta.Artist,
	})
}
