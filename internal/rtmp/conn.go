package rtmp

import (
	"bufio"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"
)

const handshakeSize = 1536

// Conn is one RTMP connection: handshake, command state machine and, depending
// on the role, the ingest loop (publisher) or a writer goroutine (player).
type Conn struct {
	srv *Server
	nc  net.Conn
	br  *bufio.Reader
	r   *Reader
	w   *Writer

	app      string
	streamID uint32

	stream  *Stream
	player  *Player
	pubName string

	windowAck uint32
	lastAck   uint64

	publishing bool
	playing    bool

	closeOnce sync.Once
}

func newConn(srv *Server, nc net.Conn) *Conn {
	br := bufio.NewReaderSize(nc, 64<<10)
	return &Conn{
		srv:      srv,
		nc:       nc,
		br:       br,
		r:        NewReader(br),
		w:        NewWriter(nc),
		streamID: 1,
	}
}

func (c *Conn) remoteAddr() string { return c.nc.RemoteAddr().String() }

// serve is the connection's read loop. It exits on any read error, which also
// covers client disconnects and idle timeouts.
func (c *Conn) serve() {
	defer c.Close()

	_ = c.nc.SetDeadline(time.Now().Add(c.srv.cfg.HandshakeTimeout))
	if err := c.handshake(); err != nil {
		c.srv.debugf("handshake failed from %s: %v", c.remoteAddr(), err)
		return
	}
	_ = c.nc.SetDeadline(time.Time{})

	for {
		if t := c.srv.cfg.ReadTimeout; t > 0 {
			_ = c.nc.SetReadDeadline(time.Now().Add(t))
		}
		msg, err := c.r.ReadMessage()
		if err != nil {
			if !errors.Is(err, io.EOF) && !c.srv.isClosed() {
				c.srv.debugf("read from %s: %v", c.remoteAddr(), err)
			}
			return
		}
		if err := c.handleMessage(msg); err != nil {
			c.srv.debugf("protocol error from %s: %v", c.remoteAddr(), err)
			return
		}
		c.maybeAck()
	}
}

// handshake implements the RTMP simple handshake, which every mainstream
// encoder (OBS, ffmpeg, XSplit, hardware encoders) accepts. Clients that offer
// the complex/digest variant still work: they fall back after validating S1.
func (c *Conn) handshake() error {
	var c0 [1]byte
	if _, err := io.ReadFull(c.br, c0[:]); err != nil {
		return err
	}
	if c0[0] != 3 {
		return fmt.Errorf("unsupported rtmp version %d", c0[0])
	}
	c1 := make([]byte, handshakeSize)
	if _, err := io.ReadFull(c.br, c1); err != nil {
		return err
	}

	out := make([]byte, 1+handshakeSize*2)
	out[0] = 3 // S0: version
	// S1: time=0, zero=0 (signals simple handshake), random payload.
	if _, err := rand.Read(out[9 : 1+handshakeSize]); err != nil {
		return err
	}
	copy(out[1+handshakeSize:], c1) // S2 echoes C1
	if _, err := c.nc.Write(out); err != nil {
		return err
	}

	c2 := make([]byte, handshakeSize)
	if _, err := io.ReadFull(c.br, c2); err != nil {
		return err
	}
	return nil
}

func (c *Conn) maybeAck() {
	if c.windowAck == 0 {
		return
	}
	total := c.r.TotalRead()
	if total-c.lastAck >= uint64(c.windowAck) {
		c.lastAck = total
		_ = c.w.Ack(uint32(total))
	}
}

func (c *Conn) handleMessage(m *Message) error {
	switch m.TypeID {
	case MsgSetChunkSize:
		if len(m.Payload) < 4 {
			return errors.New("short set chunk size")
		}
		n := binary.BigEndian.Uint32(m.Payload[:4]) & 0x7FFFFFFF
		return c.r.SetChunkSize(n)
	case MsgAbort:
		return nil
	case MsgAck, MsgSetPeerBandwidth, MsgUserControl, MsgSharedObject:
		return nil
	case MsgWindowAckSize:
		if len(m.Payload) >= 4 {
			c.windowAck = binary.BigEndian.Uint32(m.Payload[:4])
		}
		return nil
	case MsgCommandAMF0, MsgCommandAMF3:
		c.handleCommand(m)
		return nil
	case MsgDataAMF0, MsgDataAMF3:
		c.handleData(m)
		return nil
	case MsgAudio, MsgVideo:
		if c.publishing && c.stream != nil {
			classify(m)
			c.stream.onMedia(m)
		}
		return nil
	case MsgAggregate:
		c.handleAggregate(m)
		return nil
	default:
		return nil
	}
}

// classify caches per-message flags once, before fan-out.
func classify(m *Message) {
	switch m.TypeID {
	case MsgVideo:
		m.IsKeyframe = isVideoKeyframe(m.Payload)
		m.IsSeqHeader = isVideoSeqHeader(m.Payload)
	case MsgAudio:
		m.IsSeqHeader = isAudioSeqHeader(m.Payload)
	}
}

// handleAggregate unpacks an aggregate message into its sub-messages.
func (c *Conn) handleAggregate(m *Message) {
	if !c.publishing || c.stream == nil {
		return
	}
	p := m.Payload
	for len(p) >= 11 {
		typeID := p[0]
		size := int(p[1])<<16 | int(p[2])<<8 | int(p[3])
		ts := uint32(p[4])<<16 | uint32(p[5])<<8 | uint32(p[6])
		if size < 0 || 11+size+4 > len(p) {
			return
		}
		sub := &Message{
			TypeID:    typeID,
			StreamID:  m.StreamID,
			Timestamp: m.Timestamp + ts,
			Payload:   p[11 : 11+size],
		}
		classify(sub)
		c.stream.onMedia(sub)
		p = p[11+size+4:]
	}
}

func (c *Conn) handleData(m *Message) {
	if !c.publishing || c.stream == nil {
		return
	}
	// onMetaData may be wrapped as @setDataFrame.
	vals, err := DecodeAMF0(m.Payload)
	if err != nil || len(vals) == 0 {
		return
	}
	first, _ := vals[0].(string)
	if first == "@setDataFrame" || first == "onMetaData" {
		c.stream.setMeta(m)
	}
	c.stream.onMedia(m)
}

func (c *Conn) handleCommand(m *Message) {
	vals, err := DecodeAMF0(m.Payload)
	if err != nil || len(vals) < 1 {
		return
	}
	name, _ := vals[0].(string)
	tx := 0.0
	if len(vals) > 1 {
		if f, ok := vals[1].(float64); ok {
			tx = f
		}
	}

	switch name {
	case "connect":
		c.onConnect(vals, tx)
	case "releaseStream", "FCPublish", "FCUnpublish", "FCSubscribe":
		_ = c.w.Command(0, "_result", tx, nil, nil)
	case "createStream":
		c.streamID = 1
		_ = c.w.Command(0, "_result", tx, nil, float64(1))
	case "publish":
		c.onPublish(vals, tx)
	case "play":
		c.onPlay(vals, tx)
	case "deleteStream", "closeStream":
		c.onCloseStream()
	case "getStreamLength":
		_ = c.w.Command(0, "_result", tx, nil, float64(0))
	case "_checkbw", "ping":
		_ = c.w.Command(0, "_result", tx, nil, nil)
	case "pause":
		// Live streams cannot pause; acknowledge so clients do not hang.
		_ = c.w.Command(c.streamID, "onStatus", 0, nil, Object{
			{Key: "level", Val: "status"},
			{Key: "code", Val: "NetStream.Pause.Notify"},
			{Key: "description", Val: "live stream cannot pause"},
		})
	default:
		c.srv.debugf("unhandled command %q from %s", name, c.remoteAddr())
	}
}

func (c *Conn) onConnect(vals []any, tx float64) {
	if obj := argObject(vals, 2); obj != nil {
		if app, ok := obj["app"].(string); ok {
			c.app = app
		}
	}
	// Advertise our window and chunk size before answering.
	_ = c.w.WindowAckSize(2500000)
	_ = c.w.SetPeerBandwidth(2500000, 2)
	_ = c.w.SetChunkSizeMsg(c.srv.cfg.ChunkSize)

	err := c.w.Command(0, "_result", tx,
		Object{
			{Key: "fmsVer", Val: "FMS/3,5,7,7009"},
			{Key: "capabilities", Val: float64(31)},
			{Key: "mode", Val: float64(1)},
		},
		Object{
			{Key: "level", Val: "status"},
			{Key: "code", Val: "NetConnection.Connect.Success"},
			{Key: "description", Val: "Connection succeeded."},
			{Key: "objectEncoding", Val: float64(0)},
		})
	if err != nil {
		c.srv.debugf("connect reply failed: %v", err)
		return
	}
	c.srv.logf("connect app=%q addr=%s", c.app, c.remoteAddr())
}

func (c *Conn) onPublish(vals []any, tx float64) {
	raw := argString(vals, 3)
	if raw == "" {
		c.failPublish("NetStream.Publish.BadName", "empty stream name")
		return
	}
	name, query := splitStreamName(raw)
	name = streamKey(c.app, name)
	if name == "" {
		c.failPublish("NetStream.Publish.BadName", "empty stream name")
		return
	}
	if !c.srv.authorizePublish(query) {
		c.srv.logf("publish denied stream=%q addr=%s (bad key)", name, c.remoteAddr())
		c.failPublish("NetStream.Publish.Denied", "invalid publish key")
		return
	}

	st, err := c.srv.acquireStream(name, c)
	if err != nil {
		c.srv.logf("publish rejected stream=%q addr=%s: %v", name, c.remoteAddr(), err)
		if errors.Is(err, ErrAlreadyPublishing) {
			c.failPublish("NetStream.Publish.BadName", "stream already publishing")
		} else {
			c.failPublish("NetStream.Publish.Failed", err.Error())
		}
		return
	}

	c.stream = st
	c.pubName = name
	c.publishing = true

	_ = c.w.UserControl(ucStreamBegin, 1)
	_ = c.w.Command(1, "onStatus", 0, nil, Object{
		{Key: "level", Val: "status"},
		{Key: "code", Val: "NetStream.Publish.Start"},
		{Key: "description", Val: "Start publishing"},
	})
	st.notifyPublish()
	c.srv.logf("publish start stream=%q addr=%s", name, c.remoteAddr())
}

func (c *Conn) failPublish(code, desc string) {
	_ = c.w.Command(1, "onStatus", 0, nil, Object{
		{Key: "level", Val: "error"},
		{Key: "code", Val: code},
		{Key: "description", Val: desc},
	})
	// Give the client a moment to read the status before the socket closes.
	time.AfterFunc(500*time.Millisecond, func() { c.Close() })
}

func (c *Conn) onPlay(vals []any, tx float64) {
	raw := argString(vals, 3)
	name, query := splitStreamName(raw)
	name = streamKey(c.app, name)
	if name == "" {
		_ = c.w.Command(1, "onStatus", 0, nil, Object{
			{Key: "level", Val: "error"},
			{Key: "code", Val: "NetStream.Play.StreamNotFound"},
			{Key: "description", Val: "empty stream name"},
		})
		return
	}
	if !c.srv.authorizePlay(query) {
		_ = c.w.Command(1, "onStatus", 0, nil, Object{
			{Key: "level", Val: "error"},
			{Key: "code", Val: "NetStream.Play.Denied"},
			{Key: "description", Val: "invalid play key"},
		})
		return
	}

	st := c.srv.viewStream(name)
	if max := c.srv.cfg.MaxSubsPerStream; max > 0 && st.viewerCount() >= max {
		_ = c.w.Command(1, "onStatus", 0, nil, Object{
			{Key: "level", Val: "error"},
			{Key: "code", Val: "NetStream.Play.Failed"},
			{Key: "description", Val: "viewer limit reached"},
		})
		return
	}
	p := newPlayer(c, st, c.srv.cfg.QueueBytes)
	c.stream = st
	c.player = p
	c.playing = true

	_ = c.w.UserControl(ucStreamBegin, 1)
	_ = c.w.Command(1, "onStatus", 0, nil, Object{
		{Key: "level", Val: "status"},
		{Key: "code", Val: "NetStream.Play.Reset"},
		{Key: "description", Val: "Playing and resetting " + name},
	})
	_ = c.w.Command(1, "onStatus", 0, nil, Object{
		{Key: "level", Val: "status"},
		{Key: "code", Val: "NetStream.Play.Start"},
		{Key: "description", Val: "Playing " + name},
	})
	_ = c.w.Data(1, "|RtmpSampleAccess", true, true)

	live, pubAddr, viewers := st.subscribe(p)
	if live {
		c.srv.logf("play start stream=%q addr=%s publisher=%s viewers=%d", name, c.remoteAddr(), pubAddr, viewers)
	} else {
		c.srv.logf("play waiting stream=%q addr=%s viewers=%d", name, c.remoteAddr(), viewers)
		_ = c.w.Command(1, "onStatus", 0, nil, Object{
			{Key: "level", Val: "status"},
			{Key: "code", Val: "NetStream.Play.UnpublishNotify"},
			{Key: "description", Val: "waiting for publisher"},
		})
	}

	go p.run()
}

func (c *Conn) onCloseStream() {
	if c.player != nil {
		c.stream.unsubscribe(c.player)
		c.player.shutdown()
		c.player = nil
		c.playing = false
	}
}

// Close tears down the connection exactly once.
func (c *Conn) Close() error {
	c.closeOnce.Do(func() {
		if c.player != nil {
			c.player.shutdown()
			if c.stream != nil {
				c.stream.unsubscribe(c.player)
			}
		}
		if c.publishing && c.stream != nil {
			if c.stream.removePublisher(c) {
				c.srv.logf("publish stop stream=%q addr=%s", c.pubName, c.remoteAddr())
			}
			c.srv.releaseStream(c.stream)
		}
		_ = c.nc.Close()
		c.srv.connClosed()
	})
	return nil
}

// helpers -------------------------------------------------------------------

func argString(vals []any, i int) string {
	if i < len(vals) {
		if s, ok := vals[i].(string); ok {
			return s
		}
	}
	return ""
}

func argObject(vals []any, i int) map[string]any {
	if i < len(vals) {
		if m, ok := vals[i].(map[string]any); ok {
			return m
		}
	}
	return nil
}

func queryValue(raw, key string) string {
	if raw == "" {
		return ""
	}
	v, err := url.ParseQuery(raw)
	if err != nil {
		return ""
	}
	return v.Get(key)
}

// trimSlash normalises "/live/test" -> "live/test".
func trimSlash(s string) string {
	return strings.Trim(s, "/")
}
