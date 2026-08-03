package web

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
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
			// IPv6 interface addresses are advertised only when the listener
			// serves nothing else: beside an IPv4 URL they are longer, harder
			// to read, and reach the same daemon. The listener itself stays
			// dual-stack, and the hostname candidate still resolves to AAAA
			// for a device that prefers it.
			if (a.Is4() || a.Is4In6()) && v4 {
				hosts = append(hosts, a.Unmap().String())
			} else if a.Is6() && !a.Is4In6() && v6 && !v4 {
				hosts = append(hosts, a.String())
			}
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

// privatePeer reports whether a connection's remote address belongs on the
// local network. Private, loopback and link-local ranges qualify outright; a
// global IPv6 address qualifies only inside a directly connected prefix,
// because a home LAN using ISP-delegated global IPv6 is still the LAN. The
// prefix exception never applies to IPv4: a public IPv4 subnet neighbour
// shares a connected prefix without being anyone's home network. Everything
// else is refused, which is what keeps a wildcard bind from serving past the
// LAN on a host that also has a public address.
func privatePeer(remoteAddr string, local []netip.Prefix) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return false
	}
	a, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
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
