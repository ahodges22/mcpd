//go:build linux

package secretstore

import (
	"context"
	"fmt"
)

type linuxStore struct {
	runner nativeOperationRunner
}

func newLinuxStore(runner nativeOperationRunner) *linuxStore {
	return &linuxStore{runner: runner}
}

func (s *linuxStore) Get(ctx context.Context, name string) (Result, error) {
	return s.run(ctx, OperationGet, name, "")
}

func (s *linuxStore) Set(ctx context.Context, name, value string) error {
	if err := ValidateValue(value); err != nil {
		return err
	}
	_, err := s.run(ctx, OperationSet, name, value)
	return err
}

func (s *linuxStore) Delete(ctx context.Context, name string) error {
	_, err := s.run(ctx, OperationDelete, name, "")
	return err
}

func (s *linuxStore) Retry() {}

func (s *linuxStore) run(ctx context.Context, operation Operation, name, value string) (Result, error) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return Result{}, &Error{
			Operation: operation,
			Provider:  "native",
			Name:      name,
			Condition: ConditionTimedOut,
			Cause:     fmt.Errorf("native operation deadline is required"),
		}
	}
	return s.runner.Run(ctx, HelperRequest{Operation: operation, Name: name, Deadline: deadline}, value)
}
