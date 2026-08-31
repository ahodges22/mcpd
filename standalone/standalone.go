// Package standalone manages the existing standalone mcpd user service.
package standalone

import (
	"context"

	"github.com/ahodges22/mcpd/internal/service"
)

type State = service.State

func Inspect(ctx context.Context) (State, error) {
	if err := ctx.Err(); err != nil {
		return State{}, err
	}
	return service.Inspect()
}

// StopDisable returns the state that Restore accepts after stopping the service.
func StopDisable(ctx context.Context) (State, error) {
	prior, err := Inspect(ctx)
	if err != nil {
		return State{}, err
	}
	if !prior.Installed {
		return prior, nil
	}
	if err := ctx.Err(); err != nil {
		return State{}, err
	}
	if err := service.StopDisable(); err != nil {
		return prior, err
	}
	return prior, nil
}

func Restore(ctx context.Context, prior State) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return service.Restore(prior)
}
