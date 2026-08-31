// Package runtime exposes mcpd as a caller-owned component.
package runtime

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"

	"github.com/ahodges22/mcpd/internal/app"
	"github.com/ahodges22/mcpd/internal/config"
	"github.com/ahodges22/mcpd/internal/mcpsrv"
	"github.com/ahodges22/mcpd/internal/stateowner"
	internalversion "github.com/ahodges22/mcpd/internal/version"
)

// Version is the embedded mcpd component version.
var Version = internalversion.String()

type Paths struct {
	Config string
	State  string
}

// DefaultPaths resolves the standard per-user mcpd paths.
func DefaultPaths() Paths {
	return Paths{
		Config: defaultPath("XDG_CONFIG_HOME", ".config", "config.json"),
		State:  defaultPath("XDG_STATE_HOME", ".local/state", ""),
	}
}

func defaultPath(env, fallback, file string) string {
	base := os.Getenv(env)
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = "."
		}
		base = filepath.Join(home, fallback)
	}
	dir := filepath.Join(base, "mcpd")
	if file == "" {
		return dir
	}
	return filepath.Join(dir, file)
}

type Options struct {
	Paths            Paths
	OAuthCallbackURL string
	Owner            string
	Logger           *slog.Logger
}

type OwnershipError = stateowner.ConflictError
type OwnershipStatus struct {
	Held  bool   `json:"held"`
	Owner string `json:"owner,omitempty"`
	PID   int    `json:"pid,omitempty"`
}

// InspectOwnership reports ownership without creating, acquiring, or modifying a lock.
func InspectOwnership(paths Paths) (OwnershipStatus, error) {
	if paths == (Paths{}) {
		paths = DefaultPaths()
	}
	meta, held, err := stateowner.Inspect(paths.State)
	if err != nil {
		return OwnershipStatus{}, err
	}
	if !held {
		return OwnershipStatus{}, nil
	}
	return OwnershipStatus{Held: true, Owner: meta.Owner, PID: meta.PID}, nil
}

type SearchRequest = mcpsrv.SearchRequest
type SearchResponse = mcpsrv.SearchResponse
type SearchResult = mcpsrv.SearchResult
type DescribeRequest = mcpsrv.DescribeRequest
type DescribeResponse = mcpsrv.DescribeResponse
type InvokeRequest = mcpsrv.CallRequest

type StatusResponse struct {
	Backends     []BackendStatus `json:"backends"`
	ToolCount    int             `json:"tool_count"`
	Serving      int             `json:"serving"`
	Unvectorized int             `json:"unvectorized"`
	Search       *SearchStatus   `json:"search,omitempty"`
}

type SearchStatus struct {
	Model            string `json:"model"`
	CatalogTotal     int    `json:"catalog_total"`
	PendingBase      int    `json:"pending_base"`
	PendingExpansion int    `json:"pending_expansion"`
	QueueState       string `json:"queue_state"`
	Queued           bool   `json:"queued"`
	Running          bool   `json:"running"`
	Degraded         bool   `json:"degraded"`
	Error            string `json:"error,omitempty"`
}

type BackendStatus struct {
	Name              string `json:"name"`
	State             string `json:"state"`
	Label             string `json:"label"`
	LastError         string `json:"last_error,omitempty"`
	OAuth             bool   `json:"oauth"`
	TokenExpiry       string `json:"token_expiry,omitempty"`
	RecommendedAction string `json:"recommended_action,omitempty"`
}

type BackendSpec struct {
	Command        string            `json:"command,omitempty"`
	Args           []string          `json:"args,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	EnvPassthrough []string          `json:"env_passthrough,omitempty"`
	HTTPURL        string            `json:"http_url,omitempty"`
	Headers        map[string]string `json:"headers,omitempty"`
	Auth           string            `json:"auth,omitempty"`
	TimeoutSec     int               `json:"timeout,omitempty"`
}
type AddBackendRequest struct {
	Name string      `json:"name"`
	Spec BackendSpec `json:"spec"`
}
type ClientChangeRequest struct {
	Address string `json:"address,omitempty"`
	Apply   bool   `json:"apply"`
}
type ClientPlanResponse struct {
	Client   string   `json:"client"`
	Path     string   `json:"path"`
	Endpoint string   `json:"endpoint"`
	Notes    []string `json:"notes,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
	Applied  bool     `json:"applied"`
}
type SecretStatus struct {
	Name      string           `json:"name"`
	Consumers []SecretConsumer `json:"consumers"`
	Source    string           `json:"source"`
	Condition string           `json:"condition,omitempty"`
}
type SecretConsumer struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

type CompletionRequest struct {
	URL string `json:"url"`
}
type OperationResponse struct {
	Status   string   `json:"status,omitempty"`
	Error    string   `json:"error,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
}

type Runtime struct{ app *app.App }

func New(ctx context.Context, opts Options) (*Runtime, error) {
	paths := opts.Paths
	if paths == (Paths{}) {
		paths = DefaultPaths()
	}
	a, err := app.New(ctx, app.Options{
		Paths: app.Paths{Config: paths.Config, State: paths.State}, OAuthCallbackURL: opts.OAuthCallbackURL,
		Owner: opts.Owner, Logger: opts.Logger,
	})
	if err != nil {
		return nil, err
	}
	return &Runtime{app: a}, nil
}

func (r *Runtime) AdminHandler() http.Handler         { return r.app.AdminHandler() }
func (r *Runtime) ProtocolHandler() http.Handler      { return r.app.ProtocolHandler() }
func (r *Runtime) Shutdown(ctx context.Context) error { return r.app.Shutdown(ctx) }

// StandaloneHandler is retained for the mcpd command's HTML compatibility
// surface. Embedders should use AdminHandler.
func (r *Runtime) StandaloneHandler() http.Handler { return r.app.StandaloneHandler() }

// StandaloneRemote controls the legacy remote listener used by the mcpd command.
// Embedders should not need this surface.
type StandaloneRemote interface {
	Apply()
}

// StandaloneRemote configures, but does not start, the legacy remote listener.
func (r *Runtime) StandaloneRemote(addr, hostname string) StandaloneRemote {
	return r.app.ConfigureStandaloneRemote(addr, hostname)
}

// ValidateConfig validates a declaration without taking state ownership.
func ValidateConfig(path string) error { _, err := config.Load(path); return err }

// Initialize creates an empty standard declaration and restricted state directory.
// Existing declarations are validated and never replaced.
func Initialize(paths Paths) error {
	if paths == (Paths{}) {
		paths = DefaultPaths()
	}
	if err := os.MkdirAll(paths.State, 0o700); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	if err := os.Chmod(paths.State, 0o700); err != nil {
		return fmt.Errorf("restrict state directory: %w", err)
	}
	_, _, err := config.NewWriter(paths.Config)
	return err
}
