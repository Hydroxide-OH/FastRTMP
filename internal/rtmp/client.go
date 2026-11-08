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
	"time"
)

// Client is a minimal RTMP client used by the load/latency test tool and the
// integration tests. It speaks the same subset of RTMP that OBS and ffmpeg use.
type Client struct {
	conn net.Conn
	br   *bufio.Reader
	r    *Reader
	w    *Writer

	URL      string
	App      string
	Stream   string
	StreamID uint32

	windowAck uint32
	lastAck   uint64
}

// Dial opens a TCP connection and performs the RTMP handshake.
func Dial(rawURL string) (*Client, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "rtmp" {
		return nil, fmt.Errorf("unsupported scheme %q (want rtmp://)", u.Scheme)
	}
	host := u.Host
	if _, _, err := net.SplitHostPort(host); err != nil {
		host = net.JoinHostPort(host, "1935")
	}
	path := strings.Trim(u.Path, "/")
	app, stream := path, ""
	if i := strings.IndexByte(path, '/'); i >= 0 {
		app, stream = path[:i], path[i+1:]
	}
	if u.RawQuery != "" {
		stream += "?" + u.RawQuery
	}

	conn, err := net.DialTimeout("tcp", host, 5*time.Second)
	if err != nil {
		return nil, err
	}
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
	}
	c := &Client{
		conn:     conn,
		br:       bufio.NewReaderSize(conn, 64<<10),
		URL:      rawURL,
		App:      app,
		Stream:   stream,
		StreamID: 1,
	}
	c.r = NewReader(c.br)
	c.w = NewWriter(conn)
	if err := c.handshake(); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return c, nil
}

func (c *Client) handshake() error {
	c1 := make([]byte, handshakeSize)
	binary.BigEndian.PutUint32(c1[0:4], uint32(time.Now().Unix()))
	if _, err := rand.Read(c1[8:]); err != nil {
		return err
	}
	if _, err := c.conn.Write(append([]byte{3}, c1...)); err != nil {
		return err
	}

	s0 := make([]byte, 1)
	if _, err := io.ReadFull(c.br, s0); err != nil {
		return err
	}
	if s0[0] != 3 {
		return fmt.Errorf("bad server version %d", s0[0])
	}
	s1 := make([]byte, handshakeSize)
	if _, err := io.ReadFull(c.br, s1); err != nil {
		return err
	}
	if _, err := io.ReadFull(c.br, make([]byte, handshakeSize)); err != nil {
		return err
	}
	if _, err := c.conn.Write(s1); err != nil {
		return err
	}
	return nil
}

func (c *Client) Close() error { return c.conn.Close() }

// Connect performs the NetConnection handshake.
func (c *Client) Connect() error {
	tcURL := fmt.Sprintf("rtmp://%s/%s", c.conn.RemoteAddr().String(), c.App)
	if err := c.w.Command(0, "connect", float64(1), Object{
		{Key: "app", Val: c.App},
		{Key: "type", Val: "nonprivate"},
		{Key: "flashVer", Val: "FMLE/3.0 (compatible; fastrtmp-test)"},
		{Key: "tcUrl", Val: tcURL},
	}); err != nil {
		return err
	}
	vals, err := c.waitFor("_result", 10*time.Second)
	if err != nil {
		return err
	}
	if len(vals) < 4 {
		return errors.New("short _result")
	}
	if obj, ok := vals[3].(map[string]any); ok {
		if code, _ := obj["code"].(string); code != "NetConnection.Connect.Success" {
			return fmt.Errorf("connect rejected: %s", code)
		}
	}
	// Bigger chunks cut per-message overhead by ~30x for video frames.
	if err := c.w.SetChunkSizeMsg(4096); err != nil {
		return err
	}
	return nil
}

// CreateStream allocates the message stream id used for publish/play.
func (c *Client) CreateStream() error {
	if err := c.w.Command(0, "createStream", float64(2), nil); err != nil {
		return err
	}
	vals, err := c.waitFor("_result", 10*time.Second)
	if err != nil {
		return err
	}
	if len(vals) >= 4 {
		if f, ok := vals[3].(float64); ok && f > 0 {
			c.StreamID = uint32(f)
		}
	}
	return nil
}

// Publish starts publishing and waits for NetStream.Publish.Start.
func (c *Client) Publish(name string) error {
	if err := c.w.Command(c.StreamID, "publish", float64(3), nil, name, "live"); err != nil {
		return err
	}
	code, desc, err := c.waitStatus("NetStream.Publish.Start", 10*time.Second)
	if err != nil {
		return fmt.Errorf("publish: %w (code=%s desc=%s)", err, code, desc)
	}
	return nil
}

// Play starts playing and waits for NetStream.Play.Start.
func (c *Client) Play(name string) error {
	if err := c.w.Command(c.StreamID, "play", float64(3), nil, name, float64(-2)); err != nil {
		return err
	}
	code, desc, err := c.waitStatus("NetStream.Play.Start", 10*time.Second)
	if err != nil {
		return fmt.Errorf("play: %w (code=%s desc=%s)", err, code, desc)
	}
	return nil
}

func (c *Client) waitStatus(want string, timeout time.Duration) (code, desc string, err error) {
	deadline := time.Now().Add(timeout)
	for {
		msg, err := c.ReadMessage(deadline)
		if err != nil {
			return "", "", err
		}
		if msg.TypeID != MsgCommandAMF0 && msg.TypeID != MsgCommandAMF3 {
			continue
		}
		vals, err := DecodeAMF0(msg.Payload)
		if err != nil || len(vals) == 0 {
			continue
		}
		if name, _ := vals[0].(string); name != "onStatus" {
			continue
		}
		var obj map[string]any
		for i := len(vals) - 1; i >= 0; i-- {
			if m, ok := vals[i].(map[string]any); ok {
				obj = m
				break
			}
		}
		code, _ = obj["code"].(string)
		desc, _ = obj["description"].(string)
		if code == want {
			return code, desc, nil
		}
		if level, _ := obj["level"].(string); level == "error" {
			return code, desc, fmt.Errorf("server error %s", code)
		}
	}
}

func (c *Client) waitFor(command string, timeout time.Duration) ([]any, error) {
	deadline := time.Now().Add(timeout)
	for {
		msg, err := c.ReadMessage(deadline)
		if err != nil {
			return nil, err
		}
		if msg.TypeID != MsgCommandAMF0 && msg.TypeID != MsgCommandAMF3 {
			continue
		}
		vals, err := DecodeAMF0(msg.Payload)
		if err != nil || len(vals) == 0 {
			continue
		}
		if name, _ := vals[0].(string); name == command {
			return vals, nil
		}
	}
}

// ReadMessage returns the next message, transparently handling protocol control
// messages and sending acknowledgements so long downloads do not stall.
func (c *Client) ReadMessage(deadline time.Time) (*Message, error) {
	for {
		_ = c.conn.SetReadDeadline(deadline)
		msg, err := c.r.ReadMessage()
		if err != nil {
			return nil, err
		}
		switch msg.TypeID {
		case MsgSetChunkSize:
			if len(msg.Payload) >= 4 {
				_ = c.r.SetChunkSize(binary.BigEndian.Uint32(msg.Payload[:4]))
			}
			continue
		case MsgWindowAckSize:
			if len(msg.Payload) >= 4 {
				c.windowAck = binary.BigEndian.Uint32(msg.Payload[:4])
			}
			continue
		case MsgAbort, MsgUserControl, MsgAck, MsgSetPeerBandwidth:
			if c.windowAck > 0 {
				if total := c.r.TotalRead(); total-c.lastAck >= uint64(c.windowAck) {
					c.lastAck = total
					_ = c.w.Ack(uint32(total))
				}
			}
			continue
		}
		return msg, nil
	}
}

// NextMedia blocks until an audio/video message arrives, skipping commands,
// metadata and control traffic.
func (c *Client) NextMedia(timeout time.Duration) (*Message, error) {
	deadline := time.Now().Add(timeout)
	for {
		msg, err := c.ReadMessage(deadline)
		if err != nil {
			return nil, err
		}
		if msg.TypeID == MsgAudio || msg.TypeID == MsgVideo {
			return msg, nil
		}
	}
}

// ---------- publisher helpers ----------

// SendMetadata publishes a minimal onMetaData.
func (c *Client) SendMetadata(width, height int, fps float64) error {
	return c.w.Data(c.StreamID, "@setDataFrame", "onMetaData", Object{
		{Key: "width", Val: float64(width)},
		{Key: "height", Val: float64(height)},
		{Key: "framerate", Val: fps},
		{Key: "videocodecid", Val: float64(7)},
		{Key: "audiocodecid", Val: float64(10)},
		{Key: "encoder", Val: "fastrtmp-test"},
	})
}

// SendAVCConfig publishes a syntactically valid AVC sequence header.
func (c *Client) SendAVCConfig() error {
	payload := []byte{0x17, 0x00, 0x00, 0x00, 0x00,
		0x01, 0x42, 0x00, 0x1E, 0xFF, 0xE1, 0x00, 0x08,
		0x67, 0x42, 0x00, 0x1E, 0x95, 0xA8, 0x28, 0x0F, 0x01, 0x00, 0x04, 0x68, 0xCE, 0x3C, 0x80}
	return c.WriteVideo(0, payload)
}

// SendAACConfig publishes a syntactically valid AAC sequence header.
func (c *Client) SendAACConfig() error {
	return c.WriteAudio(0, []byte{0xAF, 0x00, 0x12, 0x10})
}

// WriteVideo sends a video tag.
func (c *Client) WriteVideo(ts uint32, payload []byte) error {
	return c.w.WriteMessage(&Message{TypeID: MsgVideo, StreamID: c.StreamID, Timestamp: ts, Payload: payload})
}

// WriteAudio sends an audio tag.
func (c *Client) WriteAudio(ts uint32, payload []byte) error {
	return c.w.WriteMessage(&Message{TypeID: MsgAudio, StreamID: c.StreamID, Timestamp: ts, Payload: payload})
}

// WriteData sends a data (script) message.
func (c *Client) WriteData(ts uint32, vals ...any) error {
	payload, err := EncodeAMF0(vals...)
	if err != nil {
		return err
	}
	return c.w.WriteMessage(&Message{TypeID: MsgDataAMF0, StreamID: c.StreamID, Timestamp: ts, Payload: payload})
}
