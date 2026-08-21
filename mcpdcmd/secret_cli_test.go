package mcpdcmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ahodges22/mcpd/internal/config"
	"github.com/ahodges22/mcpd/internal/secretstore"
)

type cliProvider struct {
	setName  string
	setValue string
}

func (*cliProvider) Get(context.Context, string) (secretstore.Result, error) {
	return secretstore.Result{}, nil
}

func (p *cliProvider) Set(_ context.Context, name, value string) error {
	p.setName, p.setValue = name, value
	return nil
}

func (*cliProvider) Delete(context.Context, string) error { return nil }

func TestSecretSetNeverUsesArgument(t *testing.T) {
	deps := secretCommandDeps{
		stdin:  strings.NewReader("stdin-value\n"),
		stdout: io.Discard,
		stderr: io.Discard,
	}
	err := runSecret([]string{"set", "TOKEN", "argument-value"}, deps)
	if err == nil {
		t.Fatal("set accepted a value argument")
	}
	if strings.Contains(err.Error(), "argument-value") {
		t.Fatalf("error exposed the rejected value: %v", err)
	}
}

func TestSecretInputLineEndingRules(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{name: "none", in: "value", want: "value"},
		{name: "lf", in: "value\n", want: "value"},
		{name: "crlf", in: "value\r\n", want: "value"},
		{name: "one only", in: "value\n\n", want: "value\n"},
		{name: "no trim", in: " value \n", want: " value "},
		{name: "lone cr", in: "value\r", want: "value\r"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := readSecretStream(strings.NewReader(tc.in))
			if err != nil {
				t.Fatalf("readSecretStream: %v", err)
			}
			if string(got) != tc.want {
				t.Fatalf("value = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSecretCLIUsesDaemonFirst(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	authenticator, err := secretstore.EnsureControlAuthenticator(state)
	if err != nil {
		t.Fatalf("EnsureControlAuthenticator: %v", err)
	}
	var gotPath, gotBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if answerSecretChallenge(w, r, authenticator) {
			return
		}
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()
	var stdout bytes.Buffer
	deps := secretCommandDeps{
		stdin:      strings.NewReader("daemon-value\n"),
		stdout:     &stdout,
		stderr:     io.Discard,
		httpClient: server.Client(),
		openProvider: func(string, *config.Secrets) (secretstore.Provider, error) {
			t.Fatal("daemon success opened the provider directly")
			return nil, nil
		},
	}
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	err = runSecret([]string{"set", "--addr", server.URL, "--config", missing, "--state", state, "TOKEN"}, deps)
	if err != nil {
		t.Fatalf("runSecret: %v", err)
	}
	if gotPath != "/api/secrets/TOKEN" || !strings.Contains(gotBody, "daemon-value") {
		t.Fatalf("request path = %q, body = %q", gotPath, gotBody)
	}
	if !strings.Contains(stdout.String(), "set TOKEN") || strings.Contains(stdout.String(), "daemon-value") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestSecretSetRejectsNonLoopbackDestination(t *testing.T) {
	deps := secretCommandDeps{
		stdin:  strings.NewReader("secret-value\n"),
		stdout: io.Discard,
		stderr: io.Discard,
		httpClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			t.Fatal("non-loopback destination reached the HTTP transport")
			return nil, nil
		})},
	}
	err := runSecret([]string{"set", "--addr", "http://example.com:7420", "TOKEN"}, deps)
	if err == nil || !strings.Contains(err.Error(), "loopback") {
		t.Fatalf("error = %v", err)
	}
}

func TestSecretSetRefusesRedirectWithoutReplayingBody(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	if _, err := secretstore.EnsureControlAuthenticator(state); err != nil {
		t.Fatalf("EnsureControlAuthenticator: %v", err)
	}
	var targetCalls int
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		targetCalls++
	}))
	defer target.Close()
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", target.URL)
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer relay.Close()
	deps := secretCommandDeps{
		stdin:      strings.NewReader("secret-value\n"),
		stdout:     io.Discard,
		stderr:     io.Discard,
		httpClient: relay.Client(),
	}
	err := runSecret([]string{"set", "--addr", relay.URL, "--state", state, "TOKEN"}, deps)
	if err == nil || !strings.Contains(err.Error(), "redirect") {
		t.Fatalf("error = %v", err)
	}
	if targetCalls != 0 {
		t.Fatalf("redirect target calls = %d", targetCalls)
	}
}

func TestSecretSetRejectsUnauthenticatedLoopbackPeer(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	if _, err := secretstore.EnsureControlAuthenticator(state); err != nil {
		t.Fatalf("EnsureControlAuthenticator: %v", err)
	}
	secretRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/secrets/-/challenge" {
			_, _ = w.Write([]byte(`{"proof":"00"}`))
			return
		}
		secretRequests++
	}))
	defer server.Close()
	deps := secretCommandDeps{
		stdin:      strings.NewReader("secret-value\n"),
		stdout:     io.Discard,
		stderr:     io.Discard,
		httpClient: server.Client(),
	}
	err := runSecret([]string{"set", "--addr", server.URL, "--state", state, "TOKEN"}, deps)
	if err == nil || !strings.Contains(err.Error(), "identity challenge") {
		t.Fatalf("error = %v", err)
	}
	if secretRequests != 0 {
		t.Fatalf("unauthenticated peer received %d secret requests", secretRequests)
	}
}

func TestSecretSetDoesNotGoOfflineWhenControlKeyIsUnreadable(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	if err := os.Mkdir(state, 0o700); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"backends":{},"secrets":{"provider":"file"}}`), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	challengeRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/secrets/-/challenge" {
			challengeRequests++
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		t.Fatalf("unverified daemon received %s %s", r.Method, r.URL.Path)
	}))
	defer server.Close()
	provider := &cliProvider{}
	err := runSecret([]string{
		"set", "--addr", server.URL, "--config", cfgPath, "--state", state, "TOKEN",
	}, secretCommandDeps{
		stdin:      strings.NewReader("must-not-go-offline\n"),
		stdout:     io.Discard,
		stderr:     io.Discard,
		httpClient: server.Client(),
		openProvider: func(string, *config.Secrets) (secretstore.Provider, error) {
			return provider, nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), state) || !strings.Contains(err.Error(), secretstore.ControlKeyFile) {
		t.Fatalf("error = %v", err)
	}
	if provider.setName != "" || provider.setValue != "" {
		t.Fatalf("offline provider mutation = %q, %q", provider.setName, provider.setValue)
	}
	if challengeRequests != 1 {
		t.Fatalf("challenge requests = %d, want 1", challengeRequests)
	}
}

func TestSecretSetDoesNotGoOfflineAfterVerifiedDaemonTransportFailure(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	authenticator, err := secretstore.EnsureControlAuthenticator(state)
	if err != nil {
		t.Fatalf("EnsureControlAuthenticator: %v", err)
	}
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"backends":{},"secrets":{"provider":"file"}}`), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if answerSecretChallenge(w, r, authenticator) {
			return
		}
		if r.URL.Path == "/api/secrets/TOKEN" {
			panic(http.ErrAbortHandler)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()
	provider := &cliProvider{}
	err = runSecret([]string{
		"set", "--addr", server.URL, "--config", cfgPath, "--state", state, "TOKEN",
	}, secretCommandDeps{
		stdin:      strings.NewReader("must-not-go-offline\n"),
		stdout:     io.Discard,
		stderr:     io.Discard,
		httpClient: server.Client(),
		openProvider: func(string, *config.Secrets) (secretstore.Provider, error) {
			return provider, nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "daemon secret request failed") {
		t.Fatalf("error = %v", err)
	}
	if provider.setName != "" || provider.setValue != "" {
		t.Fatalf("offline provider mutation = %q, %q", provider.setName, provider.setValue)
	}
}

func TestOfflineSecretSetRejectsWrongOwner(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	if err := os.Mkdir(state, 0o700); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"backends":{},"secrets":{"provider":"file"}}`), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	deps := secretCommandDeps{
		stdin:      strings.NewReader("offline-value\n"),
		stdout:     io.Discard,
		stderr:     io.Discard,
		httpClient: &http.Client{Transport: failingRoundTripper{}},
		currentUID: func() int { return os.Geteuid() + 1 },
		openProvider: func(string, *config.Secrets) (secretstore.Provider, error) {
			t.Fatal("owner mismatch opened the provider")
			return nil, nil
		},
	}
	err := runSecret([]string{"set", "--addr", "http://127.0.0.1:1", "--config", cfgPath, "--state", state, "TOKEN"}, deps)
	if err == nil || !strings.Contains(err.Error(), "owner uid") || !strings.Contains(err.Error(), "effective uid") {
		t.Fatalf("error = %v", err)
	}
}

func TestOfflineNativeSetReportsNamespace(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	if err := os.Mkdir(state, 0o700); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"backends":{},"secrets":{"provider":"native"}}`), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	provider := &cliProvider{}
	var stdout bytes.Buffer
	deps := secretCommandDeps{
		stdin:      strings.NewReader("offline-value\n"),
		stdout:     &stdout,
		stderr:     io.Discard,
		httpClient: &http.Client{Transport: failingRoundTripper{}},
		currentUID: os.Geteuid,
		openProvider: func(string, *config.Secrets) (secretstore.Provider, error) {
			return provider, nil
		},
		credentialIdentity: func() string { return "alex (uid 501) macOS Keychain login session" },
	}
	err := runSecret([]string{"set", "--addr", "http://127.0.0.1:1", "--config", cfgPath, "--state", state, "TOKEN"}, deps)
	if err != nil {
		t.Fatalf("runSecret: %v", err)
	}
	if provider.setName != "TOKEN" || provider.setValue != "offline-value" {
		t.Fatalf("provider set = %q, %q", provider.setName, provider.setValue)
	}
	out := stdout.String()
	if !strings.Contains(out, "alex (uid 501) macOS Keychain login session") || !strings.Contains(out, "cannot be determined until daemon startup") {
		t.Fatalf("stdout = %q", out)
	}
	if strings.Contains(out, "sudo -u") || strings.Contains(out, "offline-value") {
		t.Fatalf("unsafe stdout = %q", out)
	}
}

type failingRoundTripper struct{}

func (failingRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("daemon unavailable")
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func answerSecretChallenge(w http.ResponseWriter, r *http.Request, authenticator *secretstore.ControlAuthenticator) bool {
	if r.URL.Path != "/api/secrets/-/challenge" {
		return false
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"proof": authenticator.Proof(r.URL.Query().Get("nonce"))})
	return true
}
