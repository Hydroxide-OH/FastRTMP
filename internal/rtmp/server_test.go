package rtmp

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func testServer(t *testing.T, cfg Config) *Server {
	t.Helper()
	if cfg.RTMPAddr == "" {
		cfg.RTMPAddr = "127.0.0.1:0"
	}
	if cfg.HTTPAddr == "" {
		cfg.HTTPAddr = "127.0.0.1:0"
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	s := New(cfg)
	if err := s.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = s.Shutdown(ctx)
	})
	return s
}

func rtmpURL(s *Server, key string) string {
	return fmt.Sprintf("rtmp://%s/%s", s.Addr(), key)
}

func publishClient(t *testing.T, s *Server, key string) *Client {
	t.Helper()
	c, err := Dial(rtmpURL(s, key))
	if err != nil {
		t.Fatalf("dial publisher: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if err := c.Connect(); err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := c.CreateStream(); err != nil {
		t.Fatalf("createStream: %v", err)
	}
	if err := c.Publish(c.Stream); err != nil {
		t.Fatalf("publish: %v", err)
	}
	return c
}

func playClient(t *testing.T, s *Server, key string) *Client {
	t.Helper()
	c, err := Dial(rtmpURL(s, key))
	if err != nil {
		t.Fatalf("dial player: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if err := c.Connect(); err != nil {
		t.Fatalf("player connect: %v", err)
	}
	if err := c.CreateStream(); err != nil {
		t.Fatalf("player createStream: %v", err)
	}
	if err := c.Play(c.Stream); err != nil {
		t.Fatalf("play: %v", err)
	}
	return c
}

// videoFrame builds a tag with a wall-clock prefix so latency can be measured.
func videoFrame(ts uint32, key bool, size int) []byte {
	b := make([]byte, size)
	if key {
		b[0] = 0x17
	} else {
		b[0] = 0x27
	}
	b[1], b[2], b[3], b[4] = 0x01, 0, 0, 0
	binary.BigEndian.PutUint64(b[4:12], uint64(time.Now().UnixNano()))
	return b
}

func TestAMF0RoundTrip(t *testing.T) {
	in := Object{
		{Key: "app", Val: "live"},
		{Key: "n", Val: float64(42)},
		{Key: "ok", Val: true},
		{Key: "nil", Val: nil},
	}
	buf, err := EncodeAMF0("connect", float64(1), in, "extra")
	if err != nil {
		t.Fatal(err)
	}
	vals, err := DecodeAMF0(buf)
	if err != nil {
		t.Fatal(err)
	}
	if len(vals) != 4 {
		t.Fatalf("want 4 values, got %d", len(vals))
	}
	if vals[0] != "connect" {
		t.Fatalf("bad command %v", vals[0])
	}
	obj, ok := vals[2].(map[string]any)
	if !ok {
		t.Fatalf("want object, got %T", vals[2])
	}
	if obj["app"] != "live" || obj["n"].(float64) != 42 || obj["ok"] != true {
		t.Fatalf("bad object %#v", obj)
	}
}

func TestChunkedMessageReassembly(t *testing.T) {
	// 300 KB payload forces dozens of chunks at the default 128 byte size.
	payload := make([]byte, 300*1024)
	for i := range payload {
		payload[i] = byte(i)
	}
	pr, pw := io.Pipe()
	w := NewWriter(pw)
	done := make(chan error, 1)
	go func() {
		done <- w.WriteMessage(&Message{TypeID: MsgVideo, StreamID: 1, Timestamp: 70000, Payload: payload})
	}()

	r := NewReader(bufio.NewReader(pr))
	m, err := r.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Payload) != len(payload) {
		t.Fatalf("payload length %d != %d", len(m.Payload), len(payload))
	}
	for i := 0; i < len(payload); i += 997 {
		if m.Payload[i] != payload[i] {
			t.Fatalf("payload mismatch at %d", i)
		}
	}
	if m.Timestamp != 70000 {
		t.Fatalf("timestamp %d", m.Timestamp)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	_ = pw.Close()
}

func TestPushPullEndToEnd(t *testing.T) {
	s := testServer(t, Config{})
	pub := publishClient(t, s, "live/test")

	if err := pub.SendMetadata(1280, 720, 30); err != nil {
		t.Fatal(err)
	}
	if err := pub.SendAVCConfig(); err != nil {
		t.Fatal(err)
	}
	if err := pub.SendAACConfig(); err != nil {
		t.Fatal(err)
	}
	// Prime the GOP cache with a keyframe + 4 interframes.
	if err := pub.WriteVideo(0, videoFrame(0, true, 32*1024)); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 4; i++ {
		if err := pub.WriteVideo(uint32(i*33), videoFrame(uint32(i*33), false, 8*1024)); err != nil {
			t.Fatal(err)
		}
	}

	viewer := playClient(t, s, "live/test")

	// A late joiner must get a keyframe first - that is the GOP cache at work.
	first, err := viewer.NextMedia(2 * time.Second)
	if err != nil {
		t.Fatalf("first media: %v", err)
	}
	if first.TypeID != MsgVideo {
		t.Fatalf("first media type %d, want video", first.TypeID)
	}
	if !isVideoKeyframe(first.Payload) {
		t.Fatalf("first video frame is not a keyframe (0x%02x) - GOP cache broken", first.Payload[0])
	}

	// Stream live frames and measure latency + loss.
	const frames = 60
	lat := make([]time.Duration, 0, frames)
	received := 0
	for i := 5; i < 5+frames; i++ {
		buf := videoFrame(uint32(i*33), i%30 == 0, 12*1024)
		if err := pub.WriteVideo(uint32(i*33), buf); err != nil {
			t.Fatalf("write frame %d: %v", i, err)
		}
		msg := nextVideo(t, viewer, 2*time.Second)
		wall := int64(binary.BigEndian.Uint64(msg.Payload[4:12]))
		d := time.Since(time.Unix(0, wall))
		if d < 0 || d > 5*time.Second {
			continue
		}
		lat = append(lat, d)
		received++
	}
	if received < frames-2 {
		t.Fatalf("received %d/%d frames", received, frames)
	}
	p95 := percentile(lat, 0.95)
	if p95 > 250*time.Millisecond {
		t.Fatalf("p95 relay latency too high: %v", p95)
	}
	t.Logf("relay latency: p50=%v p95=%v max=%v over %d frames", percentile(lat, 0.5), p95, percentile(lat, 1), received)

	snap := s.Stats()
	if len(snap.Streams) != 1 {
		t.Fatalf("want 1 stream, got %d", len(snap.Streams))
	}
	st := snap.Streams[0]
	if !st.Live || st.Viewers != 1 {
		t.Fatalf("bad stream state: live=%v viewers=%d", st.Live, st.Viewers)
	}
	if st.Media.VideoWidth != 1280 || st.Media.VideoHeight != 720 {
		t.Fatalf("metadata not parsed: %+v", st.Media)
	}
	if !st.CodecReady {
		t.Fatal("codec headers were not cached")
	}
}

func TestConcurrentViewers(t *testing.T) {
	s := testServer(t, Config{})
	pub := publishClient(t, s, "live/many")
	_ = pub.SendMetadata(640, 360, 30)
	_ = pub.SendAVCConfig()
	_ = pub.WriteVideo(0, videoFrame(0, true, 16*1024))

	const viewers = 40
	var wg sync.WaitGroup
	var ok int
	var mu sync.Mutex
	var errs []string
	ready := make(chan struct{})

	for i := 0; i < viewers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-ready
			viewer, err := Dial(rtmpURL(s, "live/many"))
			if err != nil {
				mu.Lock()
				errs = append(errs, "dial: "+err.Error())
				mu.Unlock()
				return
			}
			defer viewer.Close()
			if err := viewer.Connect(); err != nil {
				mu.Lock()
				errs = append(errs, "connect: "+err.Error())
				mu.Unlock()
				return
			}
			if err := viewer.CreateStream(); err != nil {
				mu.Lock()
				errs = append(errs, "create: "+err.Error())
				mu.Unlock()
				return
			}
			if err := viewer.Play(viewer.Stream); err != nil {
				mu.Lock()
				errs = append(errs, "play: "+err.Error())
				mu.Unlock()
				return
			}
			got := 0
			for got < 20 {
				msg, err := viewer.NextMedia(2 * time.Second)
				if err != nil {
					break
				}
				if msg.TypeID == MsgVideo {
					got++
				}
			}
			if got >= 20 {
				mu.Lock()
				ok++
				mu.Unlock()
			} else {
				mu.Lock()
				errs = append(errs, fmt.Sprintf("short: got %d frames", got))
				mu.Unlock()
			}
		}()
	}
	close(ready)
	time.Sleep(300 * time.Millisecond)

	go func() {
		for i := 1; i <= 140; i++ {
			_ = pub.WriteVideo(uint32(i*33), videoFrame(uint32(i*33), false, 8*1024))
			time.Sleep(10 * time.Millisecond)
		}
	}()
	wg.Wait()

	if ok < viewers-2 {
		uniq := map[string]int{}
		for _, e := range errs {
			uniq[e]++
		}
		t.Fatalf("%d/%d viewers received the stream; errors: %v", ok, viewers, uniq)
	}
	stats := s.Stats()
	t.Logf("viewers=%d goroutines=%d heap=%.1fMB", stats.TotalViewers, stats.GoRoutines, stats.HeapMB)
}

func TestHTTPFLV(t *testing.T) {
	s := testServer(t, Config{})
	pub := publishClient(t, s, "live/flv")
	_ = pub.SendMetadata(640, 360, 25)
	_ = pub.SendAVCConfig()
	_ = pub.SendAACConfig()
	_ = pub.WriteVideo(0, videoFrame(0, true, 24*1024))

	go func() {
		for i := 1; i <= 30; i++ {
			_ = pub.WriteVideo(uint32(i*40), videoFrame(uint32(i*40), i%25 == 0, 6*1024))
			time.Sleep(20 * time.Millisecond)
		}
	}()

	url := fmt.Sprintf("http://%s/live/flv.flv", s.HTTPAddr())
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("flv get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "video/x-flv" {
		t.Fatalf("content-type %q", ct)
	}

	head := make([]byte, 13)
	if _, err := io.ReadFull(resp.Body, head); err != nil {
		t.Fatalf("read flv header: %v", err)
	}
	if string(head[:3]) != "FLV" {
		t.Fatalf("bad flv signature %q", head[:3])
	}

	var sawVideo bool
	var videoSize int
	for i := 0; i < 200; i++ {
		var tag [11]byte
		if _, err := io.ReadFull(resp.Body, tag[:]); err != nil {
			t.Fatalf("read tag header: %v", err)
		}
		size := int(tag[1])<<16 | int(tag[2])<<8 | int(tag[3])
		body := make([]byte, size+4)
		if _, err := io.ReadFull(resp.Body, body); err != nil {
			t.Fatalf("read tag body: %v", err)
		}
		if tag[0] == MsgVideo && size == 24*1024 {
			sawVideo = true
			videoSize = size
			break
		}
	}
	if !sawVideo {
		t.Fatal("no video tag in flv output")
	}
	if videoSize != 24*1024 {
		t.Fatalf("first flv video tag size %d, want cached keyframe 24576", videoSize)
	}
}

func TestDuplicatePublisherRejected(t *testing.T) {
	s := testServer(t, Config{})
	_ = publishClient(t, s, "live/dup")

	c, err := Dial(rtmpURL(s, "live/dup"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Connect(); err != nil {
		t.Fatal(err)
	}
	if err := c.CreateStream(); err != nil {
		t.Fatal(err)
	}
	err = c.Publish(c.Stream)
	if err == nil {
		t.Fatal("second publisher was accepted, want rejection")
	}
	if !strings.Contains(err.Error(), "BadName") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestPublishKeyAuth(t *testing.T) {
	s := testServer(t, Config{PublishKey: "s3cret"})

	c, err := Dial(rtmpURL(s, "live/locked"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.Connect()
	_ = c.CreateStream()
	if err := c.Publish(c.Stream); err == nil {
		t.Fatal("publish without key was accepted")
	}

	publishClient(t, s, "live/locked?key=s3cret")
}

func TestDashboardServed(t *testing.T) {
	s := testServer(t, Config{Dashboard: true})
	resp, err := http.Get(fmt.Sprintf("http://%s/", s.HTTPAddr()))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "FastRTMP") || !strings.Contains(string(body), "/api/stats") {
		t.Fatal("dashboard html not embedded correctly")
	}
	resp2, err := http.Get(fmt.Sprintf("http://%s/healthz", s.HTTPAddr()))
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != 200 {
		t.Fatalf("healthz status %d", resp2.StatusCode)
	}
}

func nextVideo(t *testing.T, c *Client, timeout time.Duration) *Message {
	t.Helper()
	for {
		msg, err := c.NextMedia(timeout)
		if err != nil {
			t.Fatalf("next video: %v", err)
		}
		if msg.TypeID == MsgVideo {
			return msg
		}
	}
}

func percentile(lat []time.Duration, p float64) time.Duration {
	if len(lat) == 0 {
		return 0
	}
	cp := make([]time.Duration, len(lat))
	copy(cp, lat)
	for i := 1; i < len(cp); i++ {
		for j := i; j > 0 && cp[j] < cp[j-1]; j-- {
			cp[j], cp[j-1] = cp[j-1], cp[j]
		}
	}
	idx := int(float64(len(cp)-1) * p)
	return cp[idx]
}
