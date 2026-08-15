package tray

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

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

	t.Run("custom reason phrase", func(t *testing.T) {
		client, err := NewClient("127.0.0.1:7420")
		if err != nil {
			t.Fatalf("NewClient: %v", err)
		}
		client.http.Transport = responseTransport(http.StatusBadGateway, "502 peer-controlled detail", `{}`)

		_, err = client.Status(t.Context())
		if err == nil || !strings.Contains(err.Error(), "502 Bad Gateway") {
			t.Fatalf("Status error = %v, want canonical HTTP status", err)
		}
		if strings.Contains(err.Error(), "peer-controlled") {
			t.Errorf("Status error exposes the peer reason phrase: %v", err)
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

func TestClientAction(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", got)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		if string(body) != `{}` {
			t.Errorf("body = %q, want {}", body)
		}
		switch r.URL.EscapedPath() {
		case "/api/backends/alpha%2Fbeta/reconnect":
			fmt.Fprint(w, `{"status":"ok"}`)
		case "/api/backends/oauth%20backend/authorize":
			fmt.Fprint(w, `{"status":"pending","authorize_url":"https://login.example/authorize?state=one"}`)
		default:
			http.Error(w, "unexpected path", http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := clientForServer(t, server)

	if err := client.Reconnect(t.Context(), "alpha/beta"); err != nil {
		t.Fatalf("Reconnect: %v", err)
	}
	offered, err := client.Authorize(t.Context(), "oauth backend")
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if offered != "https://login.example/authorize?state=one" {
		t.Errorf("Authorize URL = %q, want provider URL", offered)
	}
	if got := requests.Load(); got != 2 {
		t.Errorf("requests = %d, want exactly one per action", got)
	}

	t.Run("response body is bounded", func(t *testing.T) {
		large := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, strings.Repeat(" ", 1<<20+1)+`{}`)
		}))
		defer large.Close()

		if err := clientForServer(t, large).Reconnect(t.Context(), "alpha"); err == nil || !strings.Contains(err.Error(), "response body exceeds") {
			t.Fatalf("Reconnect error = %v, want bounded-body refusal", err)
		}
	})

	t.Run("error detail is not exposed", func(t *testing.T) {
		failed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "backend-derived detail", http.StatusBadGateway)
		}))
		defer failed.Close()

		err := clientForServer(t, failed).Reconnect(t.Context(), "alpha")
		if err == nil || !strings.Contains(err.Error(), "502 Bad Gateway") {
			t.Fatalf("Reconnect error = %v, want HTTP status", err)
		}
		if strings.Contains(err.Error(), "backend-derived") {
			t.Errorf("Reconnect error exposes response detail: %v", err)
		}
	})

	t.Run("custom reason phrase is not exposed", func(t *testing.T) {
		client, err := NewClient("127.0.0.1:7420")
		if err != nil {
			t.Fatalf("NewClient: %v", err)
		}
		client.http.Transport = responseTransport(http.StatusBadGateway, "502 peer-controlled detail", `{}`)

		err = client.Reconnect(t.Context(), "alpha")
		if err == nil || !strings.Contains(err.Error(), "502 Bad Gateway") {
			t.Fatalf("Reconnect error = %v, want canonical HTTP status", err)
		}
		if strings.Contains(err.Error(), "peer-controlled") {
			t.Errorf("Reconnect error exposes the peer reason phrase: %v", err)
		}
	})

	t.Run("unsafe authorization URL is refused", func(t *testing.T) {
		unsafe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, `{"status":"pending","authorize_url":"http://login.example/authorize"}`)
		}))
		defer unsafe.Close()

		if _, err := clientForServer(t, unsafe).Authorize(t.Context(), "alpha"); err == nil {
			t.Fatal("Authorize accepted an external plain-HTTP URL")
		}
	})
}

func TestClientActionTimeout(t *testing.T) {
	client, err := NewClient("127.0.0.1:7420")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	client.http.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		deadline, ok := r.Context().Deadline()
		if !ok {
			t.Fatal("action request has no deadline")
		}
		if remaining := time.Until(deadline); remaining < 39*time.Second || remaining > 40*time.Second {
			t.Errorf("action deadline = %s, want a 40-second budget", remaining)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"status":"ok"}`)),
		}, nil
	})

	if err := client.Reconnect(context.Background(), "alpha"); err != nil {
		t.Fatalf("Reconnect: %v", err)
	}
}

func TestAuthorizeURL(t *testing.T) {
	accepted := []string{
		"https://login.example/authorize?client_id=one#consent",
		"http://localhost:8080/authorize",
		"http://127.42.0.9:8080/authorize",
		"http://[::1]:8080/authorize",
	}
	for _, raw := range accepted {
		t.Run("accept "+raw, func(t *testing.T) {
			var opened []string
			if err := OpenAuthorizeURL(t.Context(), raw, func(_ context.Context, target string) error {
				opened = append(opened, target)
				return nil
			}); err != nil {
				t.Fatalf("OpenAuthorizeURL(%q): %v", raw, err)
			}
			if !reflect.DeepEqual(opened, []string{raw}) {
				t.Errorf("opened = %q, want one exact URL argument", opened)
			}
		})
	}

	refused := []string{
		"", "/relative", "javascript:alert(1)", "data:text/plain,no", "https://",
		"http://login.example/authorize", "http://localhost.evil/authorize", "http://127.0.0.1.evil/authorize",
	}
	for _, raw := range refused {
		t.Run("refuse "+raw, func(t *testing.T) {
			called := false
			err := OpenAuthorizeURL(t.Context(), raw, func(context.Context, string) error {
				called = true
				return nil
			})
			if err == nil {
				t.Fatalf("OpenAuthorizeURL(%q) succeeded", raw)
			}
			if called {
				t.Fatalf("OpenAuthorizeURL(%q) invoked the opener", raw)
			}
		})
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

func responseTransport(code int, status, body string) roundTripFunc {
	return func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: code,
			Status:     status,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(body)),
		}, nil
	}
}
