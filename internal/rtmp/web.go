package rtmp

import (
	_ "embed"
	"net/http"
)

//go:embed web/index.html
var indexHTML []byte

// handleIndex serves the embedded single-page dashboard. Nothing is read from
// disk at runtime, which keeps the binary self-contained for Windows/Linux.
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.Dashboard {
		http.NotFound(w, r)
		return
	}
	if r.URL.Path != "/" && r.URL.Path != "/index.html" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(indexHTML)
}
