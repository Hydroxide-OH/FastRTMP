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
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Hydroxide-OH/FastRTMP/internal/rtmp"
)

// version is overridden at build time via -X main.version.
var version = "1.0.0"

func main() {
	var (
		addr = flag.String("addr", ":1935",
			"RTMP listen address; comma separated list, or use public:PORT / lan:PORT to bind the public and LAN address")
		httpAddr = flag.String("http", ":8080",
			"HTTP listen address for dashboard + HTTP-FLV, same syntax as -addr, empty disables it")
		listenMode  = flag.String("listen", "all", "listen preset: all | dual | public | lan (overrides the host part of -addr/-http)")
		publicIP    = flag.String("public-ip", "", "public IP used by public: targets and the banner; empty = auto-detect, off = never")
		lanIP       = flag.String("lan-ip", "", "LAN IP used by lan: targets; empty = pick the first private address")
		chunkSize   = flag.Int("chunk-size", 4096, "outbound RTMP chunk size in bytes")
		queueSize   = flag.String("queue", "8MB", "max per-viewer send backlog; on overflow the viewer drops to the next keyframe")
		gopSize     = flag.String("gop-cache", "4MB", "per-stream GOP cache budget, this is what makes instant playback work")
		readTimeout = flag.Duration("read-timeout", 30*time.Second, "idle connection timeout (0 disables)")
		maxConns    = flag.Int("max-conns", 0, "max concurrent connections (0 = unlimited)")
		maxViewers  = flag.Int("max-viewers", 0, "max viewers per stream (0 = unlimited)")
		publishKey  = flag.String("publish-key", "", "publish secret; publish URLs must carry ?key=...")
		playKey     = flag.String("play-key", "", "playback secret; play URLs must carry ?key=...")
		adminPass   = flag.String("admin-password", "", "password for the web dashboard; empty leaves it open and disables key editing")
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
	if !isFlagPassed("listen") && os.Getenv("FASTRTMP_LISTEN") != "" {
		*listenMode = os.Getenv("FASTRTMP_LISTEN")
	}
	if *publicIP == "" {
		*publicIP = os.Getenv("FASTRTMP_PUBLIC_IP")
	}
	if *lanIP == "" {
		*lanIP = os.Getenv("FASTRTMP_LAN_IP")
	}
	if *publishKey == "" {
		*publishKey = os.Getenv("FASTRTMP_PUBLISH_KEY")
	}
	if *playKey == "" {
		*playKey = os.Getenv("FASTRTMP_PLAY_KEY")
	}
	// Environment first for the admin password: command line arguments are
	// visible to every local user through the process list.
	if *adminPass == "" {
		*adminPass = os.Getenv("FASTRTMP_ADMIN_PASSWORD")
	}

	if err := rtmp.ValidateAdminPassword(*adminPass); err != nil {
		fatal("invalid -admin-password: %v", err)
	}

	if *showVersion {
		fmt.Println("FastRTMP", version)
		return
	}

	rtmpSpec, err := applyListenMode(*listenMode, *addr, "1935")
	if err != nil {
		fatal("%v", err)
	}
	httpSpec := ""
	if *httpAddr != "" {
		if httpSpec, err = applyListenMode(*listenMode, *httpAddr, "8080"); err != nil {
			fatal("%v", err)
		}
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
		RTMPAddr:         rtmpSpec,
		HTTPAddr:         httpSpec,
		PublicIP:         *publicIP,
		LANIP:            *lanIP,
		ChunkSize:        uint32(*chunkSize),
		QueueBytes:       queueBytes,
		GOPCacheBytes:    gopBytes,
		ReadTimeout:      *readTimeout,
		HandshakeTimeout: 10 * time.Second,
		MaxConns:         *maxConns,
		MaxSubsPerStream: *maxViewers,
		PublishKey:       *publishKey,
		PlayKey:          *playKey,
		AdminPassword:    *adminPass,
		Version:          version,
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

	banner(srv, *adminPass != "")

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

// applyListenMode rewrites the host part of a listen specification according to
// the -listen preset, keeping whatever port was requested.
//
//	all     -> unchanged (every interface)
//	dual    -> public:PORT,lan:PORT
//	public  -> public:PORT
//	lan     -> lan:PORT
func applyListenMode(mode, spec, defaultPort string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", "all", "any", "*":
		return spec, nil
	case "dual", "both", "public+lan":
		port := portOfSpec(spec, defaultPort)
		return "public:" + port + ",lan:" + port, nil
	case "public", "wan", "internet":
		return "public:" + portOfSpec(spec, defaultPort), nil
	case "lan", "local", "private":
		return "lan:" + portOfSpec(spec, defaultPort), nil
	}
	return "", fmt.Errorf("invalid -listen %q (want all, dual, public or lan)", mode)
}

// portOfSpec pulls the port out of the first entry of a listen specification,
// whatever form that entry takes.
func portOfSpec(spec, defaultPort string) string {
	first := strings.TrimSpace(strings.Split(spec, ",")[0])
	for _, prefix := range []string{"public:", "lan:", "all:", "*:"} {
		first = strings.TrimPrefix(strings.ToLower(first), prefix)
	}
	if _, p, err := net.SplitHostPort(first); err == nil && p != "" {
		return p
	}
	if strings.HasPrefix(first, ":") && len(first) > 1 {
		return first[1:]
	}
	if _, err := strconv.Atoi(first); err == nil {
		return first
	}
	return defaultPort
}

func banner(srv *rtmp.Server, adminAuth bool) {
	rtmpPort := portOf(srv.Addr())
	httpPort := portOf(srv.HTTPAddr())
	book := srv.Addresses()

	fmt.Printf("\n  FastRTMP v%s  ·  low-latency RTMP server\n", version)
	fmt.Println("  ─────────────────────────────────────────────────────────")
	fmt.Printf("  Mode        %s\n", describeListeners(srv))
	fmt.Printf("  RTMP        :%s  ·  HTTP  :%s\n", rtmpPort, httpPort)
	fmt.Println()

	if book.PublicIP != "" {
		fmt.Printf("  Public      rtmp://%s:%s/live/{stream}\n", book.PublicIP, rtmpPort)
		fmt.Printf("              http://%s:%s  (dashboard + /flv/live/{stream}.flv)\n", book.PublicIP, httpPort)
	} else if book.PublicErr != nil {
		fmt.Printf("  Public      unavailable (%v)\n", book.PublicErr)
	}
	if book.LANIP != "" {
		fmt.Printf("  LAN         rtmp://%s:%s/live/{stream}\n", book.LANIP, rtmpPort)
		fmt.Printf("              http://%s:%s\n", book.LANIP, httpPort)
	}
	fmt.Printf("  Local       rtmp://127.0.0.1:%s/live/{stream}\n", rtmpPort)
	dashState := "no password, key editing disabled"
	if adminAuth {
		dashState = "password protected, keys editable"
	}
	fmt.Printf("  Dashboard   http://127.0.0.1:%s/  (%s)\n", httpPort, dashState)
	fmt.Printf("  Health      http://127.0.0.1:%s/healthz\n", httpPort)

	if extra := extraInterfaces(book); len(extra) > 0 {
		fmt.Printf("\n  Other interfaces: %s\n", strings.Join(extra, ", "))
	}

	hints := []string{}
	if rtmpPort != "-" || httpPort != "-" {
		hints = append(hints, fmt.Sprintf("Open %s/tcp and %s/tcp in the firewall / cloud security group.", rtmpPort, httpPort))
	}
	if book.PublicIP != "" && !book.PublicOnHost {
		hints = append(hints,
			fmt.Sprintf("%s is not on a local NIC (you are behind NAT): forward %s/tcp and %s/tcp on the router or cloud gateway.",
				book.PublicIP, rtmpPort, httpPort))
	}
	if !adminAuth {
		hints = append(hints, "Dashboard has no password: anyone who can reach the HTTP port can read the stats and cannot rotate keys. Add -admin-password <secret> to lock it down.")
	}
	if srv.PublishKey() != "" {
		hints = append(hints, fmt.Sprintf("Publish auth is on (key length %d): append ?key=<secret> to publish URLs.", len(srv.PublishKey())))
	}
	if srv.PlayKey() != "" {
		hints = append(hints, fmt.Sprintf("Playback auth is on (key length %d): append ?key=<secret> to play URLs.", len(srv.PlayKey())))
	}
	if len(hints) > 0 {
		fmt.Println("\n  Hints")
		for _, h := range hints {
			fmt.Printf("    · %s\n", h)
		}
	}
	fmt.Println()
}

// describeListeners summarises what is actually bound, which is more honest
// than echoing the -listen flag: a target that fell back to a wildcard bind
// says so, instead of pretending it owns a dedicated address.
func describeListeners(srv *rtmp.Server) string {
	var parts []string
	seen := make(map[rtmp.ListenKind]bool)
	for _, t := range srv.Listeners() {
		if seen[t.Kind] {
			continue
		}
		seen[t.Kind] = true
		if t.Kind == rtmp.ListenAll {
			continue
		}
		if t.Wildcard {
			parts = append(parts, string(t.Kind)+" via all interfaces")
			continue
		}
		parts = append(parts, string(t.Kind))
	}
	if len(parts) == 0 {
		return "all interfaces"
	}
	return strings.Join(parts, " + ")
}

// extraInterfaces lists the local addresses other than the banner's LAN pick.
func extraInterfaces(book *rtmp.AddressBook) []string {
	var out []string
	for _, ip := range book.LANIPs {
		if ip != book.LANIP && ip != book.PublicIP {
			out = append(out, ip)
		}
	}
	return out
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
