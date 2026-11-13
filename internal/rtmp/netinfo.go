package rtmp

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"
)

// AddressBook describes where this host can be reached. It is filled in once at
// startup and then only read, so it is safe to share between goroutines.
type AddressBook struct {
	// PublicIP is the address the internet sees for this host. Empty when it
	// could not be determined or when detection was switched off.
	PublicIP string
	// PublicOnHost reports whether PublicIP is configured on one of the local
	// interfaces. It is false behind NAT, where the public address lives on a
	// router or a cloud edge and packets only arrive via port forwarding.
	PublicOnHost bool
	// PublicSource is "flag", "detected" or "" (nothing was asked for).
	PublicSource string
	// PublicErr holds the reason detection failed, for the startup log.
	PublicErr error

	// LANIPs lists the local unicast IPv4 addresses, private ones first.
	LANIPs []string
	// LANIP is the preferred LAN address, empty when the host has none.
	LANIP string
}

// publicIPDisabled lists the spellings that turn detection off.
func publicIPDisabled(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "off", "none", "-", "false", "no":
		return true
	}
	return false
}

// autoPublicIP reports whether the value means "go find out".
func autoPublicIP(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "auto", "true", "yes", "detect":
		return true
	}
	return false
}

// HasIP reports whether the given address is configured on a local interface.
func (b *AddressBook) HasIP(ip string) bool {
	for _, local := range b.LANIPs {
		if local == ip {
			return true
		}
	}
	return false
}

// virtualIfaceHints mark adapters that exist only to host virtual machines or
// containers. They are perfectly valid IP addresses, which is exactly why they
// are dangerous as a default: a LAN URL pointing at a VirtualBox host-only
// adapter looks right in the banner and is unreachable from the network.
var virtualIfaceHints = []string{
	"virtualbox", "vmware", "vethernet", "hyper-v", "docker", "wsl",
	"vmnet", "tap", "tun", "loopback", "npcap", "zerotier", "tailscale",
	"vpns", "wireguard", "br-", "veth",
}

func looksVirtual(iface net.Interface) bool {
	name := strings.ToLower(iface.Name)
	for _, hint := range virtualIfaceHints {
		if strings.Contains(name, hint) {
			return true
		}
	}
	// A MAC starting with 0a:00:27 is a VirtualBox host-only adapter, whose
	// name is localised on Windows and therefore useless for matching.
	mac := strings.ToLower(iface.HardwareAddr.String())
	return strings.HasPrefix(mac, "0a:00:27")
}

// LocalIPv4s returns every non-loopback IPv4 address on the host, ordered by how
// likely it is to be the address a person on the same network should use:
// physical interfaces first, private addresses first, virtual adapters last.
func LocalIPv4s() []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}

	type candidate struct {
		ip      string
		private bool
		virtual bool
	}
	var found []candidate
	seen := make(map[string]bool)

	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		virtual := looksVirtual(iface)
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
			if ip4 == nil || ip4.IsLoopback() || ip4.IsLinkLocalUnicast() {
				continue
			}
			s := ip4.String()
			if seen[s] {
				continue
			}
			seen[s] = true
			found = append(found, candidate{ip: s, private: isPrivateIPv4(ip4), virtual: virtual})
		}
	}

	sort.SliceStable(found, func(i, j int) bool {
		if found[i].virtual != found[j].virtual {
			return !found[i].virtual
		}
		if found[i].private != found[j].private {
			return found[i].private
		}
		return false
	})

	out := make([]string, 0, len(found))
	for _, c := range found {
		out = append(out, c.ip)
	}
	return out
}

// isPrivateIPv4 covers RFC1918 plus the shared address space (100.64/10) that
// carrier-grade NAT hands out.
func isPrivateIPv4(ip net.IP) bool {
	if ip.IsPrivate() {
		return true
	}
	return len(ip) == 4 && ip[0] == 100 && ip[1] >= 64 && ip[1] <= 127
}

// publicIPEndpoints are plain-text echoes, tried in order. Several of them on
// purpose: any single one can be blocked or rate limited from a given network.
var publicIPEndpoints = []string{
	"https://api.ipify.org",
	"https://ipv4.icanhazip.com",
	"https://ifconfig.me/ip",
	"https://api.ip.sb/ip",
	"https://ipinfo.io/ip",
}

// detectPublicTimeout bounds the whole detection attempt. The echoes are raced
// in parallel, so in practice this is just the slowest single response.
const detectPublicTimeout = 4 * time.Second

// DetectPublicIPv4 asks a few echo services for the public IPv4 address of this
// host. The lookup is best effort: a host with no outbound access simply gets an
// error and keeps working.
func DetectPublicIPv4(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, detectPublicTimeout)
	defer cancel()

	client := &http.Client{
		Timeout: detectPublicTimeout,
		Transport: &http.Transport{
			Proxy:             http.ProxyFromEnvironment,
			DisableKeepAlives: true,
		},
	}

	type result struct {
		ip  string
		err error
	}
	// Buffered to len(): the losers finish after we return and must not block.
	results := make(chan result, len(publicIPEndpoints))
	for _, endpoint := range publicIPEndpoints {
		go func(endpoint string) {
			ip, err := fetchEchoIP(ctx, client, endpoint)
			results <- result{ip: ip, err: err}
		}(endpoint)
	}

	var lastErr error
	for range publicIPEndpoints {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case r := <-results:
			if r.err == nil {
				return r.ip, nil
			}
			lastErr = r.err
		}
	}
	return "", fmt.Errorf("public IP detection failed: %w", lastErr)
}

func fetchEchoIP(ctx context.Context, client *http.Client, endpoint string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "fastrtmp-public-ip")
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("%s: %w", endpoint, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s: status %d", endpoint, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 128))
	if err != nil {
		return "", fmt.Errorf("%s: %w", endpoint, err)
	}
	ip := net.ParseIP(strings.TrimSpace(string(body)))
	if ip == nil || ip.To4() == nil {
		return "", fmt.Errorf("%s: not an IPv4 address", endpoint)
	}
	if ip4 := ip.To4(); ip4.IsLoopback() || ip4.IsPrivate() || ip4.IsLinkLocalUnicast() {
		// A misconfigured proxy can answer with an internal address.
		return "", fmt.Errorf("%s: returned non-public address %s", endpoint, ip4)
	}
	return ip.To4().String(), nil
}
