package rtmp

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"sync"
	"time"
)

const (
	chunkSizeDefault = 128
	chunkSizeMax     = 0x7FFFFFFF
	msgLengthMax     = 0xFFFFFF // 16 MB - hard RTMP limit
)

// ---------- reader ----------

type inChunkStream struct {
	timestamp   uint32
	delta       uint32
	length      uint32
	typeID      uint8
	msgStreamID uint32
	extended    bool
	buf         []byte
	filled      uint32
}

// Reader assembles RTMP chunks into whole messages.
//
// A single Reader must only be used by one goroutine at a time; connections use
// one reader goroutine each, which is also why no locking is needed here.
type Reader struct {
	br        *bufio.Reader
	chunkSize uint32
	streams   map[uint32]*inChunkStream
	total     uint64
	maxMsg    uint32
}

func NewReader(br *bufio.Reader) *Reader {
	return &Reader{
		br:        br,
		chunkSize: chunkSizeDefault,
		streams:   make(map[uint32]*inChunkStream, 8),
		maxMsg:    msgLengthMax,
	}
}

// TotalRead returns the number of bytes consumed so far (used for RTMP acks).
func (r *Reader) TotalRead() uint64 { return r.total }

// SetChunkSize applies a peer chunk-size change.
func (r *Reader) SetChunkSize(n uint32) error {
	if n == 0 || n > chunkSizeMax {
		return fmt.Errorf("rtmp: illegal chunk size %d", n)
	}
	r.chunkSize = n
	return nil
}

func (r *Reader) readByte() (byte, error) {
	b, err := r.br.ReadByte()
	if err == nil {
		r.total++
	}
	return b, err
}

func (r *Reader) readFull(b []byte) error {
	n, err := io.ReadFull(r.br, b)
	r.total += uint64(n)
	return err
}

func (r *Reader) uint24() (uint32, error) {
	var b [3]byte
	if err := r.readFull(b[:]); err != nil {
		return 0, err
	}
	return uint32(b[0])<<16 | uint32(b[1])<<8 | uint32(b[2]), nil
}

func (r *Reader) uint32() (uint32, error) {
	var b [4]byte
	if err := r.readFull(b[:]); err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint32(b[:]), nil
}

func (r *Reader) uint32LE() (uint32, error) {
	var b [4]byte
	if err := r.readFull(b[:]); err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(b[:]), nil
}

// ReadMessage blocks until one complete message has been reassembled.
func (r *Reader) ReadMessage() (*Message, error) {
	for {
		b0, err := r.readByte()
		if err != nil {
			return nil, err
		}
		fmtType := b0 >> 6
		csid := uint32(b0 & 0x3F)
		switch csid {
		case 0:
			b, err := r.readByte()
			if err != nil {
				return nil, err
			}
			csid = 64 + uint32(b)
		case 1:
			b1, err := r.readByte()
			if err != nil {
				return nil, err
			}
			b2, err := r.readByte()
			if err != nil {
				return nil, err
			}
			csid = 64 + uint32(b1) + uint32(b2)<<8
		}

		cs := r.streams[csid]
		if cs == nil {
			cs = &inChunkStream{}
			r.streams[csid] = cs
		}

		newMessage := cs.length == 0 || cs.filled >= cs.length

		switch fmtType {
		case 0:
			ts, err := r.uint24()
			if err != nil {
				return nil, err
			}
			length, err := r.uint24()
			if err != nil {
				return nil, err
			}
			typeID, err := r.readByte()
			if err != nil {
				return nil, err
			}
			sid, err := r.uint32LE()
			if err != nil {
				return nil, err
			}
			cs.timestamp, cs.delta = ts, 0
			cs.length, cs.typeID, cs.msgStreamID = length, typeID, sid
			cs.extended = false
			if ts == 0xFFFFFF {
				ext, err := r.uint32()
				if err != nil {
					return nil, err
				}
				cs.timestamp = ext
				cs.extended = true
			}
			newMessage = true
		case 1:
			delta, err := r.uint24()
			if err != nil {
				return nil, err
			}
			length, err := r.uint24()
			if err != nil {
				return nil, err
			}
			typeID, err := r.readByte()
			if err != nil {
				return nil, err
			}
			cs.extended = false
			if delta == 0xFFFFFF {
				ext, err := r.uint32()
				if err != nil {
					return nil, err
				}
				delta = ext
				cs.extended = true
			}
			cs.timestamp += delta
			cs.delta = delta
			cs.length, cs.typeID = length, typeID
			newMessage = true
		case 2:
			delta, err := r.uint24()
			if err != nil {
				return nil, err
			}
			cs.extended = false
			if delta == 0xFFFFFF {
				ext, err := r.uint32()
				if err != nil {
					return nil, err
				}
				delta = ext
				cs.extended = true
			}
			cs.timestamp += delta
			cs.delta = delta
			newMessage = true
		case 3:
			if cs.extended {
				if _, err := r.uint32(); err != nil { // extended timestamp present
					return nil, err
				}
			}
			if newMessage {
				cs.timestamp += cs.delta
			}
		}

		if newMessage {
			if cs.length > r.maxMsg {
				return nil, fmt.Errorf("rtmp: message %d bytes exceeds limit", cs.length)
			}
			if cs.length == 0 {
				continue
			}
			cs.buf = make([]byte, cs.length)
			cs.filled = 0
		}

		n := cs.length - cs.filled
		if n > r.chunkSize {
			n = r.chunkSize
		}
		if err := r.readFull(cs.buf[cs.filled : cs.filled+n]); err != nil {
			return nil, err
		}
		cs.filled += n

		if cs.filled >= cs.length {
			msg := &Message{
				TypeID:     cs.typeID,
				StreamID:   cs.msgStreamID,
				Timestamp:  cs.timestamp,
				Payload:    cs.buf,
				ReceivedAt: time.Now(),
			}
			cs.buf = nil
			cs.filled = cs.length // mark "ready for next message"
			return msg, nil
		}
	}
}

// ---------- writer ----------

// Writer serialises messages into chunks and writes them in a single syscall
// per message. Writes are mutex protected so control messages can be injected
// from other goroutines (pings, acks, latency probes) without interleaving.
type Writer struct {
	mu        sync.Mutex
	w         io.Writer
	chunkSize uint32
	scratch   []byte
}

func NewWriter(w io.Writer) *Writer {
	return &Writer{w: w, chunkSize: chunkSizeDefault, scratch: make([]byte, 0, 4096)}
}

func (w *Writer) SetChunkSize(n uint32) {
	if n == 0 {
		return
	}
	w.mu.Lock()
	w.chunkSize = n
	w.mu.Unlock()
}

func (w *Writer) ChunkSize() uint32 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.chunkSize
}

func (w *Writer) WriteMessage(m *Message) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	buf := w.encode(m)
	if _, err := w.w.Write(buf); err != nil {
		return err
	}
	if cap(buf) < 1<<20 {
		w.scratch = buf[:0]
	} else {
		w.scratch = make([]byte, 0, 4096)
	}
	return nil
}

// WriteBatch writes several messages with a single syscall. Used by the
// per-viewer writer goroutine, which drains a small queue at a time.
func (w *Writer) WriteBatch(msgs []*Message) error {
	if len(msgs) == 0 {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	buf := w.scratch[:0]
	for _, m := range msgs {
		buf = w.appendEncoded(buf, m)
	}
	if _, err := w.w.Write(buf); err != nil {
		return err
	}
	if cap(buf) < 1<<20 {
		w.scratch = buf[:0]
	} else {
		w.scratch = make([]byte, 0, 4096)
	}
	return nil
}

func (w *Writer) encode(m *Message) []byte {
	buf := w.scratch[:0]
	return w.appendEncoded(buf, m)
}

// appendEncoded emits a fmt=0 header for every message. fmt=0 costs 11 extra
// bytes per message but removes an entire class of delta-timestamp bugs and
// keeps per-message latency flat; at 60 fps that is ~2 KB/s.
func (w *Writer) appendEncoded(buf []byte, m *Message) []byte {
	csid := m.csid()
	n := len(m.Payload)
	extended := m.Timestamp >= 0xFFFFFF

	// Basic header (fmt 0).
	switch {
	case csid < 64:
		buf = append(buf, byte(csid))
	case csid < 320:
		buf = append(buf, 0, byte(csid-64))
	default:
		v := csid - 64
		buf = append(buf, 1, byte(v), byte(v>>8))
	}

	ts := m.Timestamp
	if extended {
		ts = 0xFFFFFF
	}
	buf = appendU24(buf, ts)
	buf = appendU24(buf, uint32(n))
	buf = append(buf, m.TypeID)
	buf = append(buf, byte(m.StreamID), byte(m.StreamID>>8), byte(m.StreamID>>16), byte(m.StreamID>>24))
	if extended {
		buf = appendU32(buf, m.Timestamp)
	}

	chunkSize := int(w.chunkSize)
	payload := m.Payload
	for {
		k := len(payload)
		if k > chunkSize {
			k = chunkSize
		}
		buf = append(buf, payload[:k]...)
		payload = payload[k:]
		if len(payload) == 0 {
			break
		}
		// Continuation chunks use fmt=3.
		switch {
		case csid < 64:
			buf = append(buf, 0xC0|byte(csid)) // fmt=3 continuation
		case csid < 320:
			buf = append(buf, 0xC0, byte(csid-64))
		default:
			v := csid - 64
			buf = append(buf, 0xC1, byte(v), byte(v>>8))
		}
		if extended {
			buf = appendU32(buf, m.Timestamp)
		}
	}
	return buf
}

// csid picks the chunk stream id for outbound messages. Audio/video/commands
// use distinct streams so nothing ever queues behind a big media chunk.
func (m *Message) csid() uint32 {
	switch m.TypeID {
	case MsgAudio:
		return 4
	case MsgVideo:
		return 6
	case MsgDataAMF0, MsgDataAMF3:
		return 5
	default:
		return 3
	}
}

// ---------- control helpers ----------

func (w *Writer) writeControl(typeID uint8, payload []byte) error {
	return w.WriteMessage(&Message{TypeID: typeID, StreamID: 0, Payload: payload})
}

func (w *Writer) SetChunkSizeMsg(n uint32) error {
	var p [4]byte
	binary.BigEndian.PutUint32(p[:], n)
	if err := w.writeControl(MsgSetChunkSize, p[:]); err != nil {
		return err
	}
	w.SetChunkSize(n)
	return nil
}

func (w *Writer) WindowAckSize(n uint32) error {
	var p [4]byte
	binary.BigEndian.PutUint32(p[:], n)
	return w.writeControl(MsgWindowAckSize, p[:])
}

func (w *Writer) SetPeerBandwidth(n uint32, limit uint8) error {
	var p [5]byte
	binary.BigEndian.PutUint32(p[:4], n)
	p[4] = limit
	return w.writeControl(MsgSetPeerBandwidth, p[:])
}

func (w *Writer) Ack(seq uint32) error {
	var p [4]byte
	binary.BigEndian.PutUint32(p[:], seq)
	return w.writeControl(MsgAck, p[:])
}

func (w *Writer) UserControl(event uint16, values ...uint32) error {
	p := make([]byte, 2+4*len(values))
	binary.BigEndian.PutUint16(p, event)
	for i, v := range values {
		binary.BigEndian.PutUint32(p[2+4*i:], v)
	}
	return w.writeControl(MsgUserControl, p)
}

func (w *Writer) Command(streamID uint32, vals ...any) error {
	payload, err := EncodeAMF0(vals...)
	if err != nil {
		return err
	}
	return w.WriteMessage(&Message{TypeID: MsgCommandAMF0, StreamID: streamID, Payload: payload})
}

func (w *Writer) Data(streamID uint32, vals ...any) error {
	payload, err := EncodeAMF0(vals...)
	if err != nil {
		return err
	}
	return w.WriteMessage(&Message{TypeID: MsgDataAMF0, StreamID: streamID, Payload: payload})
}
