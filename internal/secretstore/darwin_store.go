//go:build darwin

package secretstore

import (
	"context"
	"fmt"
	"sync"
)

type nativeOperationRunner interface {
	Run(context.Context, HelperRequest, string) (Result, error)
}

type darwinStore struct {
	runner nativeOperationRunner
	gate   chan struct{}

	mu                 sync.Mutex
	interactionLatched bool
	retryPermit        bool
}

func newDarwinStore(runner nativeOperationRunner) *darwinStore {
	store := &darwinStore{
		runner: runner,
		gate:   make(chan struct{}, 1),
	}
	store.gate <- struct{}{}
	return store
}

func (s *darwinStore) Get(ctx context.Context, name string) (Result, error) {
	return s.run(ctx, OperationGet, name, "")
}

func (s *darwinStore) Set(ctx context.Context, name, value string) error {
	if err := ValidateValue(value); err != nil {
		return err
	}
	_, err := s.run(ctx, OperationSet, name, value)
	return err
}

func (s *darwinStore) Delete(ctx context.Context, name string) error {
	_, err := s.run(ctx, OperationDelete, name, "")
	return err
}

func (s *darwinStore) Retry() {
	s.mu.Lock()
	if s.interactionLatched {
		s.retryPermit = true
	}
	s.mu.Unlock()
}

func (s *darwinStore) run(ctx context.Context, operation Operation, name, value string) (Result, error) {
	select {
	case <-ctx.Done():
		return Result{}, &Error{Operation: operation, Provider: "native", Name: name, Condition: ConditionTimedOut, Cause: ctx.Err()}
	case <-s.gate:
	}
	defer func() { s.gate <- struct{}{} }()

	s.mu.Lock()
	if s.interactionLatched && !s.retryPermit {
		s.mu.Unlock()
		return Result{}, &Error{Operation: operation, Provider: "native", Name: name, Condition: ConditionInteraction}
	}
	s.retryPermit = false
	s.mu.Unlock()

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
	result, err := s.runner.Run(ctx, HelperRequest{Operation: operation, Name: name, Deadline: deadline}, value)
	if condition, _ := ConditionOf(err); condition == ConditionTimedOut && operation == OperationGet {
		err = &Error{
			Operation: operation,
			Provider:  "native",
			Name:      name,
			Condition: ConditionInteraction,
			Cause:     err,
		}
	}

	s.mu.Lock()
	if condition, _ := ConditionOf(err); condition == ConditionInteraction || condition == ConditionTimedOut {
		s.interactionLatched = true
	} else if err == nil {
		s.interactionLatched = false
	}
	s.mu.Unlock()
	return result, err
}
