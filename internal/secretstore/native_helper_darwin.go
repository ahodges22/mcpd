//go:build darwin

package secretstore

import (
	"context"
	"os"
)

func platformNativeExecutor() (func(context.Context, HelperRequest, string) (Result, error), error) {
	runner, err := newDarwinSecurityCommandRunner(os.Getenv(nativeMarkerDirEnv), "/usr/bin/security")
	if err != nil {
		return nil, err
	}
	adapter := &darwinAdapter{runner: runner}
	return adapter.Execute, nil
}

func NewNativeStore(stateDir, executable string, args ...string) (NativeProvider, error) {
	supervisor, err := NewPOSIXSupervisor(stateDir, executable, args...)
	if err != nil {
		return nil, err
	}
	return newDarwinStore(supervisor), nil
}
