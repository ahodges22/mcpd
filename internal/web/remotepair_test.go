package web

import (
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestPairingURLs(t *testing.T) {
	addrs := []netip.Addr{
		netip.MustParseAddr("192.168.1.10"),
		netip.MustParseAddr("169.254.9.9"),   // v4 link-local: excluded
		netip.MustParseAddr("fe80::1"),       // v6 link-local: excluded
		netip.MustParseAddr("2001:db8::5"),   // global v6
		netip.MustParseAddr("100.66.204.87"), // CGNAT: the peer gate refuses these, so never advertised
		netip.MustParseAddr("127.0.0.1"),     // loopback: excluded
	}
	tok := "00112233445566778899aabbccddeeff"

	t.Run("dual stack emits v4 plus hostname; v6 addresses stay off the list", func(t *testing.T) {
		got := pairingURLs("", 7421, tok, addrs, "desk", "")
		want := []string{
			"http://192.168.1.10:7421/?token=" + tok,
			"http://desk:7421/?token=" + tok,
		}
		if !slices.Equal(got, want) {
			t.Fatalf("got %v want %v", got, want)
		}
	})
	t.Run("a v6-only wildcard keeps v6 addresses, bracketed", func(t *testing.T) {
		got := pairingURLs("::", 7421, tok, addrs, "desk", "")
		want := []string{"http://[2001:db8::5]:7421/?token=" + tok}
		if !slices.Equal(got, want) {
			t.Fatalf("got %v want %v", got, want)
		}
	})
	t.Run("v4 wildcard emits v4 only, no hostname", func(t *testing.T) {
		got := pairingURLs("0.0.0.0", 7421, tok, addrs, "desk", "")
		want := []string{"http://192.168.1.10:7421/?token=" + tok}
		if !slices.Equal(got, want) {
			t.Fatalf("got %v want %v", got, want)
		}
	})
	t.Run("specific bind IP is the only candidate with the bound port", func(t *testing.T) {
		got := pairingURLs("192.168.1.10", 9999, tok, addrs, "desk", "")
		want := []string{"http://192.168.1.10:9999/?token=" + tok}
		if !slices.Equal(got, want) {
			t.Fatalf("got %v want %v", got, want)
		}
	})
	t.Run("hostname bind is the only candidate", func(t *testing.T) {
		got := pairingURLs("desk.local", 7421, tok, addrs, "desk", "")
		want := []string{"http://desk.local:7421/?token=" + tok}
		if !slices.Equal(got, want) {
			t.Fatalf("got %v want %v", got, want)
		}
	})
	t.Run("advertised origin leads and the fallbacks follow", func(t *testing.T) {
		got := pairingURLs("192.168.1.10", 9999, tok, addrs, "desk", "https://mcpd.home.example")
		want := []string{
			"https://mcpd.home.example/?token=" + tok,
			"http://192.168.1.10:9999/?token=" + tok,
		}
		if !slices.Equal(got, want) {
			t.Fatalf("got %v want %v", got, want)
		}
	})
}

func TestPrivatePeer(t *testing.T) {
	local := []netip.Prefix{
		netip.MustParsePrefix("2001:db8:aa::/64"),
		netip.MustParsePrefix("203.0.113.0/24"), // public v4 on a local interface
	}
	for addr, want := range map[string]bool{
		"192.168.1.9:5000": true, "10.1.2.3:80": true, "172.20.0.2:1": true,
		"127.0.0.1:9": true, "[::1]:9": true, "[fd00::5]:9": true, "[fe80::1]:9": true,
		"[2001:db8:aa::7]:443": true,  // global v6 inside a connected prefix: the LAN
		"[2001:db8:bb::7]:443": false, // global v6 off-prefix: not the LAN
		"203.0.113.7:443":      false, // public v4 stays refused even inside a local prefix
		"8.8.8.8:53":           false,
		"nonsense":             false,
		"":                     false,
	} {
		if got := privatePeer(addr, local); got != want {
			t.Errorf("privatePeer(%q) = %v, want %v", addr, got, want)
		}
	}
}

func TestRemoteTokenFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "remote-token")
	tok, err := newRemoteToken()
	if err != nil || !validRemoteToken(tok) {
		t.Fatalf("newRemoteToken: %q %v", tok, err)
	}
	if err := storeRemoteToken(path, tok); err != nil {
		t.Fatalf("store: %v", err)
	}
	got, ok := loadRemoteToken(path)
	if !ok || got != tok {
		t.Fatalf("load: %q %v", got, ok)
	}
	for name, contents := range map[string]string{
		"empty": "", "truncated": tok[:10], "garbage": "zz" + tok[2:],
	} {
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, ok := loadRemoteToken(path); ok {
			t.Fatalf("%s token accepted", name)
		}
	}
	os.Remove(path)
	if _, ok := loadRemoteToken(path); ok {
		t.Fatal("missing file accepted")
	}
}

func TestValidateAdvertise(t *testing.T) {
	for raw, want := range map[string]string{
		"https://mcpd.home.example":  "https://mcpd.home.example",
		"https://mcpd.home.example/": "https://mcpd.home.example",
		"http://mcpd.lan:8443":       "http://mcpd.lan:8443",
		"":                           "",
	} {
		got, err := validateAdvertise(raw)
		if err != nil || got != want {
			t.Errorf("validateAdvertise(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
	for _, bad := range []string{
		"mcpd.home.example",         // no scheme
		"ftp://mcpd.home.example",   // wrong scheme
		"https://",                  // no host
		"https://mcpd.example/path", // path
		"https://mcpd.example?x=1",  // query
		"https://user@mcpd.example", // userinfo
		"https://mcpd.example#frag", // fragment
	} {
		if got, err := validateAdvertise(bad); err == nil {
			t.Errorf("validateAdvertise(%q) accepted as %q", bad, got)
		}
	}
}

func TestCandidateInterface(t *testing.T) {
	for name, want := range map[string]bool{
		"eth0": true, "enp3s0": true, "wlan0": true, "wlp2s0": true, "en0": true,
		"docker0": false, "br-9f2a11": false, "veth12ab": false, "virbr0": false,
		"podman1": false, "tailscale0": false, "tun0": false, "tap3": false,
		"wg0": false, "utun4": false, "cni0": false, "flannel.1": false, "lxcbr0": false,
	} {
		if got := candidateInterface(name); got != want {
			t.Errorf("candidateInterface(%q) = %v, want %v", name, got, want)
		}
	}
}
