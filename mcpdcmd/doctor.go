package mcpdcmd

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/ahodges22/mcpd/internal/config"
	servicepkg "github.com/ahodges22/mcpd/internal/service"
)

type doctorCommandDeps struct {
	inspect    func() (servicepkg.State, error)
	httpClient *http.Client
	stdout     io.Writer
	stderr     io.Writer
	attempts   int
	sleep      func(time.Duration)
}

func defaultDoctorCommandDeps() doctorCommandDeps {
	return doctorCommandDeps{
		inspect:    servicepkg.Inspect,
		httpClient: &http.Client{Timeout: time.Second},
		stdout:     os.Stdout,
		stderr:     os.Stderr,
		attempts:   20,
		sleep:      time.Sleep,
	}
}

func runDoctor(args []string, deps doctorCommandDeps) error {
	if deps.stdout == nil {
		deps.stdout = io.Discard
	}
	if deps.stderr == nil {
		deps.stderr = io.Discard
	}
	if deps.httpClient == nil {
		deps.httpClient = http.DefaultClient
	}
	if deps.attempts < 1 {
		deps.attempts = 1
	}
	if deps.sleep == nil {
		deps.sleep = func(time.Duration) {}
	}
	fs := flag.NewFlagSet("mcpd doctor", flag.ContinueOnError)
	fs.SetOutput(deps.stderr)
	configPath := fs.String("config", defaultPath("XDG_CONFIG_HOME", ".config", "config.json"), "declaration file")
	addr := fs.String("addr", "127.0.0.1:7420", "address mcpd serves on")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("mcpd doctor accepts no positional arguments")
	}
	if err := requireLoopbackAddr(*addr); err != nil {
		return err
	}

	var failures []error
	if _, err := config.Load(*configPath); err != nil {
		fmt.Fprintf(deps.stdout, "FAIL config: %v\n", err)
		failures = append(failures, err)
	} else {
		fmt.Fprintf(deps.stdout, "PASS config: %s\n", *configPath)
	}
	state, err := deps.inspect()
	if err != nil {
		fmt.Fprintf(deps.stdout, "FAIL service: %v\n", err)
		failures = append(failures, err)
	} else if !state.Installed || !state.Enabled || !state.Running {
		err = fmt.Errorf("%s", describeServiceState(state))
		fmt.Fprintf(deps.stdout, "FAIL service: %v\n", err)
		failures = append(failures, err)
	} else {
		fmt.Fprintf(deps.stdout, "PASS service: %s\n", describeServiceState(state))
	}

	dashboardURL := "http://" + *addr + "/"
	if err := waitForDaemon(dashboardURL+"api/status", deps); err != nil {
		fmt.Fprintf(deps.stdout, "FAIL daemon: %v\n", err)
		failures = append(failures, err)
	} else {
		fmt.Fprintf(deps.stdout, "PASS daemon: %s\n", dashboardURL)
	}
	if len(failures) > 0 {
		return errors.New("doctor found problems")
	}
	return nil
}

func waitForDaemon(url string, deps doctorCommandDeps) error {
	var last error
	for attempt := 0; attempt < deps.attempts; attempt++ {
		response, err := deps.httpClient.Get(url)
		if err == nil {
			if response.StatusCode != http.StatusOK {
				err = fmt.Errorf("GET %s: %s", url, response.Status)
			} else {
				var status map[string]json.RawMessage
				decodeErr := json.NewDecoder(response.Body).Decode(&status)
				if decodeErr != nil {
					err = fmt.Errorf("GET %s: response is not mcpd status JSON: %w", url, decodeErr)
				} else if !hasStatusFields(status) {
					err = fmt.Errorf("GET %s: response is not mcpd status JSON", url)
				} else {
					response.Body.Close()
					return nil
				}
			}
			response.Body.Close()
		}
		last = err
		if attempt+1 < deps.attempts {
			deps.sleep(100 * time.Millisecond)
		}
	}
	return last
}

func hasStatusFields(status map[string]json.RawMessage) bool {
	for _, field := range []string{"backends", "tool_count", "unvectorized", "serving"} {
		if _, ok := status[field]; !ok {
			return false
		}
	}
	return true
}
