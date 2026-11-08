package rtmp

import (
	"encoding/binary"
	"errors"
	"io"
	"net/http"
	"runtime"
	"strconv"
	"strings"
)

// writeFLVTag emits one RTMP message as an FLV tag. The three writes are
// coalesced by the caller through bufio-less batching; HTTP-FLV clients get one
// flush per media batch, which is what keeps glass-to-glass latency low.
func writeFLVTag(w io.Writer, m *Message) error {
	var h [11]byte
	n := len(m.Payload)
	typeID := m.TypeID
	if typeID == MsgDataAMF3 {
		typeID = MsgDataAMF0
	}
	h[0] = typeID
	h[1], h[2], h[3] = byte(n>>16), byte(n>>8), byte(n)
	ts := m.Timestamp
	h[4], h[5], h[6] = byte(ts>>16), byte(ts>>8), byte(ts)
	h[7] = byte(ts >> 24)
	// h[8..10] = stream id 0

	if _, err := w.Write(h[:]); err != nil {
		return err
	}
	if n > 0 {
		if _, err := w.Write(m.Payload); err != nil {
			return err
		}
	}
	var prev [4]byte
	binary.BigEndian.PutUint32(prev[:], uint32(11+n))
	_, err := w.Write(prev[:])
	return err
}

// flvHeader is the 9 byte file header followed by the first PrevTagSize0.
var flvHeader = []byte{'F', 'L', 'V', 0x01, 0x05, 0, 0, 0, 9, 0, 0, 0, 0}

// handleFLV serves HTTP-FLV, the lowest-latency way to reach a browser
// (mpegts.js / flv.js) or ffplay without WebRTC signalling.
func (s *Server) handleFLV(w http.ResponseWriter, r *http.Request) {
	var key string
	switch {
	case strings.HasPrefix(r.URL.Path, "/flv/"):
		key = strings.TrimPrefix(r.URL.Path, "/flv/")
	case strings.HasPrefix(r.URL.Path, "/live/"):
		key = "live/" + strings.TrimPrefix(r.URL.Path, "/live/")
	default:
		http.NotFound(w, r)
		return
	}
	key = strings.TrimSuffix(strings.Trim(key, "/"), ".flv")
	if key == "" || strings.Contains(key, "..") {
		http.Error(w, "bad stream key", http.StatusBadRequest)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	st := s.viewStream(key)
	if max := s.cfg.MaxSubsPerStream; max > 0 && st.viewerCount() >= max {
		http.Error(w, "viewer limit reached", http.StatusServiceUnavailable)
		return
	}

	h := w.Header()
	h.Set("Content-Type", "video/x-flv")
	h.Set("Cache-Control", "no-cache, no-store")
	h.Set("Pragma", "no-cache")
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("X-Accel-Buffering", "no") // disable nginx buffering when proxied
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(flvHeader); err != nil {
		return
	}
	flusher.Flush()

	client := newFLVClient(st, w, flusher.Flush, r.RemoteAddr, s.cfg.QueueBytes)
	st.subscribe(client)
	defer func() {
		client.shutdown()
		st.unsubscribe(client)
		s.releaseStream(st)
	}()

	// HTTP has no in-band close signal for a streaming response: when the
	// browser tab closes and no media is flowing we would sit on the queue
	// forever. The request context is cancelled by net/http as soon as the peer
	// goes away, so use it to tear the viewer down (and free the TCP socket).
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-r.Context().Done():
			client.shutdown()
		case <-done:
		}
	}()

	s.debugf("http-flv open stream=%q addr=%s", key, r.RemoteAddr)
	if err := client.run(); err != nil && !errors.Is(err, io.ErrClosedPipe) && !errors.Is(err, http.ErrHandlerTimeout) {
		s.debugf("http-flv closed stream=%q addr=%s: %v", key, r.RemoteAddr, err)
	}
}

func goRoutines() int { return runtime.NumGoroutine() }

func heapMB() float64 {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return float64(ms.HeapAlloc) / (1 << 20)
}

// ParseSize accepts "4MB", "512KB" or a plain byte count.
func ParseSize(s string) (int, error) {
	s = strings.TrimSpace(strings.ToUpper(s))
	if s == "" {
		return 0, errors.New("empty size")
	}
	mult := 1
	switch {
	case strings.HasSuffix(s, "GB"):
		mult, s = 1<<30, strings.TrimSuffix(s, "GB")
	case strings.HasSuffix(s, "MB"):
		mult, s = 1<<20, strings.TrimSuffix(s, "MB")
	case strings.HasSuffix(s, "KB"):
		mult, s = 1<<10, strings.TrimSuffix(s, "KB")
	case strings.HasSuffix(s, "B"):
		mult, s = 1, strings.TrimSuffix(s, "B")
	}
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0, err
	}
	if n < 0 {
		return 0, errors.New("negative size")
	}
	return n * mult, nil
}
