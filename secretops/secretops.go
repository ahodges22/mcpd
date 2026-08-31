// Package secretops performs bounded secret-provider operations while no runtime owns state.
package secretops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/ahodges22/mcpd/internal/config"
	"github.com/ahodges22/mcpd/internal/secretstore"
	"github.com/ahodges22/mcpd/internal/stateowner"
)

const defaultTimeout = 5 * time.Second

type Paths struct{ Config, State string }
type Consumer struct{ Kind, Name string }
type SecretStatus struct {
	Name      string     `json:"name"`
	Consumers []Consumer `json:"consumers"`
	Source    string     `json:"source"`
	Condition string     `json:"condition,omitempty"`
}

func Status(ctx context.Context, paths Paths) ([]SecretStatus, error) {
	var out []SecretStatus
	err := withCoordinator(ctx, paths, func(ctx context.Context, c *secretstore.ResolutionCoordinator, _ secretstore.Provider) error {
		out = convertStatuses(c.Status(ctx))
		return nil
	})
	return out, err
}

func Set(ctx context.Context, paths Paths, name, value string) ([]Consumer, error) {
	if err := secretstore.ValidateValue(value); err != nil {
		return nil, err
	}
	var out []Consumer
	err := withCoordinator(ctx, paths, func(ctx context.Context, c *secretstore.ResolutionCoordinator, p secretstore.Provider) error {
		if err := p.Set(ctx, name, value); err != nil {
			return err
		}
		out = convertConsumers(c.Dependents(name))
		return nil
	})
	return out, err
}

func Remove(ctx context.Context, paths Paths, name string) ([]Consumer, error) {
	var out []Consumer
	err := withCoordinator(ctx, paths, func(ctx context.Context, c *secretstore.ResolutionCoordinator, p secretstore.Provider) error {
		out = convertConsumers(c.Dependents(name))
		return p.Delete(ctx, name)
	})
	return out, err
}

func Refresh(ctx context.Context, paths Paths, name string) ([]Consumer, error) {
	var out []Consumer
	err := withCoordinator(ctx, paths, func(ctx context.Context, c *secretstore.ResolutionCoordinator, _ secretstore.Provider) error {
		out = convertConsumers(c.RefreshConsumers(ctx, name))
		return nil
	})
	return out, err
}

func Retry(ctx context.Context, paths Paths) ([]SecretStatus, error) {
	var out []SecretStatus
	err := withCoordinator(ctx, paths, func(ctx context.Context, c *secretstore.ResolutionCoordinator, p secretstore.Provider) error {
		if native, ok := p.(secretstore.NativeProvider); ok {
			native.Retry()
		}
		out = convertStatuses(c.Status(ctx))
		return nil
	})
	return out, err
}

func withCoordinator(parent context.Context, paths Paths, op func(context.Context, *secretstore.ResolutionCoordinator, secretstore.Provider) error) error {
	ctx, cancel := bounded(parent)
	defer cancel()
	lease, err := stateowner.Acquire(paths.State, "offline secret operation")
	if err != nil {
		return err
	}
	defer lease.Close()
	cfg, err := config.Load(paths.Config)
	if err != nil {
		return err
	}
	if cfg.Secrets == nil {
		return errors.New("secret storage is not configured")
	}
	provider, err := openProvider(paths.State, cfg.Secrets)
	if err != nil {
		return err
	}
	coordinator := secretstore.NewResolutionCoordinator(cfg, provider, func(string) (string, bool) { return "", false }, secretstore.ResolutionTuning{}, nil)
	return op(ctx, coordinator, provider)
}

func bounded(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	if _, ok := parent.Deadline(); ok {
		return context.WithCancel(parent)
	}
	return context.WithTimeout(parent, defaultTimeout)
}

func openProvider(state string, declaration *config.Secrets) (secretstore.Provider, error) {
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

func convertConsumers(in []secretstore.ConsumerIdentity) []Consumer {
	out := make([]Consumer, len(in))
	for i, c := range in {
		out[i] = Consumer{Kind: string(c.Kind), Name: c.Name}
	}
	return out
}
func convertStatuses(in []secretstore.SecretStatus) []SecretStatus {
	out := make([]SecretStatus, len(in))
	for i, s := range in {
		out[i] = SecretStatus{Name: s.Name, Consumers: convertConsumers(s.Consumers), Source: string(s.Source), Condition: string(s.Condition)}
	}
	return out
}
