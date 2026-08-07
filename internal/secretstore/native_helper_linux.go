//go:build linux

package secretstore

import "context"

func platformNativeExecutor() (func(context.Context, HelperRequest, string) (Result, error), error) {
	return func(_ context.Context, request HelperRequest, _ string) (Result, error) {
		return Result{}, &Error{
			Operation: request.Operation,
			Provider:  "native",
			Name:      request.Name,
			Condition: ConditionUnavailable,
		}
	}, nil
}
