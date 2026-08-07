//go:build linux

package secretstore

import "context"

func platformNativeExecutor() (func(context.Context, HelperRequest, string) (Result, error), error) {
	return func(ctx context.Context, request HelperRequest, value string) (Result, error) {
		bus, err := newGodbusSecretServiceBus()
		if err != nil {
			return Result{}, linuxNativeError(request.Operation, request.Name, err)
		}
		defer bus.Close()
		return (&linuxAdapter{bus: bus}).Execute(ctx, request, value)
	}, nil
}

func NewNativeStore(stateDir, executable string, args ...string) (NativeProvider, error) {
	supervisor, err := NewPOSIXSupervisor(stateDir, executable, args...)
	if err != nil {
		return nil, err
	}
	return newLinuxStore(supervisor), nil
}
