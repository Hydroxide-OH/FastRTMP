package rtmp

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

var errNoNetwork = errors.New("no route to host")

func testBook() *AddressBook {
	return &AddressBook{
		PublicIP:     "203.0.113.7",
		PublicOnHost: true,
		LANIPs:       []string{"192.168.1.10"},
		LANIP:        "192.168.1.10",
	}
}

func TestParseListenSpec(t *testing.T) {
	tests := []struct {
		name    string
		spec    string
		book    *AddressBook
		want    []string // addr for each target, in order
		kinds   []ListenKind
		wantErr bool
	}{
		{
			name: "empty",
			spec: "",
			book: testBook(),
			want: nil,
		},
		{
			name:  "wildcard colon port",
			spec:  ":1935",
			book:  testBook(),
			want:  []string{":1935"},
			kinds: []ListenKind{ListenAll},
		},
		{
			name:  "bare port means every interface",
			spec:  "1935",
			book:  testBook(),
			want:  []string{":1935"},
			kinds: []ListenKind{ListenAll},
		},
		{
			name:  "explicit wildcard address",
			spec:  "0.0.0.0:1935",
			book:  testBook(),
			want:  []string{":1935"},
			kinds: []ListenKind{ListenAll},
		},
		{
			name:  "explicit address is kept verbatim",
			spec:  "127.0.0.1:1935",
			book:  testBook(),
			want:  []string{"127.0.0.1:1935"},
			kinds: []ListenKind{ListenExplicit},
		},
		{
			name:  "dual listen",
			spec:  "public:1935,lan:1935",
			book:  testBook(),
			want:  []string{"203.0.113.7:1935", "192.168.1.10:1935"},
			kinds: []ListenKind{ListenPublic, ListenLAN},
		},
		{
			name:  "symbolic without port uses the default",
			spec:  "public,lan",
			book:  testBook(),
			want:  []string{"203.0.113.7:1935", "192.168.1.10:1935"},
			kinds: []ListenKind{ListenPublic, ListenLAN},
		},
		{
			name:  "different ports per target",
			spec:  "public:1935,lan:19350",
			book:  testBook(),
			want:  []string{"203.0.113.7:1935", "192.168.1.10:19350"},
			kinds: []ListenKind{ListenPublic, ListenLAN},
		},
		{
			name: "NATed public address falls back to wildcard and drops the LAN duplicate",
			spec: "public:1935,lan:1935",
			book: &AddressBook{
				PublicIP:     "203.0.113.7",
				PublicOnHost: false, // lives on the router, not on this host
				LANIPs:       []string{"192.168.1.10"},
				LANIP:        "192.168.1.10",
			},
			want:  []string{":1935"},
			kinds: []ListenKind{ListenPublic},
		},
		{
			name:  "unknown public ip falls back to wildcard",
			spec:  "public:1935",
			book:  &AddressBook{LANIPs: []string{"192.168.1.10"}, LANIP: "192.168.1.10", PublicErr: errNoNetwork},
			want:  []string{":1935"},
			kinds: []ListenKind{ListenPublic},
		},
		{
			name:  "wildcard wins over a specific address on the same port",
			spec:  ":1935,192.168.1.10:1935",
			book:  testBook(),
			want:  []string{":1935"},
			kinds: []ListenKind{ListenAll},
		},
		{
			name:  "two explicit addresses on different hosts",
			spec:  "10.0.0.1:1935,10.0.0.2:1935",
			book:  testBook(),
			want:  []string{"10.0.0.1:1935", "10.0.0.2:1935"},
			kinds: []ListenKind{ListenExplicit, ListenExplicit},
		},
		{
			name:    "bad port",
			spec:    "public:http",
			book:    testBook(),
			wantErr: true,
		},
		{
			name:    "bad port range",
			spec:    ":70000",
			book:    testBook(),
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseListenSpec(tc.spec, tc.book, defaultRTMPPort)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %d targets %v, want %d", len(got), got, len(tc.want))
			}
			for i, want := range tc.want {
				if got[i].Addr != want {
					t.Errorf("target %d addr = %q, want %q", i, got[i].Addr, want)
				}
				if tc.kinds != nil && got[i].Kind != tc.kinds[i] {
					t.Errorf("target %d kind = %q, want %q", i, got[i].Kind, tc.kinds[i])
				}
			}
		})
	}
}

func TestParseListenSpecFallbackNote(t *testing.T) {
	book := &AddressBook{PublicIP: "203.0.113.7", PublicOnHost: false}
	got, err := ParseListenSpec("public:1935", book, defaultRTMPPort)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !got[0].Wildcard {
		t.Fatalf("want a single wildcard target, got %v", got)
	}
	if !strings.Contains(got[0].Note, "NAT") {
		t.Fatalf("fallback note should explain NAT, got %q", got[0].Note)
	}
}

// TestDualListenersShareStreams proves that the extra listeners are front doors
// to the same server: publish on one, play on the other.
func TestDualListenersShareStreams(t *testing.T) {
	s := testServer(t, Config{RTMPAddr: "127.0.0.1:0,127.0.0.1:0"})

	lns := s.Listeners()
	if len(lns) != 2 {
		t.Fatalf("want 2 listeners, got %d", len(lns))
	}
	if lns[0].Addr == lns[1].Addr {
		t.Fatalf("listeners share an address: %s", lns[0].Addr)
	}

	pubURL := "rtmp://" + lns[0].Addr + "/live/dual"
	pub, err := Dial(pubURL)
	if err != nil {
		t.Fatalf("dial publisher on %s: %v", lns[0].Addr, err)
	}
	t.Cleanup(func() { _ = pub.Close() })
	if err := pub.Connect(); err != nil {
		t.Fatal(err)
	}
	if err := pub.CreateStream(); err != nil {
		t.Fatal(err)
	}
	if err := pub.Publish(pub.Stream); err != nil {
		t.Fatal(err)
	}

	_ = pub.SendAVCConfig()
	_ = pub.WriteVideo(0, videoFrame(0, true, 16*1024))

	viewer, err := Dial("rtmp://" + lns[1].Addr + "/live/dual")
	if err != nil {
		t.Fatalf("dial viewer on %s: %v", lns[1].Addr, err)
	}
	t.Cleanup(func() { _ = viewer.Close() })
	if err := viewer.Connect(); err != nil {
		t.Fatal(err)
	}
	if err := viewer.CreateStream(); err != nil {
		t.Fatal(err)
	}
	if err := viewer.Play(viewer.Stream); err != nil {
		t.Fatal(err)
	}

	msg := nextVideo(t, viewer, 2*time.Second)
	if msg.TypeID != MsgVideo {
		t.Fatalf("viewer got message type %d, want video", msg.TypeID)
	}
	if !isVideoKeyframe(msg.Payload) {
		t.Fatal("viewer on the second listener did not get the cached keyframe")
	}
}

func TestHTTPListeners(t *testing.T) {
	s := testServer(t, Config{HTTPAddr: "127.0.0.1:0,127.0.0.1:0"})

	lns := s.HTTPListeners()
	if len(lns) != 2 {
		t.Fatalf("want 2 HTTP listeners, got %d", len(lns))
	}
	for _, ln := range lns {
		resp, err := http.Get("http://" + ln.Addr + "/healthz")
		if err != nil {
			t.Fatalf("healthz on %s: %v", ln.Addr, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("healthz on %s returned %d", ln.Addr, resp.StatusCode)
		}
	}
}
