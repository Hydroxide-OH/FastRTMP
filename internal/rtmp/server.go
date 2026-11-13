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
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Config controls the RTMP ingest server and its auxiliary HTTP endpoints.
type Config struct {
	// RTMPAddr is one address or a comma separated list. Besides ordinary
	// "host:port" entries it accepts the symbolic forms "public:1935" and
	// "lan:1935", which bind the internet-facing and the local network address
	// respectively. See ParseListenSpec.
	RTMPAddr string // ":1935"
	// HTTPAddr follows the same rules; empty disables HTTP.
	HTTPAddr string // ":8080"

	// PublicIP pins the address used by "public:" targets and the startup
	// banner. Empty means "detect it", "off" disables detection.
	PublicIP string
	// LANIP pins the address used by "lan:" targets. Empty means "pick one".
	LANIP string

	ChunkSize     uint32 // outbound chunk size, 4096 is a good default
	QueueBytes    int    // per-viewer send backlog before drop-to-keyframe
	GOPCacheBytes int    // per-stream GOP cache budget

	ReadTimeout      time.Duration // idle connection timeout, 0 disables
	HandshakeTimeout time.Duration

	MaxConns         int // 0 = unlimited
	MaxSubsPerStream int // 0 = unlimited

	PublishKey string // when set, publish URLs must carry ?key=<value>
	PlayKey    string

	// AdminPassword protects the dashboard. Empty keeps the dashboard open
	// (the original behaviour) but also disables runtime secret editing: a
	// passwordless panel must never hand out the publish key.
	AdminPassword string

	// Version is echoed by /api/config so the dashboard can show it.
	Version string

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

// boundListener pairs a resolved target with the listener that serves it.
type boundListener struct {
	target ListenTarget
	ln     net.Listener
}

func (b *boundListener) addr() string { return b.ln.Addr().String() }

// Server owns the listeners, the stream registry and the HTTP surface.
type Server struct {
	cfg Config
	log *slog.Logger

	// auth gates the dashboard; keys holds secrets the dashboard can rotate.
	auth *adminAuth
	keys keyStore

	lns      []*boundListener // RTMP listeners, primary first
	httpSrvs []*http.Server   // one per HTTP listener, all sharing a handler
	httpLns  []*boundListener

	pubOnce   sync.Once
	pubIP     string
	pubErr    error
	pubSource string

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
	srv := &Server{
		cfg:     cfg,
		log:     cfg.Logger,
		streams: make(map[string]*Stream),
		started: time.Now(),
		done:    make(chan struct{}),
	}
	srv.auth = newAdminAuth(cfg.AdminPassword)
	srv.keys.setPublish(cfg.PublishKey)
	srv.keys.setPlay(cfg.PlayKey)
	return srv
}

// Start binds every configured listener and returns immediately.
//
// RTMPAddr and HTTPAddr may carry several comma separated entries, so a single
// server can be exposed on the public interface and on the LAN with different
// (or identical) ports. Public traffic and LAN traffic reach the same streams:
// these are just extra front doors, not separate servers.
func (s *Server) Start() error {
	if err := s.startRTMP(); err != nil {
		return err
	}
	if s.cfg.HTTPAddr != "" {
		if err := s.startHTTP(); err != nil {
			s.closeAll()
			return err
		}
	}

	go s.sampleLoop()
	go s.janitorLoop()
	return nil
}

func (s *Server) startRTMP() error {
	book := s.addressBook(specWantsPublic(s.cfg.RTMPAddr))
	targets, err := ParseListenSpec(s.cfg.RTMPAddr, book, defaultRTMPPort)
	if err != nil {
		return fmt.Errorf("rtmp listen: %w", err)
	}
	if len(targets) == 0 {
		return fmt.Errorf("rtmp listen: no address configured")
	}

	for _, t := range targets {
		bl, err := s.bindTarget(t)
		if err != nil {
			s.closeAll()
			return fmt.Errorf("rtmp listen %s: %w", t.Addr, err)
		}
		s.lns = append(s.lns, bl)
		s.logf("RTMP listening on %s (tcp, %s)", bl.addr(), bl.target.describe())
	}

	for _, bl := range s.lns {
		go s.acceptLoop(bl)
	}
	return nil
}

// bindTarget opens one listener. A symbolic target (public:/lan:) degrades to a
// wildcard bind when the resolved address turns out not to be usable on this
// host - a NATed public address is the normal case, and it must not stop the
// server from starting. An explicit address is never rewritten: a typo there is
// an error worth reporting, not something to paper over.
func (s *Server) bindTarget(t ListenTarget) (*boundListener, error) {
	ln, err := net.Listen("tcp", t.Addr)
	if err == nil {
		if t.Note != "" {
			s.log.Warn("listener degraded to all interfaces", "want", t.Kind, "addr", t.Addr, "reason", t.Note)
		}
		return &boundListener{target: t, ln: ln}, nil
	}
	if t.Kind == ListenExplicit {
		return nil, err
	}

	fallback := ListenTarget{
		Kind:     t.Kind, // keep the intent so logs say "public" was requested
		Addr:     net.JoinHostPort("", portOfAddr(t.Addr)),
		Wildcard: true,
		Note:     err.Error(),
	}
	ln2, err2 := net.Listen("tcp", fallback.Addr)
	if err2 != nil {
		return nil, err
	}
	s.log.Warn("listener degraded to all interfaces", "want", t.Kind, "addr", t.Addr, "reason", err.Error())
	return &boundListener{target: fallback, ln: ln2}, nil
}

// Listeners describes the RTMP listeners that were actually bound.
func (s *Server) Listeners() []ListenTarget {
	out := make([]ListenTarget, 0, len(s.lns))
	for _, bl := range s.lns {
		t := bl.target
		t.Addr = bl.addr()
		out = append(out, t)
	}
	return out
}

// HTTPListeners describes the HTTP listeners that were actually bound.
func (s *Server) HTTPListeners() []ListenTarget {
	out := make([]ListenTarget, 0, len(s.httpLns))
	for _, bl := range s.httpLns {
		t := bl.target
		t.Addr = bl.addr()
		out = append(out, t)
	}
	return out
}

// Addr returns the primary RTMP address (useful when using :0 in tests).
func (s *Server) Addr() string {
	if len(s.lns) == 0 {
		return s.cfg.RTMPAddr
	}
	return s.lns[0].addr()
}

// HTTPAddr returns the primary HTTP address.
func (s *Server) HTTPAddr() string {
	if len(s.httpLns) == 0 {
		return ""
	}
	return s.httpLns[0].addr()
}

// addressBook resolves the host addresses, looking up the public IP only when
// the listen spec actually asks for it (or when the caller wants it for the
// banner). The lookup is cached, so it costs one network round trip at most.
func (s *Server) addressBook(wantPublic bool) *AddressBook {
	lanIPs := LocalIPv4s()
	book := &AddressBook{LANIPs: lanIPs, LANIP: s.cfg.LANIP}
	if book.LANIP == "" && len(lanIPs) > 0 {
		book.LANIP = lanIPs[0]
	}

	if wantPublic || s.cfg.PublicIP != "" {
		book.PublicIP, book.PublicErr, book.PublicSource = s.publicIP()
		if book.PublicIP != "" {
			book.PublicOnHost = book.HasIP(book.PublicIP)
		}
	}
	return book
}

// Addresses reports where this server is reachable. Local interfaces are
// described immediately; the public address costs one lookup and is cached
// unless Config.PublicIP turns detection off.
func (s *Server) Addresses() *AddressBook {
	return s.addressBook(true)
}

func (s *Server) publicIP() (ip string, err error, source string) {
	s.pubOnce.Do(func() {
		switch {
		case publicIPDisabled(s.cfg.PublicIP):
			s.pubIP, s.pubErr, s.pubSource = "", nil, ""
		case !autoPublicIP(s.cfg.PublicIP):
			parsed := net.ParseIP(strings.TrimSpace(s.cfg.PublicIP))
			if parsed == nil || parsed.To4() == nil {
				s.pubIP, s.pubSource = "", "flag"
				s.pubErr = fmt.Errorf("invalid public IP %q", s.cfg.PublicIP)
				return
			}
			s.pubIP, s.pubSource = parsed.To4().String(), "flag"
		default:
			ctx, cancel := context.WithTimeout(context.Background(), detectPublicTimeout)
			defer cancel()
			detected, derr := DetectPublicIPv4(ctx)
			if derr != nil {
				s.pubErr = derr
				return
			}
			s.pubIP, s.pubSource = detected, "detected"
		}
	})
	return s.pubIP, s.pubErr, s.pubSource
}

func (s *Server) acceptLoop(bl *boundListener) {
	var delay time.Duration
	for {
		nc, err := bl.ln.Accept()
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
			s.logf("accept error on %s: %v", bl.addr(), err)
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

// closeAll closes every listener that has been opened so far.
func (s *Server) closeAll() {
	for _, bl := range s.lns {
		_ = bl.ln.Close()
	}
	for _, bl := range s.httpLns {
		_ = bl.ln.Close()
	}
}

// Shutdown stops accepting, closes live connections and waits for cleanup.
func (s *Server) Shutdown(ctx context.Context) error {
	var err error
	s.stopOnce.Do(func() {
		s.closed.Store(true)
		// The first listener's Close error is the one reported to the caller;
		// the rest are best effort, we are going down either way.
		for i, bl := range s.lns {
			if cerr := bl.ln.Close(); cerr != nil && i == 0 {
				err = cerr
			}
		}
		for _, hs := range s.httpSrvs {
			_ = hs.Shutdown(ctx)
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
	want := s.keys.publish()
	if want == "" {
		return true
	}
	got := queryValue(query, "key")
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func (s *Server) authorizePlay(query string) bool {
	want := s.keys.play()
	if want == "" {
		return true
	}
	got := queryValue(query, "key")
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// PublishKey returns the secret a publisher must currently supply, or "" when
// publishing is open.
func (s *Server) PublishKey() string { return s.keys.publish() }

// PlayKey returns the secret a player must currently supply, or "" when
// playback is open.
func (s *Server) PlayKey() string { return s.keys.play() }

// SetPublishKey rotates the publish secret. An empty value opens publishing to
// anyone who can reach the port.
func (s *Server) SetPublishKey(k string) error {
	if err := ValidateStreamKey(k); err != nil {
		return err
	}
	s.keys.setPublish(k)
	s.logf("publish key updated (enabled=%t)", k != "")
	return nil
}

// SetPlayKey rotates the playback secret.
func (s *Server) SetPlayKey(k string) error {
	if err := ValidateStreamKey(k); err != nil {
		return err
	}
	s.keys.setPlay(k)
	s.logf("play key updated (enabled=%t)", k != "")
	return nil
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
			s.auth.sweep()
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

func (s *Server) startHTTP() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/login", s.handleLoginPage)
	mux.HandleFunc("/api/login", s.handleLogin)
	mux.HandleFunc("/api/logout", s.handleLogout)
	mux.HandleFunc("/api/config", s.guard(s.handleConfig))
	mux.HandleFunc("/live/", s.handleFLV)
	mux.HandleFunc("/flv/", s.handleFLV)
	mux.HandleFunc("/api/stats", s.guard(s.handleStats))
	mux.HandleFunc("/api/streams", s.guard(s.handleStats))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("ok\n"))
	})
	// The dashboard draws its mark inline, so the browser's automatic favicon
	// probe only needs a quiet answer instead of a 404 in the console.
	mux.HandleFunc("/favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	book := s.addressBook(specWantsPublic(s.cfg.HTTPAddr))
	targets, err := ParseListenSpec(s.cfg.HTTPAddr, book, defaultHTTPPort)
	if err != nil {
		return fmt.Errorf("http listen: %w", err)
	}

	var listeners []*boundListener
	for _, t := range targets {
		bl, err := s.bindTarget(t)
		if err != nil {
			for _, prev := range listeners {
				_ = prev.ln.Close()
			}
			return fmt.Errorf("http listen %s: %w", t.Addr, err)
		}
		listeners = append(listeners, bl)
	}

	// One http.Server may Serve several listeners at once: the dashboard and
	// the HTTP-FLV endpoints are therefore reachable on every bound address.
	for _, bl := range listeners {
		srv := &http.Server{
			Addr:    bl.addr(),
			Handler: mux,
			// No WriteTimeout: FLV responses are long lived by design.
			ReadHeaderTimeout: 5 * time.Second,
		}
		s.httpLns = append(s.httpLns, bl)
		s.httpSrvs = append(s.httpSrvs, srv)
		s.logf("HTTP listening on %s (dashboard + http-flv, %s)", bl.addr(), bl.target.describe())

		server := srv
		go func() {
			if err := server.Serve(bl.ln); err != nil && !errors.Is(err, http.ErrServerClosed) && !s.closed.Load() {
				s.logf("http server stopped on %s: %v", bl.addr(), err)
			}
		}()
	}
	return nil
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
