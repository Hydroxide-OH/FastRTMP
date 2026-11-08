// Command fastrtmp is a low-latency, high-concurrency RTMP ingest/relay server.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Hydroxide-OH/FastRTMP/internal/rtmp"
)

// version is overridden at build time via -X main.version.
var version = "1.0.0"

func main() {
	var (
		addr        = flag.String("addr", ":1935", "RTMP listen address, e.g. :1935")
		httpAddr    = flag.String("http", ":8080", "HTTP listen address for dashboard + HTTP-FLV, empty disables it")
		chunkSize   = flag.Int("chunk-size", 4096, "outbound RTMP chunk size in bytes")
		queueSize   = flag.String("queue", "8MB", "max per-viewer send backlog; on overflow the viewer drops to the next keyframe")
		gopSize     = flag.String("gop-cache", "4MB", "per-stream GOP cache budget, this is what makes instant playback work")
		readTimeout = flag.Duration("read-timeout", 30*time.Second, "idle connection timeout (0 disables)")
		maxConns    = flag.Int("max-conns", 0, "max concurrent connections (0 = unlimited)")
		maxViewers  = flag.Int("max-viewers", 0, "max viewers per stream (0 = unlimited)")
		publishKey  = flag.String("publish-key", "", "publish secret; publish URLs must carry ?key=...")
		playKey     = flag.String("play-key", "", "playback secret; play URLs must carry ?key=...")
		logLevel    = flag.String("log-level", "info", "log level: debug|info|warn|error")
		pprofAddr   = flag.String("pprof", "", "pprof listen address, e.g. 127.0.0.1:6060")
		showVersion = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()

	// Environment fallbacks make container/systemd deployment declarative
	// without rebuilding the command line.
	if !isFlagPassed("addr") && os.Getenv("FASTRTMP_ADDR") != "" {
		*addr = os.Getenv("FASTRTMP_ADDR")
	}
	if !isFlagPassed("http") && os.Getenv("FASTRTMP_HTTP") != "" {
		*httpAddr = os.Getenv("FASTRTMP_HTTP")
	}
	if *publishKey == "" {
		*publishKey = os.Getenv("FASTRTMP_PUBLISH_KEY")
	}
	if *playKey == "" {
		*playKey = os.Getenv("FASTRTMP_PLAY_KEY")
	}

	if *showVersion {
		fmt.Println("FastRTMP", version)
		return
	}

	queueBytes, err := rtmp.ParseSize(*queueSize)
	if err != nil {
		fatal("invalid -queue: %v", err)
	}
	gopBytes, err := rtmp.ParseSize(*gopSize)
	if err != nil {
		fatal("invalid -gop-cache: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: parseLevel(*logLevel)}))
	cfg := rtmp.Config{
		RTMPAddr:         *addr,
		HTTPAddr:         *httpAddr,
		ChunkSize:        uint32(*chunkSize),
		QueueBytes:       queueBytes,
		GOPCacheBytes:    gopBytes,
		ReadTimeout:      *readTimeout,
		HandshakeTimeout: 10 * time.Second,
		MaxConns:         *maxConns,
		MaxSubsPerStream: *maxViewers,
		PublishKey:       *publishKey,
		PlayKey:          *playKey,
		Dashboard:        true,
		Logger:           logger,
	}
	srv := rtmp.New(cfg)
	if err := srv.Start(); err != nil {
		fatal("%v", err)
	}

	if *pprofAddr != "" {
		go func() {
			logger.Info("pprof listening", "addr", *pprofAddr)
			if err := http.ListenAndServe(*pprofAddr, nil); err != nil {
				logger.Warn("pprof stopped", "err", err)
			}
		}()
	}

	banner(srv, *publishKey != "", *playKey != "")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()
	logger.Info("shutting down...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Warn("shutdown incomplete", "err", err)
	}
	logger.Info("bye")
}

func isFlagPassed(name string) bool {
	passed := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == name {
			passed = true
		}
	})
	return passed
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "fastrtmp: "+format+"\n", args...)
	os.Exit(1)
}

func banner(srv *rtmp.Server, hasPubKey, hasPlayKey bool) {
	rtmpPort := portOf(srv.Addr())
	httpPort := portOf(srv.HTTPAddr())
	ips := localIPs()
	if len(ips) == 0 {
		ips = []string{"127.0.0.1"}
	}
	ip := ips[0]

	fmt.Printf("\n  FastRTMP v%s  ·  low-latency RTMP server\n", version)
	fmt.Println("  ─────────────────────────────────────────────────────────")
	fmt.Printf("  Publish     rtmp://%s:%s/live/{stream}\n", ip, rtmpPort)
	fmt.Printf("  Play        rtmp://%s:%s/live/{stream}\n", ip, rtmpPort)
	fmt.Printf("  HTTP-FLV    http://%s:%s/flv/live/{stream}.flv\n", ip, httpPort)
	fmt.Printf("  Dashboard   http://%s:%s\n", ip, httpPort)
	fmt.Printf("  Health      http://%s:%s/healthz\n", ip, httpPort)

	if len(ips) > 1 {
		fmt.Printf("\n  Other interfaces: %s\n", strings.Join(ips[1:], ", "))
	}
	if hasPubKey {
		fmt.Println("  Publish auth: enabled (append ?key=<secret> to the URL)")
	}
	if hasPlayKey {
		fmt.Println("  Playback auth: enabled (append ?key=<secret> to the URL)")
	}
	fmt.Println()
}

func portOf(addr string) string {
	if addr == "" {
		return "-"
	}
	if _, p, err := net.SplitHostPort(addr); err == nil {
		return p
	}
	return addr
}

// localIPs returns non-loopback IPv4 addresses, most likely LAN first.
func localIPs() []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []string
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipnet.IP.To4()
			if ip4 == nil || ip4.IsLoopback() {
				continue
			}
			out = append(out, ip4.String())
		}
	}
	return out
}
