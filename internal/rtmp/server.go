package rtmp

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Config controls the RTMP ingest server and its auxiliary HTTP endpoints.
type Config struct {
	RTMPAddr string // ":1935"
	HTTPAddr string // ":8080", empty disables HTTP

	ChunkSize     uint32 // outbound chunk size, 4096 is a good default
	QueueBytes    int    // per-viewer send backlog before drop-to-keyframe
	GOPCacheBytes int    // per-stream GOP cache budget

	ReadTimeout      time.Duration // idle connection timeout, 0 disables
	HandshakeTimeout time.Duration

	MaxConns         int // 0 = unlimited
	MaxSubsPerStream int // 0 = unlimited

	PublishKey string // when set, publish URLs must carry ?key=<value>
	PlayKey    string

	Dashboard bool
	Logger    *slog.Logger
}

func (c *Config) withDefaults() {
	if c.RTMPAddr == "" {
		c.RTMPAddr = ":1935"
	}
	if c.ChunkSize == 0 {
		c.ChunkSize = 4096
	}
	if c.QueueBytes == 0 {
		c.QueueBytes = 8 << 20
	}
	if c.GOPCacheBytes == 0 {
		c.GOPCacheBytes = 4 << 20
	}
	if c.ReadTimeout == 0 {
		c.ReadTimeout = 30 * time.Second
	}
	if c.HandshakeTimeout == 0 {
		c.HandshakeTimeout = 10 * time.Second
	}
	if c.Logger == nil {
		c.Logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	}
}

// Server owns the listener, the stream registry and the HTTP surface.
type Server struct {
	cfg Config
	log *slog.Logger

	ln      net.Listener
	httpSrv *http.Server

	mu      sync.RWMutex
	streams map[string]*Stream

	started    time.Time
	conns      atomic.Int64
	totalConns atomic.Uint64
	bytesIn    atomic.Uint64
	closed     atomic.Bool

	wg       sync.WaitGroup
	stopOnce sync.Once
	done     chan struct{}
}

func New(cfg Config) *Server {
	cfg.withDefaults()
	return &Server{
		cfg:     cfg,
		log:     cfg.Logger,
		streams: make(map[string]*Stream),
		started: time.Now(),
		done:    make(chan struct{}),
	}
}

// Start binds both listeners and returns immediately.
func (s *Server) Start() error {
	ln, err := net.Listen("tcp", s.cfg.RTMPAddr)
	if err != nil {
		return fmt.Errorf("rtmp listen %s: %w", s.cfg.RTMPAddr, err)
	}
	s.ln = ln
	s.logf("RTMP listening on %s (tcp)", ln.Addr().String())

	go s.acceptLoop()

	if s.cfg.HTTPAddr != "" {
		hs, err := s.startHTTP()
		if err != nil {
			_ = ln.Close()
			return err
		}
		s.httpSrv = hs
	}

	go s.sampleLoop()
	go s.janitorLoop()
	return nil
}

// Addr returns the bound RTMP address (useful when using :0 in tests).
func (s *Server) Addr() string {
	if s.ln == nil {
		return s.cfg.RTMPAddr
	}
	return s.ln.Addr().String()
}

// HTTPAddr returns the bound HTTP address.
func (s *Server) HTTPAddr() string {
	if s.httpSrv == nil {
		return ""
	}
	return s.httpSrv.Addr
}

func (s *Server) acceptLoop() {
	var delay time.Duration
	for {
		nc, err := s.ln.Accept()
		if err != nil {
			if s.closed.Load() {
				return
			}
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				if delay < time.Second {
					delay = 5 * time.Millisecond
				} else {
					delay *= 2
				}
				time.Sleep(delay)
				continue
			}
			s.logf("accept error: %v", err)
			return
		}
		delay = 0

		if max := s.cfg.MaxConns; max > 0 && s.conns.Load() >= int64(max) {
			s.debugf("connection limit reached, rejecting %s", nc.RemoteAddr())
			_ = nc.Close()
			continue
		}

		if tc, ok := nc.(*net.TCPConn); ok {
			_ = tc.SetNoDelay(true) // latency: never coalesce small RTMP messages
			_ = tc.SetKeepAlive(true)
			_ = tc.SetKeepAlivePeriod(30 * time.Second)
			_ = tc.SetReadBuffer(256 << 10)
			_ = tc.SetWriteBuffer(256 << 10)
		}

		s.conns.Add(1)
		s.totalConns.Add(1)
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			newConn(s, nc).serve()
		}()
	}
}

func (s *Server) connClosed() {
	s.conns.Add(-1)
}

// Shutdown stops accepting, closes live connections and waits for cleanup.
func (s *Server) Shutdown(ctx context.Context) error {
	var err error
	s.stopOnce.Do(func() {
		s.closed.Store(true)
		if s.ln != nil {
			err = s.ln.Close()
		}
		if s.httpSrv != nil {
			_ = s.httpSrv.Shutdown(ctx)
		}
		close(s.done)

		s.mu.RLock()
		streams := make([]*Stream, 0, len(s.streams))
		for _, st := range s.streams {
			streams = append(streams, st)
		}
		s.mu.RUnlock()
		for _, st := range streams {
			st.mu.Lock()
			if st.pub != nil {
				_ = st.pub.conn.nc.Close()
			}
			for v := range st.subs {
				v.shutdown()
			}
			st.mu.Unlock()
		}
	})

	wait := make(chan struct{})
	go func() { s.wg.Wait(); close(wait) }()
	select {
	case <-wait:
	case <-ctx.Done():
		return ctx.Err()
	}
	return err
}

func (s *Server) isClosed() bool { return s.closed.Load() }

// ---------- stream registry ----------

func (s *Server) acquireStream(name string, c *Conn) (*Stream, error) {
	st := s.getOrCreate(name)
	if err := st.setPublisher(c); err != nil {
		return nil, err
	}
	return st, nil
}

func (s *Server) getOrCreate(name string) *Stream {
	s.mu.RLock()
	st := s.streams[name]
	s.mu.RUnlock()
	if st != nil {
		return st
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if st = s.streams[name]; st != nil {
		return st
	}
	st = newStream(name, s.cfg.GOPCacheBytes)
	s.streams[name] = st
	s.logf("stream created %q", name)
	return st
}

func (s *Server) viewStream(name string) *Stream {
	return s.getOrCreate(name)
}

// releaseStream removes a stream from the registry once nothing references it.
func (s *Server) releaseStream(st *Stream) {
	if st.isLive() || st.viewerCount() > 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if cur, ok := s.streams[st.Name]; !ok || cur != st {
		return
	}
	if st.isLive() || st.viewerCount() > 0 {
		return
	}
	delete(s.streams, st.Name)
}

func (s *Server) streamList() []*Stream {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Stream, 0, len(s.streams))
	for _, st := range s.streams {
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (s *Server) authorizePublish(query string) bool {
	if s.cfg.PublishKey == "" {
		return true
	}
	got := queryValue(query, "key")
	return subtle.ConstantTimeCompare([]byte(got), []byte(s.cfg.PublishKey)) == 1
}

func (s *Server) authorizePlay(query string) bool {
	if s.cfg.PlayKey == "" {
		return true
	}
	got := queryValue(query, "key")
	return subtle.ConstantTimeCompare([]byte(got), []byte(s.cfg.PlayKey)) == 1
}

// ---------- stats ----------

// ServerStats is the payload of /api/stats.
type ServerStats struct {
	UptimeSec    int64            `json:"uptime_sec"`
	Connections  int64            `json:"connections"`
	TotalConns   uint64           `json:"total_connections"`
	Streams      []StreamSnapshot `json:"streams"`
	StreamCount  int              `json:"stream_count"`
	LiveStreams  int              `json:"live_streams"`
	TotalViewers int              `json:"total_viewers"`
	GoRoutines   int              `json:"goroutines"`
	HeapMB       float64          `json:"heap_mb"`
}

func (s *Server) Stats() ServerStats {
	var st ServerStats
	st.UptimeSec = int64(time.Since(s.started).Seconds())
	st.Connections = s.conns.Load()
	st.TotalConns = s.totalConns.Load()
	streams := s.streamList()
	st.Streams = make([]StreamSnapshot, 0, len(streams))
	for _, stream := range streams {
		snap := stream.snapshot()
		st.Streams = append(st.Streams, snap)
		st.TotalViewers += snap.Viewers
		if snap.Live {
			st.LiveStreams++
		}
	}
	st.StreamCount = len(st.Streams)
	st.GoRoutines = goRoutines()
	st.HeapMB = heapMB()
	return st
}

// janitorLoop drops registry entries that nobody references any more. Without
// it, a client that plays random non-existent names could grow the map forever.
func (s *Server) janitorLoop() {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-t.C:
			now := time.Now()
			s.mu.RLock()
			candidates := make([]*Stream, 0, len(s.streams))
			for _, st := range s.streams {
				candidates = append(candidates, st)
			}
			s.mu.RUnlock()
			for _, st := range candidates {
				st.mu.RLock()
				idle := st.pub == nil && len(st.subs) == 0 && now.Sub(st.created) > time.Minute
				st.mu.RUnlock()
				if idle {
					s.releaseStream(st)
				}
			}
		}
	}
}

// sampleLoop refreshes per-stream bitrate and fps once a second.
func (s *Server) sampleLoop() {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-s.done:
			return
		case now := <-t.C:
			for _, st := range s.streamList() {
				bytes := st.bytesIn.Load()
				frames := st.videoFrames.Load()
				dt := now.Sub(st.lastSample).Seconds()
				if st.lastSample.IsZero() || dt <= 0 {
					st.lastSample, st.lastBytes, st.lastFrames = now, bytes, frames
					continue
				}
				deltaBytes := bytes - st.lastBytes
				deltaFrames := frames - st.lastFrames
				st.bitrateKbps.Store(int64(float64(deltaBytes*8) / dt / 1000))
				st.fpsNow.Store(int64(float64(deltaFrames) / dt))
				st.lastSample, st.lastBytes, st.lastFrames = now, bytes, frames
			}
		}
	}
}

// ---------- HTTP API ----------

func (s *Server) startHTTP() (*http.Server, error) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/live/", s.handleFLV)
	mux.HandleFunc("/flv/", s.handleFLV)
	mux.HandleFunc("/api/stats", s.handleStats)
	mux.HandleFunc("/api/streams", s.handleStats)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("ok\n"))
	})

	ln, err := net.Listen("tcp", s.cfg.HTTPAddr)
	if err != nil {
		return nil, fmt.Errorf("http listen %s: %w", s.cfg.HTTPAddr, err)
	}
	srv := &http.Server{
		Addr:              ln.Addr().String(),
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		// No WriteTimeout: FLV responses are long lived by design.
	}
	s.logf("HTTP listening on %s (dashboard + http-flv)", ln.Addr().String())
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) && !s.closed.Load() {
			s.logf("http server stopped: %v", err)
		}
	}()
	return srv, nil
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(s.Stats())
}

// ---------- logging ----------

func (s *Server) logf(format string, args ...any) {
	s.log.Info(fmt.Sprintf(format, args...))
}

func (s *Server) debugf(format string, args ...any) {
	s.log.Debug(fmt.Sprintf(format, args...))
}
