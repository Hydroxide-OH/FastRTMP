package rtmp

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"
)

// dashClient is a tiny browser stand-in: it keeps the session cookie and posts
// JSON the same way the dashboard does.
type dashClient struct {
	t    *testing.T
	base string
	http *http.Client
}

func newDashClient(t *testing.T, s *Server) *dashClient {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &dashClient{
		t:    t,
		base: "http://" + s.HTTPAddr(),
		http: &http.Client{Jar: jar},
	}
}

func (d *dashClient) get(path string) (int, string) {
	d.t.Helper()
	resp, err := d.http.Get(d.base + path)
	if err != nil {
		d.t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func (d *dashClient) post(path, body string) (int, string) {
	d.t.Helper()
	resp, err := d.http.Post(d.base+path, "application/json", strings.NewReader(body))
	if err != nil {
		d.t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out)
}

func (d *dashClient) login(pw string) int {
	d.t.Helper()
	code, _ := d.post("/api/login", fmt.Sprintf(`{"password":%q}`, pw))
	return code
}

func TestDashboardRequiresPassword(t *testing.T) {
	s := testServer(t, Config{Dashboard: true, AdminPassword: "hunter2"})
	d := newDashClient(t, s)

	code, body := d.get("/")
	if code != http.StatusUnauthorized {
		t.Fatalf("GET / without session = %d, want 401", code)
	}
	if !strings.Contains(body, "管理面板") {
		t.Fatal("login page was not served")
	}
	if code, _ := d.get("/api/stats"); code != http.StatusUnauthorized {
		t.Fatalf("GET /api/stats without session = %d, want 401", code)
	}

	if code := d.login("wrong"); code != http.StatusUnauthorized {
		t.Fatalf("login with wrong password = %d, want 401", code)
	}
	if code := d.login("hunter2"); code != http.StatusOK {
		t.Fatalf("login = %d, want 200", code)
	}

	if code, body := d.get("/"); code != http.StatusOK || !strings.Contains(body, "/api/stats") {
		t.Fatalf("GET / after login = %d", code)
	}
	if code, _ := d.get("/api/stats"); code != http.StatusOK {
		t.Fatalf("GET /api/stats after login = %d, want 200", code)
	}

	code, _ = d.post("/api/logout", "")
	if code != http.StatusOK {
		t.Fatalf("logout = %d", code)
	}
	if code, _ := d.get("/api/stats"); code != http.StatusUnauthorized {
		t.Fatalf("stats after logout = %d, want 401", code)
	}
}

func TestLoginIsRateLimited(t *testing.T) {
	s := testServer(t, Config{Dashboard: true, AdminPassword: "hunter2"})
	d := newDashClient(t, s)
	for i := 0; i < adminMaxFails; i++ {
		d.login("nope")
	}
	if code := d.login("hunter2"); code != http.StatusTooManyRequests {
		t.Fatalf("login after %d failures = %d, want 429", adminMaxFails, code)
	}
}

func TestDashboardOpenWithoutPassword(t *testing.T) {
	s := testServer(t, Config{Dashboard: true})
	d := newDashClient(t, s)
	if code, _ := d.get("/"); code != http.StatusOK {
		t.Fatalf("passwordless dashboard = %d, want 200", code)
	}
	if code, _ := d.get("/login"); code != http.StatusOK {
		t.Fatalf("/login without password should redirect, got %d", code)
	}
	// Secrets are not readable, and editing is refused.
	code, body := d.get("/api/config")
	if code != http.StatusOK {
		t.Fatalf("GET /api/config = %d", code)
	}
	var cfg adminConfig
	if err := json.Unmarshal([]byte(body), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.PublishKey != "" || cfg.SecretsShown {
		t.Fatalf("passwordless dashboard leaked secrets: %+v", cfg)
	}
	if code, _ := d.post("/api/config", `{"publish_key":"x"}`); code != http.StatusForbidden {
		t.Fatalf("POST /api/config without admin password = %d, want 403", code)
	}
}

func TestConfigRotatesPublishKey(t *testing.T) {
	s := testServer(t, Config{Dashboard: true, AdminPassword: "hunter2", PublishKey: "old"})
	d := newDashClient(t, s)

	// The old key works, an unsigned publish does not.
	if _, err := dialPublish(s, "live/cam"); err == nil {
		t.Fatal("publish without key was accepted")
	}

	if code := d.login("hunter2"); code != http.StatusOK {
		t.Fatalf("login = %d", code)
	}
	code, body := d.post("/api/config", `{"publish_key":"brand-new"}`)
	if code != http.StatusOK {
		t.Fatalf("POST /api/config = %d: %s", code, body)
	}
	if s.PublishKey() != "brand-new" {
		t.Fatalf("publish key = %q, want brand-new", s.PublishKey())
	}

	// The rotation is live: the old secret stops working, the new one works.
	if _, err := dialPublish(s, "live/cam?key=old"); err == nil {
		t.Fatal("old publish key still accepted after rotation")
	}
	c, err := dialPublish(s, "live/cam?key=brand-new")
	if err != nil {
		t.Fatalf("new publish key rejected: %v", err)
	}
	_ = c.Close()
}

func TestConfigRejectsBadKeys(t *testing.T) {
	s := testServer(t, Config{Dashboard: true, AdminPassword: "hunter2"})
	d := newDashClient(t, s)
	if code := d.login("hunter2"); code != http.StatusOK {
		t.Fatalf("login = %d", code)
	}

	for _, body := range []string{
		`{"publish_key":"has space"}`,
		`{"publish_key":"a?b"}`,
		`{"publish_key":"` + strings.Repeat("x", 129) + `"}`,
	} {
		if code, msg := d.post("/api/config", body); code != http.StatusBadRequest {
			t.Fatalf("POST %s = %d, want 400 (%s)", body, code, msg)
		}
	}

	// An explicit empty value turns publishing auth off.
	if code, _ := d.post("/api/config", `{"publish_key":""}`); code != http.StatusOK {
		t.Fatalf("clearing the publish key = %d, want 200", code)
	}
	if s.PublishKey() != "" {
		t.Fatalf("publish key = %q, want empty", s.PublishKey())
	}
}

func TestPlayKeyGuardsHTTPFLV(t *testing.T) {
	s := testServer(t, Config{Dashboard: true, PlayKey: "watch"})
	if code, _ := newDashClient(t, s).get("/flv/live/cam.flv"); code != http.StatusForbidden {
		t.Fatalf("keyless http-flv = %d, want 403", code)
	}

	req, _ := http.NewRequest(http.MethodGet,
		fmt.Sprintf("http://%s/flv/live/cam.flv?key=watch", s.HTTPAddr()), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("http-flv with key = %d, want 200", resp.StatusCode)
	}
}

func TestValidateStreamKey(t *testing.T) {
	valid := []string{"", "abc123", "a.b_c-d~e", "中文密钥"}
	for _, k := range valid {
		if err := ValidateStreamKey(k); err != nil {
			t.Errorf("ValidateStreamKey(%q) = %v, want nil", k, err)
		}
	}
	invalid := []string{"a b", "a?b", "a&b", "a/b", "a#b", "a=b", "a\tb", strings.Repeat("x", 129)}
	for _, k := range invalid {
		if err := ValidateStreamKey(k); err == nil {
			t.Errorf("ValidateStreamKey(%q) = nil, want error", k)
		}
	}
}

func TestAdminPasswordSessionInvalidatedBySetPassword(t *testing.T) {
	a := newAdminAuth("one")
	tok := a.issue()
	if !a.valid(tok) {
		t.Fatal("fresh token should be valid")
	}
	a.setPassword("two")
	if a.valid(tok) {
		t.Fatal("token survived a password change")
	}
	if a.check("one") {
		t.Fatal("old password still accepted")
	}
	if !a.check("two") {
		t.Fatal("new password rejected")
	}
}

// dialPublish opens a publishing RTMP client, returning the connect error so
// the caller can assert on rejection.
func dialPublish(s *Server, key string) (*Client, error) {
	u, err := url.Parse(rtmpURL(s, key))
	if err != nil {
		return nil, err
	}
	c, err := Dial(u.String())
	if err != nil {
		return nil, err
	}
	if err := c.Connect(); err != nil {
		_ = c.Close()
		return nil, err
	}
	if err := c.CreateStream(); err != nil {
		_ = c.Close()
		return nil, err
	}
	if err := c.Publish(c.Stream); err != nil {
		_ = c.Close()
		return nil, err
	}
	return c, nil
}
