package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	servicepkg "github.com/ahodges22/mcpd/internal/service"
)

func TestRunDoctorChecksConfigServiceAndDaemon(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, []byte(`{"backends":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"backends":[],"tool_count":0,"unvectorized":-1,"serving":0}`)
	}))
	t.Cleanup(server.Close)
	var output strings.Builder
	deps := doctorCommandDeps{
		inspect: func() (servicepkg.State, error) {
			return servicepkg.State{Installed: true, Enabled: true, Running: true}, nil
		},
		httpClient: server.Client(),
		stdout:     &output,
		attempts:   1,
	}

	if err := runDoctor([]string{"--config", configPath, "--addr", strings.TrimPrefix(server.URL, "http://")}, deps); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"PASS config", "PASS service", "PASS daemon"} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("doctor output missing %q: %s", want, output.String())
		}
	}
}

func TestRunDoctorRejectsUnrelatedHTTP200(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "not mcpd")
	}))
	t.Cleanup(server.Close)
	deps := doctorCommandDeps{httpClient: server.Client(), attempts: 1}

	err := waitForDaemon(server.URL+"/api/status", deps)
	if err == nil {
		t.Fatal("doctor accepted an unrelated HTTP 200 response")
	}
}

func TestRunDoctorFailsWhenDaemonIsUnhealthy(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, []byte(`{"backends":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(server.Close)
	var output strings.Builder
	deps := doctorCommandDeps{
		inspect: func() (servicepkg.State, error) {
			return servicepkg.State{Installed: true, Enabled: true, Running: true}, nil
		},
		httpClient: server.Client(),
		stdout:     &output,
		attempts:   1,
	}

	err := runDoctor([]string{"--config", configPath, "--addr", strings.TrimPrefix(server.URL, "http://")}, deps)
	if err == nil {
		t.Fatal("doctor accepted an unhealthy daemon")
	}
	if !strings.Contains(output.String(), "FAIL daemon") {
		t.Fatalf("doctor output = %q", output.String())
	}
}
