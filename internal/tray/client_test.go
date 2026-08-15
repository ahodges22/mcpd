package tray

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestClientStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/status" {
			t.Errorf("request = %s %s, want GET /api/status", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"serving":1,"tool_count":9,"backends":[`+
			`{"name":"alpha","state":"up","label":"Serving","tool_count":4},`+
			`{"name":"beta","state":"needs-auth","label":"Needs authorizing","recommended_action":"authorize"}]}`)
	}))
	defer server.Close()

	client := clientForServer(t, server)
	transport, ok := client.http.Transport.(*http.Transport)
	if !ok || transport.Proxy != nil {
		t.Fatalf("transport = %#v, want a dedicated transport with no proxy callback", client.http.Transport)
	}

	got, err := client.Status(t.Context())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	want := Status{
		Serving: 1,
		Backends: []BackendStatus{
			{Name: "alpha", State: "up", Label: "Serving"},
			{Name: "beta", State: "needs-auth", Label: "Needs authorizing", RecommendedAction: ActionAuthorize},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Status() = %+v, want %+v", got, want)
	}
}

func TestClientStatusErrors(t *testing.T) {
	t.Run("non-success response", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "backend-derived detail must not escape", http.StatusServiceUnavailable)
		}))
		defer server.Close()

		_, err := clientForServer(t, server).Status(t.Context())
		if err == nil || !strings.Contains(err.Error(), "503 Service Unavailable") {
			t.Fatalf("Status error = %v, want HTTP status without response detail", err)
		}
		if strings.Contains(err.Error(), "backend-derived") {
			t.Errorf("Status error exposes response detail: %v", err)
		}
	})

	t.Run("malformed response", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, `{"serving":`)
		}))
		defer server.Close()

		if _, err := clientForServer(t, server).Status(t.Context()); err == nil {
			t.Fatal("Status accepted malformed JSON")
		}
	})

	t.Run("oversized response", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, strings.Repeat(" ", 1<<20+1)+`{}`)
		}))
		defer server.Close()

		_, err := clientForServer(t, server).Status(t.Context())
		if err == nil || !strings.Contains(err.Error(), "response body exceeds") {
			t.Fatalf("Status error = %v, want bounded-body refusal", err)
		}
	})

	t.Run("redirect", func(t *testing.T) {
		var targetCalled atomic.Bool
		target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			targetCalled.Store(true)
		}))
		defer target.Close()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target.URL, http.StatusFound)
		}))
		defer server.Close()

		if _, err := clientForServer(t, server).Status(t.Context()); err == nil {
			t.Fatal("Status followed a redirect")
		}
		if targetCalled.Load() {
			t.Fatal("Status reached the redirect target")
		}
	})

	t.Run("two second budget", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
		}))
		defer server.Close()

		started := time.Now()
		if _, err := clientForServer(t, server).Status(context.Background()); err == nil {
			t.Fatal("Status accepted a response beyond its poll budget")
		}
		if elapsed := time.Since(started); elapsed < 1500*time.Millisecond || elapsed > 3500*time.Millisecond {
			t.Errorf("Status returned after %s, want the two-second poll budget", elapsed)
		}
	})
}

func TestClientRejectsNonLoopback(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:7420", "127.0.0.2:7420", "localhost:7420", "[::1]:7420"} {
		if _, err := NewClient(addr); err != nil {
			t.Errorf("NewClient(%q) = %v, want accepted loopback", addr, err)
		}
	}
	for _, addr := range []string{
		"", ":7420", "0.0.0.0:7420", "192.168.1.10:7420", "example.com:7420", "127.0.0.1:0",
		"127.0.0.1", "http://127.0.0.1:7420", "localhost:7420/path",
	} {
		if _, err := NewClient(addr); err == nil {
			t.Errorf("NewClient(%q) accepted a non-loopback or malformed address", addr)
		}
	}
}

func clientForServer(t *testing.T, server *httptest.Server) *Client {
	t.Helper()
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}
	client, err := NewClient(parsed.Host)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return client
}
