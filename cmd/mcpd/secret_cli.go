package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/user"
	"runtime"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/ahodges22/mcpd/internal/config"
	"github.com/ahodges22/mcpd/internal/secretstore"
)

const (
	secretCLITimeout             = 5 * time.Second
	secretChallengeTimeout       = 2 * time.Second
	secretDaemonOperationTimeout = 10 * time.Second
	secretDaemonTimeout          = secretChallengeTimeout + secretDaemonOperationTimeout
)

var errSecretRedirect = errors.New("daemon redirected the secret request; refusing to forward it")

type secretCommandDeps struct {
	stdin              io.Reader
	stdout             io.Writer
	stderr             io.Writer
	httpClient         *http.Client
	isTerminal         func() bool
	readPassword       func() ([]byte, error)
	currentUID         func() int
	openProvider       func(string, *config.Secrets) (secretstore.Provider, error)
	credentialIdentity func() string
}

type secretAPIResponse struct {
	Status     string                         `json:"status,omitempty"`
	Warning    string                         `json:"warning,omitempty"`
	Error      string                         `json:"error,omitempty"`
	Dependents []secretstore.ConsumerIdentity `json:"dependents,omitempty"`
	Secrets    []secretstore.SecretStatus     `json:"secrets,omitempty"`
}

func defaultSecretCommandDeps() secretCommandDeps {
	return secretCommandDeps{
		stdin:  os.Stdin,
		stdout: os.Stdout,
		stderr: os.Stderr,
		httpClient: &http.Client{
			Timeout:       secretDaemonTimeout,
			CheckRedirect: refuseSecretRedirect,
		},
		isTerminal: func() bool { return term.IsTerminal(int(os.Stdin.Fd())) },
		readPassword: func() ([]byte, error) {
			return term.ReadPassword(int(os.Stdin.Fd()))
		},
		currentUID:         os.Geteuid,
		openProvider:       openSecretProvider,
		credentialIdentity: currentCredentialIdentity,
	}
}

func runSecret(args []string, deps secretCommandDeps) error {
	deps = fillSecretCommandDeps(deps)
	if len(args) == 0 {
		return errors.New("usage: mcpd secret <set|status|retry|remove>")
	}
	operation := args[0]
	fs := flag.NewFlagSet("mcpd secret "+operation, flag.ContinueOnError)
	fs.SetOutput(deps.stderr)
	addr := fs.String("addr", "127.0.0.1:7420", "running daemon address")
	cfgPath := fs.String("config", defaultPath("XDG_CONFIG_HOME", ".config", "config.json"), "declaration file")
	statePath := fs.String("state", defaultPath("XDG_STATE_HOME", ".local/state", ""), "state directory")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	names := fs.Args()
	switch operation {
	case "set", "remove":
		if len(names) != 1 {
			return fmt.Errorf("secret %s requires exactly one name; values are read from hidden terminal input or stdin", operation)
		}
	case "status", "retry":
		if len(names) != 0 {
			return fmt.Errorf("secret %s accepts no names or values", operation)
		}
	default:
		return fmt.Errorf("unknown secret operation %q", operation)
	}

	var value []byte
	if operation == "set" {
		var err error
		value, err = readSecretValue(deps)
		if err != nil {
			return err
		}
		defer clear(value)
		if err := secretstore.ValidateValue(string(value)); err != nil {
			return err
		}
	}

	daemonCtx, cancelDaemon := context.WithTimeout(context.Background(), secretDaemonTimeout)
	response, reachable, err := callSecretDaemon(daemonCtx, deps.httpClient, *addr, *statePath, operation, firstName(names), value)
	cancelDaemon()
	if reachable {
		if err != nil {
			return err
		}
		printSecretResult(deps.stdout, operation, firstName(names), response)
		return nil
	}
	offlineCtx, cancelOffline := context.WithTimeout(context.Background(), secretCLITimeout)
	defer cancelOffline()
	if err := runSecretOffline(offlineCtx, deps, operation, firstName(names), value, *cfgPath, *statePath, *addr); err != nil {
		return err
	}
	return nil
}

func fillSecretCommandDeps(deps secretCommandDeps) secretCommandDeps {
	defaults := defaultSecretCommandDeps()
	if deps.stdin == nil {
		deps.stdin = defaults.stdin
	}
	if deps.stdout == nil {
		deps.stdout = defaults.stdout
	}
	if deps.stderr == nil {
		deps.stderr = defaults.stderr
	}
	if deps.httpClient == nil {
		deps.httpClient = defaults.httpClient
	}
	if deps.isTerminal == nil {
		deps.isTerminal = func() bool { return false }
	}
	if deps.readPassword == nil {
		deps.readPassword = defaults.readPassword
	}
	if deps.currentUID == nil {
		deps.currentUID = defaults.currentUID
	}
	if deps.openProvider == nil {
		deps.openProvider = defaults.openProvider
	}
	if deps.credentialIdentity == nil {
		deps.credentialIdentity = defaults.credentialIdentity
	}
	return deps
}

func readSecretValue(deps secretCommandDeps) ([]byte, error) {
	if !deps.isTerminal() {
		return readSecretStream(deps.stdin)
	}
	fmt.Fprint(deps.stderr, "Secret value: ")
	value, err := deps.readPassword()
	fmt.Fprintln(deps.stderr)
	if err != nil {
		return nil, fmt.Errorf("read hidden secret input: %w", err)
	}
	return removeOneTerminator(value), nil
}

func readSecretStream(reader io.Reader) ([]byte, error) {
	value, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("read secret from stdin: %w", err)
	}
	return removeOneTerminator(value), nil
}

func removeOneTerminator(value []byte) []byte {
	if bytes.HasSuffix(value, []byte("\r\n")) {
		return value[:len(value)-2]
	}
	if bytes.HasSuffix(value, []byte("\n")) {
		return value[:len(value)-1]
	}
	return value
}

func callSecretDaemon(ctx context.Context, client *http.Client, addr, state, operation, name string, value []byte) (secretAPIResponse, bool, error) {
	baseURL, err := daemonBaseURL(addr)
	if err != nil {
		return secretAPIResponse{}, true, err
	}
	challengeCtx, cancelChallenge := context.WithTimeout(ctx, secretChallengeTimeout)
	reachable, err := authenticateSecretDaemon(challengeCtx, client, baseURL, state)
	cancelChallenge()
	if err != nil || !reachable {
		return secretAPIResponse{}, reachable, err
	}
	operationCtx, cancelOperation := context.WithTimeout(ctx, secretDaemonOperationTimeout)
	defer cancelOperation()
	method, path := http.MethodPost, "/api/secrets/"+url.PathEscape(name)
	var body io.Reader
	switch operation {
	case "set":
		encoded, err := json.Marshal(map[string]string{"value": string(value)})
		if err != nil {
			return secretAPIResponse{}, true, fmt.Errorf("encode secret request: %w", err)
		}
		defer clear(encoded)
		body = bytes.NewReader(encoded)
	case "remove":
		path += "/remove"
		body = strings.NewReader("{}")
	case "status":
		method, path = http.MethodGet, "/api/secrets/-/status"
	case "retry":
		path = "/api/secrets/-/retry"
		body = strings.NewReader("{}")
	case "refresh":
		path += "/refresh"
		body = strings.NewReader("{}")
	}
	req, err := http.NewRequestWithContext(operationCtx, method, baseURL+path, body)
	if err != nil {
		return secretAPIResponse{}, true, fmt.Errorf("build daemon request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	safeClient := *client
	safeClient.CheckRedirect = refuseSecretRedirect
	res, err := safeClient.Do(req)
	if err != nil {
		if errors.Is(err, errSecretRedirect) {
			return secretAPIResponse{}, true, errSecretRedirect
		}
		return secretAPIResponse{}, true, fmt.Errorf("daemon secret request failed: %w", err)
	}
	defer res.Body.Close()
	var response secretAPIResponse
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&response); err != nil && res.StatusCode >= 200 && res.StatusCode < 300 {
		return secretAPIResponse{}, true, fmt.Errorf("decode daemon response: %w", err)
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		if response.Error != "" {
			return secretAPIResponse{}, true, errors.New(response.Error)
		}
		return secretAPIResponse{}, true, fmt.Errorf("daemon secret request returned %s", res.Status)
	}
	return response, true, nil
}

func authenticateSecretDaemon(ctx context.Context, client *http.Client, baseURL, state string) (bool, error) {
	authenticator, authErr := secretstore.LoadControlAuthenticator(state)
	nonce, err := secretstore.NewControlNonce()
	if err != nil {
		return true, fmt.Errorf("create daemon challenge: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/secrets/-/challenge?nonce="+url.QueryEscape(nonce), nil)
	if err != nil {
		return true, fmt.Errorf("build daemon challenge: %w", err)
	}
	safeClient := *client
	safeClient.CheckRedirect = refuseSecretRedirect
	res, err := safeClient.Do(req)
	if err != nil {
		if errors.Is(err, errSecretRedirect) {
			return true, errSecretRedirect
		}
		return false, err
	}
	defer res.Body.Close()
	if authErr != nil {
		return true, fmt.Errorf(
			"daemon is reachable but secret control key %q in state directory %q is unavailable: %w",
			secretstore.ControlKeyFile,
			state,
			authErr,
		)
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return true, fmt.Errorf("daemon challenge returned %s", res.Status)
	}
	var response struct {
		Proof string `json:"proof"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 4096)).Decode(&response); err != nil {
		return true, fmt.Errorf("decode daemon challenge: %w", err)
	}
	if !authenticator.Verify(nonce, response.Proof) {
		return true, errors.New("daemon identity challenge failed")
	}
	return true, nil
}

func daemonBaseURL(addr string) (string, error) {
	raw := strings.TrimRight(addr, "/")
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid -addr %q: %w", addr, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("invalid -addr %q: scheme must be http or https", addr)
	}
	if parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("invalid -addr %q: expected only a loopback host and port", addr)
	}
	if err := requireLoopbackAddr(parsed.Host); err != nil {
		return "", err
	}
	return parsed.String(), nil
}

func refuseSecretRedirect(*http.Request, []*http.Request) error { return errSecretRedirect }

func runSecretOffline(ctx context.Context, deps secretCommandDeps, operation, name string, value []byte, cfgPath, statePath, addr string) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	if cfg.Secrets == nil {
		return errors.New("secret storage is not configured")
	}
	state, created, ownerUID, err := prepareOfflineState(statePath, deps.currentUID())
	if err != nil {
		return err
	}
	provider, err := deps.openProvider(state, cfg.Secrets)
	if err != nil {
		return offlineProviderError(cfg.Secrets.Provider, err)
	}
	coordinator := secretstore.NewResolutionCoordinator(cfg, provider, func(string) (string, bool) { return "", false }, secretstore.ResolutionTuning{}, nil)
	if created {
		fmt.Fprintf(deps.stdout, "created daemon state for uid %d\n", ownerUID)
	}
	switch operation {
	case "set":
		if err := provider.Set(ctx, name, string(value)); err != nil {
			return offlineProviderError(cfg.Secrets.Provider, err)
		}
		if cfg.Secrets.Provider == config.SecretProviderFile {
			if err := secretstore.ValidateStateDir(state); err != nil {
				return err
			}
		}
		fmt.Fprintf(deps.stdout, "set %s directly in the configured provider\n", name)
		printOfflineNamespace(deps.stdout, deps, cfg.Secrets.Provider)
		fmt.Fprintln(deps.stdout, "daemon environment shadowing cannot be determined until daemon startup")
		bestEffortNotify(deps.httpClient, addr, state, name)
	case "remove":
		dependents := coordinator.Dependents(name)
		if err := provider.Delete(ctx, name); err != nil {
			return offlineProviderError(cfg.Secrets.Provider, err)
		}
		if cfg.Secrets.Provider == config.SecretProviderFile {
			if err := secretstore.ValidateStateDir(state); err != nil {
				return err
			}
		}
		fmt.Fprintf(deps.stdout, "removed %s directly from the configured provider\n", name)
		printDependents(deps.stdout, dependents)
		printOfflineNamespace(deps.stdout, deps, cfg.Secrets.Provider)
		bestEffortNotify(deps.httpClient, addr, state, name)
	case "status", "retry":
		if operation == "retry" {
			if native, ok := provider.(secretstore.NativeProvider); ok {
				native.Retry()
			}
		}
		printStatuses(deps.stdout, coordinator.Status(ctx))
		fmt.Fprintln(deps.stdout, "offline status cannot determine the daemon environment")
		printOfflineNamespace(deps.stdout, deps, cfg.Secrets.Provider)
	}
	return nil
}

func prepareOfflineState(path string, currentUID int) (string, bool, int, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		state, err := secretstore.EnsureStateDir(path)
		return state, true, currentUID, err
	}
	if err != nil {
		return "", false, 0, fmt.Errorf("inspect state directory: %w", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", false, 0, errors.New("state directory POSIX ownership is unavailable")
	}
	ownerUID := int(stat.Uid)
	if ownerUID != currentUID {
		return "", false, ownerUID, fmt.Errorf("state directory owner uid %d does not match effective uid %d", ownerUID, currentUID)
	}
	if err := secretstore.ValidateStateDir(path); err != nil {
		return "", false, ownerUID, err
	}
	state, err := secretstore.EnsureStateDir(path)
	return state, false, ownerUID, err
}

func openSecretProvider(state string, declaration *config.Secrets) (secretstore.Provider, error) {
	switch declaration.Provider {
	case config.SecretProviderFile:
		return secretstore.NewFileStore(state)
	case config.SecretProviderNative:
		executable, err := os.Executable()
		if err != nil {
			return nil, err
		}
		return secretstore.NewNativeStore(state, executable)
	default:
		return nil, fmt.Errorf("unsupported secret provider %q", declaration.Provider)
	}
}

func offlineProviderError(provider config.SecretProvider, err error) error {
	if provider != config.SecretProviderNative {
		return err
	}
	return fmt.Errorf("native credential operation failed in the current login session; use the intended unlocked user session or explicitly configure the file provider: %w", err)
}

func currentCredentialIdentity() string {
	uid := os.Geteuid()
	name := fmt.Sprintf("uid %d", uid)
	if current, err := user.Current(); err == nil && current.Username != "" {
		name = fmt.Sprintf("%s (uid %d)", current.Username, uid)
	}
	namespace := "Secret Service login session"
	if runtime.GOOS == "darwin" {
		namespace = "macOS Keychain login session"
	}
	return name + " " + namespace
}

func printOfflineNamespace(out io.Writer, deps secretCommandDeps, provider config.SecretProvider) {
	if provider == config.SecretProviderNative {
		fmt.Fprintln(out, "native credential identity: "+deps.credentialIdentity())
	}
}

func bestEffortNotify(client *http.Client, addr, state, name string) {
	ctx, cancel := context.WithTimeout(context.Background(), secretCLITimeout)
	defer cancel()
	_, _, _ = callSecretDaemon(ctx, client, addr, state, "refresh", name, nil)
}

func printSecretResult(out io.Writer, operation, name string, response secretAPIResponse) {
	switch operation {
	case "set":
		fmt.Fprintf(out, "set %s\n", name)
	case "remove":
		fmt.Fprintf(out, "removed %s\n", name)
		printDependents(out, response.Dependents)
	case "retry":
		fmt.Fprintln(out, "secret provider retry requested")
	case "status":
		printStatuses(out, response.Secrets)
	}
	if response.Warning != "" {
		fmt.Fprintln(out, "warning: "+response.Warning)
	}
}

func printStatuses(out io.Writer, statuses []secretstore.SecretStatus) {
	for _, status := range statuses {
		condition := ""
		if status.Condition != "" {
			condition = " condition=" + string(status.Condition)
		}
		fmt.Fprintf(out, "%s\t%s%s\n", status.Name, status.Source, condition)
		printDependents(out, status.Consumers)
	}
}

func printDependents(out io.Writer, dependents []secretstore.ConsumerIdentity) {
	for _, consumer := range dependents {
		fmt.Fprintf(out, "  consumer: %s/%s\n", consumer.Kind, consumer.Name)
	}
}

func firstName(names []string) string {
	if len(names) == 0 {
		return ""
	}
	return names[0]
}
