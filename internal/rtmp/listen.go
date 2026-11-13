package rtmp

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

// ListenKind says why an address is being bound. It only affects logging and
// the startup banner; every listener accepts exactly the same traffic.
type ListenKind string

const (
	// ListenAll binds every interface on the machine (0.0.0.0), which is what
	// ":1935" has always meant.
	ListenAll ListenKind = "all"
	// ListenPublic binds the internet-facing address.
	ListenPublic ListenKind = "public"
	// ListenLAN binds a local network address.
	ListenLAN ListenKind = "lan"
	// ListenExplicit binds an address exactly as the operator wrote it.
	ListenExplicit ListenKind = "explicit"
)

func (k ListenKind) String() string { return string(k) }

// ListenTarget is one concrete address the server should bind.
type ListenTarget struct {
	Kind ListenKind
	// Addr is the host:port handed to net.Listen.
	Addr string
	// Wildcard is true when Addr covers every interface.
	Wildcard bool
	// Note explains a fallback, e.g. a NATed public IP that cannot be bound.
	Note string
}

// String renders the target for logs.
func (t ListenTarget) String() string {
	return string(t.Kind) + " " + t.Addr
}

// describe renders the target for log lines and the banner. A symbolic target
// that had to fall back to a wildcard bind keeps its name, but says so.
func (t ListenTarget) describe() string {
	if t.Wildcard && t.Kind != ListenAll {
		return string(t.Kind) + " via all interfaces"
	}
	return string(t.Kind)
}

// Default ports used when a symbolic listen target omits one.
const (
	defaultRTMPPort = 1935
	defaultHTTPPort = 8080
)

// specWantsPublic reports whether a listen spec references the public address,
// i.e. whether resolving it is worth a network round trip.
func specWantsPublic(spec string) bool {
	lower := strings.ToLower(spec)
	for _, item := range strings.Split(lower, ",") {
		item = strings.TrimSpace(item)
		if item == "public" || strings.HasPrefix(item, "public:") {
			return true
		}
	}
	return false
}

// ParseListenSpec expands a comma separated listen specification into the
// concrete addresses to bind.
//
// Each item may be:
//
//	":1935", "0.0.0.0:1935", "1935"   every interface (the historical default)
//	"*:1935", "all:1935"              same, spelled out
//	"public:1935", "public"           the internet-facing address
//	"lan:1935", "lan"                 the local network address
//	"192.168.1.10:1935"               that address, verbatim
//
// Symbolic items are resolved against book; when the address they name is not
// available (no LAN interface, public address behind NAT, detection failure)
// the target degrades to a wildcard bind so the server still starts, and the
// reason is recorded in Note.
func ParseListenSpec(spec string, book *AddressBook, defaultPort int) ([]ListenTarget, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, nil
	}
	if book == nil {
		book = &AddressBook{}
	}

	var (
		parsed []ListenTarget
		ports  = make(map[string]bool)
	)
	for _, raw := range strings.Split(spec, ",") {
		item := strings.TrimSpace(raw)
		if item == "" {
			continue
		}
		t, port, err := resolveListenItem(item, book, defaultPort)
		if err != nil {
			return nil, err
		}
		if ports[port] && t.Wildcard {
			// Two wildcard binds on one port would collide.
			continue
		}
		ports[port] = ports[port] || t.Wildcard
		parsed = append(parsed, t)
	}
	if len(parsed) == 0 {
		return nil, fmt.Errorf("listen spec %q has no usable address", spec)
	}

	// A wildcard listener already answers on every unicast address of that
	// port, so specific bindings on the same port would fail with
	// "address already in use" (or, on some stacks, silently split traffic).
	// Keep the wildcard and drop the redundant specific ones.
	wildcardPorts := make(map[string]bool)
	for _, t := range parsed {
		if t.Wildcard {
			wildcardPorts[portOfAddr(t.Addr)] = true
		}
	}
	out := parsed[:0]
	for _, t := range parsed {
		if !t.Wildcard && wildcardPorts[portOfAddr(t.Addr)] {
			continue
		}
		out = append(out, t)
	}
	return out, nil
}

// resolveListenItem turns a single spec item into a bind target plus its port.
func resolveListenItem(item string, book *AddressBook, defaultPort int) (ListenTarget, string, error) {
	kind, rest, hasKind := cutKind(item)

	switch kind {
	case ListenAll:
		port, err := portOrDefault(rest, defaultPort)
		if err != nil {
			return ListenTarget{}, "", err
		}
		return ListenTarget{Kind: ListenAll, Addr: net.JoinHostPort("", port), Wildcard: true}, port, nil

	case ListenPublic:
		port, err := portOrDefault(rest, defaultPort)
		if err != nil {
			return ListenTarget{}, "", err
		}
		return resolveSymbolic(ListenPublic, book.PublicIP, port, book.PublicIP != "" && book.PublicOnHost, publicNote(book)), port, nil

	case ListenLAN:
		port, err := portOrDefault(rest, defaultPort)
		if err != nil {
			return ListenTarget{}, "", err
		}
		return resolveSymbolic(ListenLAN, book.LANIP, port, book.LANIP != "" && book.HasIP(book.LANIP), lanNote(book)), port, nil
	}

	if !hasKind {
		// No symbolic prefix: an explicit address, possibly just a port.
		if isAllDigits(item) {
			port := item
			return ListenTarget{Kind: ListenAll, Addr: net.JoinHostPort("", port), Wildcard: true}, port, nil
		}
		if strings.HasPrefix(item, ":") {
			port := item[1:]
			if _, err := normalizePort(port); err != nil {
				return ListenTarget{}, "", err
			}
			return ListenTarget{Kind: ListenAll, Addr: net.JoinHostPort("", port), Wildcard: true}, port, nil
		}
		host, port, err := net.SplitHostPort(item)
		if err != nil {
			return ListenTarget{}, "", fmt.Errorf("listen address %q: %w", item, err)
		}
		if _, err := normalizePort(port); err != nil {
			return ListenTarget{}, "", err
		}
		wildcard := host == "" || host == "0.0.0.0" || host == "*"
		kind := ListenExplicit
		if wildcard {
			host = ""
			kind = ListenAll
		}
		return ListenTarget{Kind: kind, Addr: net.JoinHostPort(host, port), Wildcard: wildcard}, port, nil
	}
	return ListenTarget{}, "", fmt.Errorf("listen address %q: unknown prefix %q (want public:, lan: or all:)", item, kind)
}

// cutKind splits "public:1935" into (public, 1935, true). It also accepts the
// bare aliases "public", "lan", "*" and "all".
func cutKind(item string) (ListenKind, string, bool) {
	lower := strings.ToLower(item)
	aliases := []struct {
		prefix string
		kind   ListenKind
	}{
		{"public", ListenPublic},
		{"lan", ListenLAN},
		{"all", ListenAll},
		{"*", ListenAll},
	}
	for _, a := range aliases {
		if lower == a.prefix {
			return a.kind, "", true
		}
		if strings.HasPrefix(lower, a.prefix+":") {
			return a.kind, item[len(a.prefix)+1:], true
		}
	}
	return ListenExplicit, "", false
}

// resolveSymbolic turns a symbolic target into a bind target. When the named
// address cannot be bound on this host - no interface, or a public address that
// lives on a router because the host is NATed - it degrades to a wildcard bind
// so the server still starts, and records why in Note.
func resolveSymbolic(kind ListenKind, ip, port string, bindable bool, note string) ListenTarget {
	if ip == "" || !bindable {
		return ListenTarget{
			Kind:     kind,
			Addr:     net.JoinHostPort("", port),
			Wildcard: true,
			Note:     note,
		}
	}
	return ListenTarget{Kind: kind, Addr: net.JoinHostPort(ip, port)}
}

func publicNote(book *AddressBook) string {
	if book.PublicErr != nil {
		return "public IP unknown (" + book.PublicErr.Error() + "), listening on all interfaces instead"
	}
	if book.PublicIP == "" {
		return "public IP detection disabled, listening on all interfaces instead"
	}
	if !book.PublicOnHost {
		return "public IP " + book.PublicIP + " is not on a local interface (NAT), " +
			"listening on all interfaces instead - forward the port on your router or cloud gateway"
	}
	return ""
}

func lanNote(book *AddressBook) string {
	if book.LANIP == "" {
		return "no LAN address found, listening on all interfaces instead"
	}
	if !book.HasIP(book.LANIP) {
		return "LAN address " + book.LANIP + " is not on a local interface, listening on all interfaces instead"
	}
	return ""
}

func portOrDefault(rest string, defaultPort int) (string, error) {
	rest = strings.TrimPrefix(rest, ":")
	if rest == "" {
		return strconv.Itoa(defaultPort), nil
	}
	return normalizePort(rest)
}

func normalizePort(p string) (string, error) {
	n, err := strconv.Atoi(p)
	if err != nil || n < 0 || n > 65535 {
		return "", fmt.Errorf("invalid port %q", p)
	}
	return strconv.Itoa(n), nil
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// portOfAddr returns the port part of a host:port string.
func portOfAddr(addr string) string {
	if _, p, err := net.SplitHostPort(addr); err == nil {
		return p
	}
	return addr
}
