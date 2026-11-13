package rtmp

import (
	_ "embed"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
)

//go:embed web/index.html
var indexHTML []byte

//go:embed web/login.html
var loginHTML []byte

// handleIndex serves the embedded single-page dashboard. Nothing is read from
// disk at runtime, which keeps the binary self-contained for Windows/Linux.
//
// When a dashboard password is configured the page is served only to a caller
// holding a valid session; everybody else gets the login form with a 401. The
// response deliberately carries no WWW-Authenticate header, so browsers render
// our own form instead of the native basic-auth dialog.
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.Dashboard {
		http.NotFound(w, r)
		return
	}
	if r.URL.Path != "/" && r.URL.Path != "/index.html" {
		http.NotFound(w, r)
		return
	}
	if !s.auth.authenticated(r) {
		s.writeHTML(w, http.StatusUnauthorized, loginHTML)
		return
	}
	s.writeHTML(w, http.StatusOK, indexHTML)
}

// handleLoginPage serves the password form on its own URL so the "you are not
// logged in" redirect has somewhere to go.
func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.Dashboard {
		http.NotFound(w, r)
		return
	}
	if !s.auth.enabled() || s.auth.authenticated(r) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	s.writeHTML(w, http.StatusOK, loginHTML)
}

func (s *Server) writeHTML(w http.ResponseWriter, code int, body []byte) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(code)
	_, _ = w.Write(body)
}

// guard wraps an API handler with the session check.
func (s *Server) guard(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.auth.authenticated(r) {
			h(w, r)
			return
		}
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"error": "unauthorized",
			"login": "/login",
		})
	}
}

// handleLogin validates the dashboard password and hands out a session cookie.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeErr(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	if !isJSON(r) {
		writeErr(w, http.StatusUnsupportedMediaType, "send application/json")
		return
	}
	if !s.auth.enabled() {
		writeErr(w, http.StatusConflict, "no dashboard password is configured on this server")
		return
	}

	ip := clientIP(r)
	if wait, locked := s.auth.locked(ip); locked {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		writeErr(w, http.StatusTooManyRequests, "too many failed attempts, wait a moment")
		return
	}

	var req struct {
		Password string `json:"password"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	if !s.auth.check(req.Password) {
		s.auth.noteFailure(ip)
		s.log.Warn("dashboard login failed", "addr", r.RemoteAddr)
		writeErr(w, http.StatusUnauthorized, "wrong password")
		return
	}

	s.auth.noteSuccess(ip)
	s.setSessionCookie(w, r, s.auth.issue())
	s.logf("dashboard login ok addr=%s", r.RemoteAddr)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleLogout drops the session both server side and in the browser.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeErr(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	if c, err := r.Cookie(adminCookieName); err == nil {
		s.auth.revoke(c.Value)
	}
	s.setSessionCookie(w, r, "")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) setSessionCookie(w http.ResponseWriter, r *http.Request, token string) {
	c := &http.Cookie{
		Name:     adminCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   r.TLS != nil,
	}
	if token == "" {
		c.MaxAge = -1
	} else {
		c.MaxAge = int(adminSessionTTL.Seconds())
	}
	http.SetCookie(w, c)
}

// adminConfig is the payload of /api/config. It is the dashboard's view of the
// runtime settings it is allowed to change.
type adminConfig struct {
	Version      string `json:"version"`
	PublishKey   string `json:"publish_key"`
	PlayKey      string `json:"play_key"`
	SecretsShown bool   `json:"secrets_visible"`
	AdminAuth    bool   `json:"admin_auth"`
	Dashboard    bool   `json:"dashboard"`
	RTMPAddr     string `json:"rtmp_addr"`
	HTTPAddr     string `json:"http_addr"`
	PublicIP     string `json:"public_ip"`
	LANIP        string `json:"lan_ip"`
}

func (s *Server) configView() adminConfig {
	pub, play := s.keys.snapshot()
	visible := s.auth.enabled()
	if !visible {
		// A passwordless dashboard must not leak the secrets that protect the
		// ingest: anyone who can reach the port would read them.
		pub, play = "", ""
	}
	book := s.addressBook(false)
	return adminConfig{
		Version:      s.cfg.Version,
		PublishKey:   pub,
		PlayKey:      play,
		SecretsShown: visible,
		AdminAuth:    visible,
		Dashboard:    s.cfg.Dashboard,
		RTMPAddr:     s.Addr(),
		HTTPAddr:     s.HTTPAddr(),
		PublicIP:     book.PublicIP,
		LANIP:        book.LANIP,
	}
}

// handleConfig reads or rotates the stream secrets.
//
//	GET  /api/config -> current settings
//	POST /api/config {"publish_key":"...","play_key":"..."} -> rotate
//
// Omitted fields are left untouched; an explicit empty string turns that
// particular auth off.
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, s.configView())
	case http.MethodPost:
		if !s.auth.enabled() {
			writeErr(w, http.StatusForbidden,
				"set -admin-password (or FASTRTMP_ADMIN_PASSWORD) before editing stream keys from the dashboard")
			return
		}
		if !isJSON(r) {
			writeErr(w, http.StatusUnsupportedMediaType, "send application/json")
			return
		}
		var req struct {
			PublishKey *string `json:"publish_key"`
			PlayKey    *string `json:"play_key"`
		}
		if err := decodeJSON(r, &req); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		fail := func(msg string) {
			writeErr(w, http.StatusBadRequest, msg)
		}
		if req.PublishKey != nil {
			if err := s.SetPublishKey(strings.TrimSpace(*req.PublishKey)); err != nil {
				fail("publish key: " + err.Error())
				return
			}
		}
		if req.PlayKey != nil {
			if err := s.SetPlayKey(strings.TrimSpace(*req.PlayKey)); err != nil {
				fail("play key: " + err.Error())
				return
			}
		}
		writeJSON(w, http.StatusOK, s.configView())
	default:
		w.Header().Set("Allow", "GET, POST")
		writeErr(w, http.StatusMethodNotAllowed, "use GET or POST")
	}
}

// ---------- small helpers ----------

func isJSON(r *http.Request) bool {
	ct := r.Header.Get("Content-Type")
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	return strings.EqualFold(strings.TrimSpace(ct), "application/json")
}

// decodeJSON reads a small JSON body. The size cap keeps a malicious client
// from making the server allocate megabytes for a two-field form.
func decodeJSON(r *http.Request, dst any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 8<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return errBadBody
	}
	return nil
}

var errBadBody = jsonError("malformed request body")

type jsonError string

func (e jsonError) Error() string { return string(e) }

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
