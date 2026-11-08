package rtmp

import (
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	// ErrAlreadyPublishing is returned when a second publisher claims a live stream.
	ErrAlreadyPublishing = errors.New("stream is already being published")
	// ErrStreamFull is returned when the per-stream viewer limit is reached.
	ErrStreamFull = errors.New("stream viewer limit reached")
)

// viewer is one downstream consumer (RTMP player or HTTP-FLV client).
type viewer interface {
	enqueue(*Message)
	onUnpublish()
	onPublish()
	shutdown()
	info() viewerInfo
}

type viewerInfo struct {
	Kind    string `json:"kind"`
	Addr    string `json:"addr"`
	Sent    uint64 `json:"sent_msgs"`
	Dropped uint64 `json:"dropped_msgs"`
	Since   int64  `json:"since_unix"`
}

// msgQueue decouples the publisher goroutine from slow viewers.
//
// It implements "drop to keyframe": when a viewer cannot keep up, its backlog is
// discarded and it resumes on the next keyframe. This bounds memory per viewer
// (QueueBytes) and prevents one slow client from adding latency for everyone
// else - the single most important property of a low-latency relay.
type msgQueue struct {
	mu       sync.Mutex
	cond     *sync.Cond
	q        []*Message
	bytes    int
	maxBytes int
	closed   bool
	waitKey  bool
	dropped  uint64
}

func newMsgQueue(maxBytes int) *msgQueue {
	if maxBytes <= 0 {
		maxBytes = 8 << 20
	}
	q := &msgQueue{maxBytes: maxBytes}
	q.cond = sync.NewCond(&q.mu)
	return q
}

func (q *msgQueue) enqueue(m *Message) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	media := m.TypeID == MsgAudio || m.TypeID == MsgVideo
	if q.waitKey {
		if m.TypeID == MsgVideo && m.IsKeyframe {
			q.waitKey = false
		} else if media {
			q.dropped++
			return
		}
	}
	q.q = append(q.q, m)
	q.bytes += len(m.Payload)
	if q.bytes > q.maxBytes {
		q.dropped++
		q.bytes = 0
		if m.TypeID == MsgVideo && m.IsKeyframe {
			// Keep the keyframe we just received so the viewer can resync now.
			q.q = append(q.q[:0], m)
			q.bytes = len(m.Payload)
			q.waitKey = false
		} else {
			q.q = q.q[:0]
			q.waitKey = true
		}
	}
	q.cond.Signal()
}

// pop blocks until at least one message is queued or the queue is closed.
func (q *msgQueue) pop() ([]*Message, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.q) == 0 && !q.closed {
		q.cond.Wait()
	}
	if len(q.q) == 0 {
		return nil, false
	}
	batch := q.q
	q.q = nil
	q.bytes = 0
	return batch, true
}

func (q *msgQueue) close() {
	q.mu.Lock()
	q.closed = true
	q.q = nil
	q.bytes = 0
	q.mu.Unlock()
	q.cond.Broadcast()
}

func (q *msgQueue) dropCount() uint64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.dropped
}

// publisherRef is the currently publishing connection.
type publisherRef struct {
	conn  *Conn
	addr  string
	since time.Time
}

// MediaInfo is the subset of onMetaData / sequence headers the dashboard shows.
type MediaInfo struct {
	VideoWidth   int     `json:"video_width"`
	VideoHeight  int     `json:"video_height"`
	VideoFPS     float64 `json:"video_fps"`
	VideoCodec   string  `json:"video_codec"`
	AudioCodec   string  `json:"audio_codec"`
	VideoBitrate int     `json:"video_bitrate_kbps"`
	AudioBitrate int     `json:"audio_bitrate_kbps"`
}

// Stream is a single live channel: one publisher, N viewers, plus caches that
// let a new viewer start decoding immediately.
type Stream struct {
	Name string

	mu            sync.RWMutex
	pub           *publisherRef
	everPublished bool
	subs          map[viewer]struct{}
	meta          *Message
	audioSeq      *Message
	videoSeq      *Message
	gop           []*Message
	gopBytes      int
	gopMax        int
	created       time.Time
	publishedAt   time.Time
	info          MediaInfo

	// cumulative counters (never reset, so dashboards can compute averages)
	bytesIn     atomic.Uint64
	msgsIn      atomic.Uint64
	videoFrames atomic.Uint64
	audioFrames atomic.Uint64
	keyframes   atomic.Uint64

	// instantaneous values, refreshed by the server sampler
	bitrateKbps atomic.Int64
	fpsNow      atomic.Int64

	// sampler scratch (only touched by the sampler goroutine)
	lastBytes  uint64
	lastFrames uint64
	lastSample time.Time
}

func newStream(name string, gopMax int) *Stream {
	if gopMax <= 0 {
		gopMax = 4 << 20
	}
	return &Stream{
		Name:    name,
		subs:    make(map[viewer]struct{}),
		gopMax:  gopMax,
		created: time.Now(),
	}
}

// isLive reports whether a publisher is currently attached.
func (s *Stream) isLive() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.pub != nil
}

func (s *Stream) publisherAddr() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.pub == nil {
		return ""
	}
	return s.pub.addr
}

func (s *Stream) viewerCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.subs)
}

// setPublisher attaches a publisher, resetting per-session caches.
func (s *Stream) setPublisher(c *Conn) error {
	s.mu.Lock()
	if s.pub != nil && s.pub.conn != c {
		s.mu.Unlock()
		return ErrAlreadyPublishing
	}
	// A restarted publisher begins at timestamp 0. Viewers that already played
	// the previous session would see the timeline jump backwards, so drop them:
	// they reconnect in a second and get a clean, cached start.
	var stale []viewer
	if s.everPublished {
		for v := range s.subs {
			stale = append(stale, v)
			delete(s.subs, v)
		}
	}
	s.pub = &publisherRef{conn: c, addr: c.nc.RemoteAddr().String(), since: time.Now()}
	s.publishedAt = time.Now()
	s.everPublished = true
	s.meta, s.audioSeq, s.videoSeq = nil, nil, nil
	s.gop = make([]*Message, 0, 64)
	s.gopBytes = 0
	s.mu.Unlock()

	for _, v := range stale {
		v.onUnpublish()
		v.shutdown()
	}
	return nil
}

// removePublisher detaches c if it still owns the stream. Returns true when the
// stream just went offline.
func (s *Stream) removePublisher(c *Conn) bool {
	s.mu.Lock()
	if s.pub == nil || s.pub.conn != c {
		s.mu.Unlock()
		return false
	}
	s.pub = nil
	s.meta, s.audioSeq, s.videoSeq = nil, nil, nil
	s.gop, s.gopBytes = nil, 0
	viewers := make([]viewer, 0, len(s.subs))
	for v := range s.subs {
		viewers = append(viewers, v)
	}
	s.mu.Unlock()

	// Notify outside the lock: onUnpublish may write to a socket.
	for _, v := range viewers {
		v.onUnpublish()
	}
	return true
}

// subscribe registers v and pushes the startup burst (metadata, codec config,
// cached GOP) into its queue while holding the lock, so no live frame can slip
// in front of the keyframe.
func (s *Stream) subscribe(v viewer) (live bool, pubAddr string, viewers int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.meta != nil {
		v.enqueue(s.meta)
	}
	if s.videoSeq != nil {
		v.enqueue(s.videoSeq)
	}
	if s.audioSeq != nil {
		v.enqueue(s.audioSeq)
	}
	for _, m := range s.gop {
		v.enqueue(m)
	}
	s.subs[v] = struct{}{}
	live = s.pub != nil
	if s.pub != nil {
		pubAddr = s.pub.addr
	}
	return live, pubAddr, len(s.subs)
}

func (s *Stream) unsubscribe(v viewer) {
	s.mu.Lock()
	_, ok := s.subs[v]
	if ok {
		delete(s.subs, v)
	}
	s.mu.Unlock()
	if ok {
		v.onUnpublish()
	}
}

func (s *Stream) notifyPublish() {
	s.mu.RLock()
	for v := range s.subs {
		v.onPublish()
	}
	s.mu.RUnlock()
}

// onMedia ingests one published message: updates caches, then fans out.
//
// Fan-out happens under a read lock and only appends to per-viewer queues, so a
// publisher never blocks on a socket. The payload buffer is shared by all
// viewers - at 1 Gbps that is the difference between working and melting.
func (s *Stream) onMedia(m *Message) {
	s.mu.Lock()

	switch m.TypeID {
	case MsgAudio:
		s.audioFrames.Add(1)
		if m.IsSeqHeader && s.audioSeq == nil {
			s.audioSeq = m
		}
	case MsgVideo:
		s.videoFrames.Add(1)
		if m.IsSeqHeader && s.videoSeq == nil {
			s.videoSeq = m
		}
		if m.IsKeyframe {
			s.keyframes.Add(1)
		}
	}
	s.bytesIn.Add(uint64(len(m.Payload)))
	s.msgsIn.Add(1)

	media := m.TypeID == MsgAudio || m.TypeID == MsgVideo || m.TypeID == MsgDataAMF0 || m.TypeID == MsgDataAMF3
	if media && !m.IsSeqHeader {
		if m.TypeID == MsgVideo && m.IsKeyframe {
			s.gop = make([]*Message, 0, 64)
			s.gopBytes = 0
		}
		if s.gop != nil {
			if s.gopBytes+len(m.Payload) > s.gopMax {
				// Stop caching until the next keyframe to stay memory bounded.
				s.gop, s.gopBytes = nil, 0
			} else {
				s.gop = append(s.gop, m)
				s.gopBytes += len(m.Payload)
			}
		}
	}

	for v := range s.subs {
		v.enqueue(m)
	}
	s.mu.Unlock()
}

func (s *Stream) setMeta(m *Message) {
	info := parseMediaInfo(m.Payload)
	s.mu.Lock()
	s.meta = m
	if info.VideoWidth > 0 {
		s.info = info
	} else {
		// Metadata may be minimal; keep whatever we already learned.
		if s.info.VideoCodec == "" {
			s.info = info
		}
	}
	s.mu.Unlock()
}

func (s *Stream) mediaInfo() MediaInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.info
}

// snapshot is used by the HTTP API.
func (s *Stream) snapshot() StreamSnapshot {
	s.mu.RLock()
	live := s.pub != nil
	pubAddr := ""
	var pubSince int64
	if live {
		pubAddr = s.pub.addr
		pubSince = s.pub.since.Unix()
	}
	info := s.info
	viewers := len(s.subs)
	gopKB := s.gopBytes / 1024
	hasSeq := s.videoSeq != nil && s.audioSeq != nil
	s.mu.RUnlock()

	return StreamSnapshot{
		Name:          s.Name,
		Live:          live,
		PublisherAddr: pubAddr,
		PublisherSec:  pubSince,
		Viewers:       viewers,
		BitrateKbps:   int(s.bitrateKbps.Load()),
		FPS:           int(s.fpsNow.Load()),
		BytesIn:       s.bytesIn.Load(),
		MessagesIn:    s.msgsIn.Load(),
		VideoFrames:   s.videoFrames.Load(),
		AudioFrames:   s.audioFrames.Load(),
		Keyframes:     s.keyframes.Load(),
		GOPCacheKB:    gopKB,
		CodecReady:    hasSeq,
		Media:         info,
	}
}

// StreamSnapshot is the JSON shape exposed by /api/streams.
type StreamSnapshot struct {
	Name          string    `json:"name"`
	Live          bool      `json:"live"`
	PublisherAddr string    `json:"publisher_addr"`
	PublisherSec  int64     `json:"publisher_since"`
	Viewers       int       `json:"viewers"`
	BitrateKbps   int       `json:"bitrate_kbps"`
	FPS           int       `json:"fps"`
	BytesIn       uint64    `json:"bytes_in"`
	MessagesIn    uint64    `json:"messages_in"`
	VideoFrames   uint64    `json:"video_frames"`
	AudioFrames   uint64    `json:"audio_frames"`
	Keyframes     uint64    `json:"keyframes"`
	GOPCacheKB    int       `json:"gop_cache_kb"`
	CodecReady    bool      `json:"codec_ready"`
	Media         MediaInfo `json:"media"`
}

// RTMP player ---------------------------------------------------------------

// Player is an RTMP downstream connection.
type Player struct {
	c      *Conn
	stream *Stream
	q      *msgQueue
	since  time.Time
	sent   atomic.Uint64
}

func newPlayer(c *Conn, st *Stream, queueBytes int) *Player {
	return &Player{c: c, stream: st, q: newMsgQueue(queueBytes), since: time.Now()}
}

func (p *Player) enqueue(m *Message) { p.q.enqueue(m) }

// notify sends a status message without ever blocking the caller: it runs in its
// own goroutine with a short write deadline, because it is triggered from the
// publisher goroutine and one stalled viewer must not delay ingest.
func (p *Player) notify(code, desc string) {
	go func() {
		_ = p.c.nc.SetWriteDeadline(time.Now().Add(3 * time.Second))
		err := p.c.w.Command(1, "onStatus", 0, nil, Object{
			{Key: "level", Val: "status"},
			{Key: "code", Val: code},
			{Key: "description", Val: desc},
		})
		_ = p.c.nc.SetWriteDeadline(time.Time{})
		if err != nil {
			_ = p.c.Close()
		}
	}()
}

func (p *Player) onUnpublish() {
	// Keep the connection open so the viewer can pick the stream up again.
	p.notify("NetStream.Play.UnpublishNotify", "stream unpublished")
}

func (p *Player) onPublish() {
	p.notify("NetStream.Play.PublishNotify", "stream published")
}

func (p *Player) shutdown() { p.q.close() }

func (p *Player) info() viewerInfo {
	return viewerInfo{Kind: "rtmp", Addr: p.c.nc.RemoteAddr().String(), Sent: p.sent.Load(), Dropped: p.q.dropCount(), Since: p.since.Unix()}
}

// run is the per-viewer writer goroutine.
func (p *Player) run() {
	defer p.c.Close()
	for {
		batch, ok := p.q.pop()
		if !ok {
			return
		}
		for _, m := range batch {
			if m.StreamID != 1 {
				m = m.clone()
				m.StreamID = 1
			}
			if err := p.c.w.WriteMessage(m); err != nil {
				return
			}
		}
		p.sent.Add(uint64(len(batch)))
	}
}

// HTTP-FLV client -----------------------------------------------------------

// FLVClient streams the same messages as FLV tags over HTTP.
type FLVClient struct {
	st     *Stream
	w      io.Writer
	flush  func()
	q      *msgQueue
	addr   string
	since  time.Time
	sent   atomic.Uint64
	closed atomic.Bool
}

func newFLVClient(st *Stream, w io.Writer, flush func(), addr string, queueBytes int) *FLVClient {
	return &FLVClient{st: st, w: w, flush: flush, q: newMsgQueue(queueBytes), addr: addr, since: time.Now()}
}

func (f *FLVClient) enqueue(m *Message) { f.q.enqueue(m) }

func (f *FLVClient) onUnpublish() {
	// FLV has no in-band status channel - end the response so the player
	// reconnects and picks the stream up again.
	f.shutdown()
}

func (f *FLVClient) onPublish() {}

func (f *FLVClient) shutdown() {
	if f.closed.CompareAndSwap(false, true) {
		f.q.close()
	}
}

func (f *FLVClient) info() viewerInfo {
	return viewerInfo{Kind: "flv", Addr: f.addr, Sent: f.sent.Load(), Dropped: f.q.dropCount(), Since: f.since.Unix()}
}

// run pumps FLV tags until the client disconnects or the stream ends.
func (f *FLVClient) run() error {
	for {
		batch, ok := f.q.pop()
		if !ok {
			return nil
		}
		for _, m := range batch {
			if err := writeFLVTag(f.w, m); err != nil {
				return err
			}
			f.sent.Add(1)
		}
		f.flush()
	}
}

// helpers -------------------------------------------------------------------

// splitStreamName separates "stream?key=secret" into name and raw query.
func splitStreamName(raw string) (name, query string) {
	if i := strings.IndexByte(raw, '?'); i >= 0 {
		return raw[:i], raw[i+1:]
	}
	return raw, ""
}

// streamKey builds the registry key from the app and the requested stream name.
// The app is part of the key, so rtmp://host/live/test and rtmp://host/test are
// distinct streams and the HTTP-FLV path "/live/test.flv" maps 1:1.
func streamKey(app, raw string) string {
	name := strings.Trim(strings.TrimSpace(raw), "/")
	if app != "" && strings.HasPrefix(name, app+"/") {
		name = strings.TrimPrefix(name, app+"/")
	}
	if name == "" {
		return ""
	}
	if app == "" {
		return name
	}
	return strings.Trim(app, "/") + "/" + name
}

// parseMediaInfo digs the useful fields out of an onMetaData payload.
func parseMediaInfo(payload []byte) MediaInfo {
	vals, err := DecodeAMF0(payload)
	if err != nil || len(vals) == 0 {
		return MediaInfo{}
	}
	var obj map[string]any
	for i := len(vals) - 1; i >= 0; i-- {
		if m, ok := vals[i].(map[string]any); ok {
			obj = m
			break
		}
	}
	if obj == nil {
		return MediaInfo{}
	}
	info := MediaInfo{}
	info.VideoWidth = int(numField(obj, "width"))
	info.VideoHeight = int(numField(obj, "height"))
	info.VideoFPS = numField(obj, "framerate")
	if info.VideoFPS == 0 {
		info.VideoFPS = numField(obj, "fps")
	}
	info.VideoBitrate = int(numField(obj, "videodatarate"))
	info.AudioBitrate = int(numField(obj, "audiodatarate"))
	info.VideoCodec = codecName(int(numField(obj, "videocodecid")))
	info.AudioCodec = audioCodecName(int(numField(obj, "audiocodecid")))
	return info
}

func numField(obj map[string]any, key string) float64 {
	if v, ok := obj[key]; ok {
		if f, ok := v.(float64); ok {
			return f
		}
	}
	return 0
}

func codecName(id int) string {
	switch id {
	case 7:
		return "H.264"
	case 12:
		return "HEVC"
	case 13:
		return "AV1"
	case 2:
		return "Sorenson H.263"
	case 4:
		return "VP6"
	default:
		return ""
	}
}

func audioCodecName(id int) string {
	switch id {
	case 10:
		return "AAC"
	case 2:
		return "MP3"
	case 0:
		return "PCM"
	case 11:
		return "Speex"
	default:
		return ""
	}
}
