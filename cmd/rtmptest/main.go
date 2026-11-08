// Command rtmptest is a built-in publisher/viewer used to verify and load-test
// the server without OBS or ffmpeg.
//
//	push  : publish synthetic frames (8-byte wall clock prefix for latency probing)
//	pull  : play a stream and report end-to-end latency percentiles
//	load  : spawn N concurrent viewers of the same stream
//	both  : push in the background and measure with N viewers
package main

import (
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"math/rand"
	"net"
	"os"
	"os/signal"
	"sort"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Hydroxide-OH/FastRTMP/internal/rtmp"
)

func main() {
	var (
		mode     = flag.String("mode", "push", "运行模式: push|pull|load|both")
		rawURL   = flag.String("url", "rtmp://127.0.0.1:1935/live/test", "RTMP 地址（含 app/stream）")
		duration = flag.Duration("duration", 15*time.Second, "运行时长")
		fps      = flag.Int("fps", 30, "推流帧率")
		keyEvery = flag.Int("keyframe", 60, "每多少帧一个关键帧")
		frameSz  = flag.Int("frame-size", 16000, "每帧平均字节数")
		viewers  = flag.Int("viewers", 10, "load/both 模式的并发观众数")
		verbose  = flag.Bool("v", false, "打印每次错误")
		quiet    = flag.Bool("quiet", false, "只输出汇总，不打印每个观众的周期统计")
	)
	flag.Parse()

	if *fps <= 0 {
		*fps = 30
	}
	if *keyEvery <= 0 {
		*keyEvery = 60
	}

	stop := make(chan struct{})
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		<-sig
		close(stop)
	}()

	var wg sync.WaitGroup
	switch *mode {
	case "push":
		wg.Add(1)
		go func() { defer wg.Done(); runPush(*rawURL, *duration, *fps, *keyEvery, *frameSz, stop) }()
	case "pull":
		wg.Add(1)
		go func() { defer wg.Done(); runPull(*rawURL, *duration, *verbose, *quiet) }()
	case "load":
		runViewers(*rawURL, *viewers, *duration, *verbose, *quiet)
	case "both":
		wg.Add(1)
		go func() { defer wg.Done(); runPush(*rawURL, *duration+2*time.Second, *fps, *keyEvery, *frameSz, stop) }()
		time.Sleep(1500 * time.Millisecond) // let the publisher establish + GOP cache warm
		runViewers(*rawURL, *viewers, *duration, *verbose, *quiet)
	default:
		fmt.Fprintf(os.Stderr, "unknown mode %q\n", *mode)
		os.Exit(2)
	}
	wg.Wait()
}

func runPush(rawURL string, dur time.Duration, fps, keyEvery, frameSz int, stop <-chan struct{}) {
	c, err := rtmp.Dial(rawURL)
	if err != nil {
		fmt.Println("dial:", err)
		return
	}
	defer c.Close()
	if err := c.Connect(); err != nil {
		fmt.Println("connect:", err)
		return
	}
	if err := c.CreateStream(); err != nil {
		fmt.Println("createStream:", err)
		return
	}
	name := c.Stream
	if err := c.Publish(name); err != nil {
		fmt.Println("publish:", err)
		return
	}
	_ = c.SendMetadata(1280, 720, float64(fps))
	_ = c.SendAVCConfig()
	_ = c.SendAACConfig()

	fmt.Printf("publish %s  fps=%d key=%d frame=%dB\n", rawURL, fps, keyEvery, frameSz)

	frameDur := time.Second / time.Duration(fps)
	ticker := time.NewTicker(frameDur)
	defer ticker.Stop()
	deadline := time.Now().Add(dur)
	start := time.Now()
	lastReport := start
	var frames, bytes int64
	var idx int64
	payload := make([]byte, frameSz+1024)

	for {
		select {
		case <-stop:
			fmt.Println("publish stopped by user")
			return
		case <-ticker.C:
		}
		if time.Now().After(deadline) {
			break
		}
		idx++
		ts := uint32(float64(idx) * 1000 / float64(fps))

		key := idx%int64(keyEvery) == 0
		n := frameSz + rand.Intn(512)
		if key {
			n = frameSz * 3
		}
		if cap(payload) < n+16 {
			payload = make([]byte, n+16)
		}
		buf := payload[:n]
		rand.Read(buf[12:])
		if key {
			buf[0] = 0x17 // keyframe, AVC
		} else {
			buf[0] = 0x27 // inter frame, AVC
		}
		buf[1], buf[2], buf[3], buf[4] = 0x01, 0, 0, 0
		// Wall-clock prefix: lets the viewer compute true one-way latency.
		// Timestamp goes at [4:12] so byte 0 keeps the FLV frame/codec flags.
		binary.BigEndian.PutUint64(buf[4:12], uint64(time.Now().UnixNano()))

		if err := c.WriteVideo(ts, buf); err != nil {
			fmt.Println("write video:", err)
			return
		}
		if idx%3 == 0 {
			_ = c.WriteAudio(ts, []byte{0xAF, 0x01, 0x11, 0x22, 0x33, 0x44})
		}
		frames++
		bytes += int64(n)

		if time.Since(lastReport) >= time.Second {
			el := time.Since(lastReport).Seconds()
			fmt.Printf("  push %.1fs  %.0f fps  %.1f Mbps  sent=%d\n",
				time.Since(start).Seconds(), float64(frames)/el, float64(bytes*8)/el/1e6, frames)
			frames, bytes = 0, 0
			lastReport = time.Now()
		}
	}
	fmt.Printf("publish done: %d frames\n", idx)
}

type latStats struct {
	mu     sync.Mutex
	lat    []time.Duration
	frames int64
	bytes  int64
	errs   int64
	lastTS uint32
	gaps   int64
}

func (s *latStats) add(d time.Duration, n int) {
	s.mu.Lock()
	s.lat = append(s.lat, d)
	s.frames++
	s.bytes += int64(n)
	s.mu.Unlock()
}

func (s *latStats) percentile(p float64) time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.lat) == 0 {
		return 0
	}
	cp := make([]time.Duration, len(s.lat))
	copy(cp, s.lat)
	sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
	idx := int(float64(len(cp)-1) * p)
	return cp[idx]
}

func (s *latStats) count() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.frames
}

func runPull(rawURL string, dur time.Duration, verbose, quiet bool) *latStats {
	st := &latStats{}
	c, err := rtmp.Dial(rawURL)
	if err != nil {
		fmt.Println("dial:", err)
		return st
	}
	defer c.Close()
	if err := c.Connect(); err != nil {
		fmt.Println("connect:", err)
		return st
	}
	if err := c.CreateStream(); err != nil {
		fmt.Println("createStream:", err)
		return st
	}
	if err := c.Play(c.Stream); err != nil {
		fmt.Println("play:", err)
		return st
	}

	deadline := time.Now().Add(dur)
	lastReport := time.Now()
	// A new viewer starts from the GOP cache, so the first frames are up to one
	// GOP old by design. Skip that warm-up window to measure steady-state relay
	// latency instead of the instant-start buffer.
	statsFrom := time.Now().Add(2 * time.Second)
	var windowFrames int64
	for time.Now().Before(deadline) {
		msg, err := c.NextMedia(2 * time.Second)
		if err != nil {
			// A live stream can be idle (viewer waiting for a publisher, or a
			// 1 fps camera). Keep waiting until the requested duration is over.
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			if verbose {
				fmt.Println("read:", err)
			}
			break
		}
		if msg.TypeID != rtmp.MsgVideo {
			continue
		}
		if len(msg.Payload) >= 16 {
			wall := int64(binary.BigEndian.Uint64(msg.Payload[4:12]))
			if wall > 0 && time.Now().After(statsFrom) {
				lat := time.Since(time.Unix(0, wall))
				if lat > 0 && lat < time.Minute {
					st.add(lat, len(msg.Payload))
					windowFrames++
				}
			}
		}
		if !quiet && time.Since(lastReport) >= time.Second && st.count() > 0 {
			fmt.Printf("  pull %.1fs  %.0f fps  %.1f Mbps  p50=%s p95=%s\n",
				dur.Seconds()-time.Until(deadline).Seconds(),
				float64(windowFrames)/time.Since(lastReport).Seconds(),
				float64(st.bytes*8)/time.Since(lastReport).Seconds()/1e6,
				st.percentile(0.5), st.percentile(0.95))
			windowFrames = 0
			st.bytes = 0
			lastReport = time.Now()
		}
	}
	return st
}

func runViewers(rawURL string, n int, dur time.Duration, verbose, quiet bool) {
	fmt.Printf("starting %d viewers against %s for %s\n", n, rawURL, dur)
	stats := make([]*latStats, n)
	var wg sync.WaitGroup
	var joined, failed atomic.Int64
	start := time.Now()

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			st := runPull(rawURL, dur, verbose, quiet)
			stats[i] = st
			if st.count() > 0 {
				joined.Add(1)
			} else {
				failed.Add(1)
			}
		}(i)
		// Spread connection attempts slightly so we do not test SYN backlog only.
		if n >= 50 && i%25 == 24 {
			time.Sleep(20 * time.Millisecond)
		}
	}
	wg.Wait()

	var total int64
	all := &latStats{}
	for _, st := range stats {
		if st == nil {
			continue
		}
		total += st.count()
		st.mu.Lock()
		all.lat = append(all.lat, st.lat...)
		st.mu.Unlock()
	}
	elapsed := time.Since(start).Seconds()
	fmt.Println("──────── result ────────")
	fmt.Printf("viewers      : %d joined, %d failed\n", joined.Load(), failed.Load())
	fmt.Printf("frames       : %d total (%.0f/s aggregate over %.1fs)\n", total, float64(total)/elapsed, elapsed)
	fmt.Printf("latency p50  : %s\n", all.percentile(0.5))
	fmt.Printf("latency p95  : %s\n", all.percentile(0.95))
	fmt.Printf("latency p99  : %s\n", all.percentile(0.99))
	fmt.Printf("latency max  : %s\n", all.percentile(1.0))
}
