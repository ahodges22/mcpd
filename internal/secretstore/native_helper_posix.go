//go:build darwin || linux

package secretstore

import (
	"context"
	"io"
	"os"
)

func nativeHelperInvocation(args []string, instance string) bool {
	if len(args) < 4 || instance == "" {
		return false
	}
	n := len(args)
	return args[n-3] == "--" &&
		args[n-2] == nativeHelperArg &&
		args[n-1] == instance
}

func ServeNativeHelperIfRequested(parent context.Context, args []string, in io.Reader, out io.Writer) (bool, error) {
	instance := os.Getenv(nativeHelperIDEnv)
	if !nativeHelperInvocation(args, instance) {
		return false, nil
	}
	execute, err := platformNativeExecutor()
	if err != nil {
		execute = func(context.Context, HelperRequest, string) (Result, error) {
			return Result{}, &Error{Provider: "native", Condition: ConditionUnavailable, Cause: err}
		}
	}
	return true, ServeHelperOnce(parent, in, out, execute)
}
