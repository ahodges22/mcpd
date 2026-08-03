package web

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/ahodges22/mcpd/internal/atomicfile"
)

// pairingURLs renders the connectable pairing URLs for a bound remote listener.
// The wildcard never appears: a wildcard bind enumerates the interfaces,
// filtered to the families the listener serves, and a specific IP or hostname
// bind is itself the single pairing authority. Link-local addresses are
// excluded because their zone identifier is host-relative and meaningless to
// another device.
func pairingURLs(bindHost string, port int, token string, addrs []netip.Addr, hostname, advertise string) []string {
	v4 := bindHost == "" || bindHost == "0.0.0.0"
	v6 := bindHost == "" || bindHost == "::"
	var hosts []string
	bind, bindErr := netip.ParseAddr(bindHost)
	switch {
	case bindHost != "" && bindErr != nil:
		// A hostname bind ("desk.local:7421"): the name the listener answers
		// on is the pairing authority, and the only one.
		hosts = []string{bindHost}
	case bindErr == nil && !bind.IsUnspecified():
		hosts = []string{bindHost}
	default:
		var v4hosts, v6hosts []string
		for _, a := range addrs {
			if a.IsLoopback() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() {
				continue
			}
			// Never advertise what the peer gate refuses: a CGNAT address
			// (Tailscale and carrier NAT live here) yields a URL whose own
			// connections would be turned away.
			if cgnat.Contains(a.Unmap()) {
				continue
			}
			if (a.Is4() || a.Is4In6()) && v4 {
				v4hosts = append(v4hosts, a.Unmap().String())
			} else if a.Is6() && !a.Is4In6() && v6 {
				v6hosts = append(v6hosts, a.String())
			}
		}
		// IPv6 addresses are the fallback, not a peer of IPv4: beside an IPv4
		// URL they are longer, harder to read, and reach the same daemon. They
		// appear only when no IPv4 address exists to advertise, which keeps an
		// IPv6-only host pairable. The listener itself serves both families
		// regardless, and the hostname candidate still resolves to AAAA for a
		// device that prefers it.
		hosts = v4hosts
		if len(hosts) == 0 {
			hosts = v6hosts
		}
		// The resolving device picks the record family, so a hostname is only
		// offered when the listener serves both.
		if v4 && v6 && hostname != "" {
			hosts = append(hosts, hostname)
		}
	}
	urls := make([]string, 0, len(hosts)+1)
	// The advertised origin leads: it is the address a reverse proxy serves,
	// and the interface URLs stay behind it as direct fallbacks.
	if advertise != "" {
		urls = append(urls, advertise+"/?token="+token)
	}
	for _, h := range hosts {
		urls = append(urls, "http://"+net.JoinHostPort(h, strconv.Itoa(port))+"/?token="+token)
	}
	return urls
}

// validateAdvertise canonicalizes a reverse-proxy origin: scheme, host, an
// optional port, and nothing else. Empty clears the setting and is valid.
func validateAdvertise(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("not a URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("the advertised origin must be http or https, got %q", u.Scheme)
	}
	if u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return "", errors.New("the advertised origin must be scheme://host[:port] with nothing after the host")
	}
	return u.Scheme + "://" + u.Host, nil
}

// privateAddr reports whether a peer address belongs on the local network.
// Private, loopback and link-local ranges qualify outright; a global IPv6
// address qualifies only inside a directly connected prefix, because a home
// LAN using ISP-delegated global IPv6 is still the LAN. The prefix exception
// never applies to IPv4: a public IPv4 subnet neighbour shares a connected
// prefix without being anyone's home network. Everything else is refused,
// which is what keeps a wildcard bind from serving past the LAN on a host
// that also has a public address.
func privateAddr(a netip.Addr, local []netip.Prefix) bool {
	if a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() {
		return true
	}
	if a.Unmap().Is4() {
		return false
	}
	for _, p := range local {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// effectivePeer resolves the address the peer gate judges. For a direct
// connection that is the remote address itself, and a forwarding header from
// such a peer refuses outright: a browser never sends one, so its presence
// means an unconfigured proxy whose clients this gate cannot see. For a
// trusted proxy the peer is the rightmost X-Forwarded-For hop that is not
// itself trusted, and a trusted proxy reporting no client refuses too.
func effectivePeer(r *http.Request, trusted []netip.Prefix) (netip.Addr, string) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return netip.Addr{}, "the remote surface serves the local network only"
	}
	direct, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, "the remote surface serves the local network only"
	}
	forwarded := r.Header.Values("X-Forwarded-For")
	if !trustedProxy(direct, trusted) {
		if len(forwarded) > 0 || r.Header.Get("Forwarded") != "" {
			return netip.Addr{}, "a forwarding header from an unlisted source is refused; declare the proxy in remote.trusted_proxies"
		}
		return direct, ""
	}
	var hops []string
	for _, v := range forwarded {
		hops = append(hops, strings.Split(v, ",")...)
	}
	for i := len(hops) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			return netip.Addr{}, "the trusted proxy did not report a judgeable client address"
		}
		if trustedProxy(a, trusted) {
			continue
		}
		return a, ""
	}
	return netip.Addr{}, "the trusted proxy did not report a judgeable client address"
}

func trustedProxy(a netip.Addr, trusted []netip.Prefix) bool {
	for _, p := range trusted {
		if p.Contains(a.Unmap()) {
			return true
		}
	}
	return false
}

// parseTrustedProxies canonicalizes the declared reverse-proxy sources: each
// entry an IP or a CIDR prefix. An invalid entry is dropped with a warning,
// which fails closed: an untrusted source cannot speak for its clients.
func parseTrustedProxies(raw []string) []netip.Prefix {
	var out []netip.Prefix
	for _, s := range raw {
		if p, err := netip.ParsePrefix(s); err == nil {
			out = append(out, p.Masked())
			continue
		}
		if a, err := netip.ParseAddr(s); err == nil {
			a = a.Unmap()
			out = append(out, netip.PrefixFrom(a, a.BitLen()))
			continue
		}
		slog.Warn("ignoring an invalid trusted proxy entry", "entry", s)
	}
	return out
}

// localPrefixes snapshots the directly connected networks. Called per request:
// the surface serves a human clicking a button, and a cached copy would go
// stale when Wi-Fi roams or DHCP renumbers.
func localPrefixes() []netip.Prefix {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var out []netip.Prefix
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if ok && !ipn.IP.IsLoopback() {
			if p, err := prefixOf(ipn); err == nil {
				out = append(out, p)
			}
		}
	}
	return out
}

func prefixOf(ipn *net.IPNet) (netip.Prefix, error) {
	a, ok := netip.AddrFromSlice(ipn.IP)
	if !ok {
		return netip.Prefix{}, fmt.Errorf("bad interface address %v", ipn)
	}
	ones, _ := ipn.Mask.Size()
	return a.Unmap().Prefix(ones)
}

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// virtualIfacePrefixes mark interfaces no other device can reach: container
// bridges, VPN tunnels and their kin. Their addresses would only pad the
// pairing list with links that cannot work, and the advertise setting is the
// override for anything this list gets wrong.
var virtualIfacePrefixes = []string{
	"docker", "br-", "veth", "virbr", "podman", "lxc", "cni", "flannel",
	"tailscale", "tun", "tap", "utun", "wg", "zt",
}

func candidateInterface(name string) bool {
	for _, p := range virtualIfacePrefixes {
		if strings.HasPrefix(name, p) {
			return false
		}
	}
	return true
}

func newRemoteToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func validRemoteToken(s string) bool {
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == 16
}

func loadRemoteToken(path string) (string, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	tok := strings.TrimSuffix(string(raw), "\n")
	if !validRemoteToken(tok) {
		return "", false
	}
	return tok, true
}

func storeRemoteToken(path, token string) error {
	return atomicfile.Write(path, []byte(token+"\n"), 0o600)
}

func deleteRemoteToken(path string) { os.Remove(path) }
