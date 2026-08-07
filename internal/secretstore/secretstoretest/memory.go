package secretstoretest

import (
	"context"
	"errors"
	"sync"

	"github.com/ahodges22/mcpd/internal/secretstore"
)

type Memory struct {
	mu     sync.Mutex
	values map[string]string
}

func NewMemory() *Memory {
	return &Memory{values: map[string]string{}}
}

func (m *Memory) Get(ctx context.Context, name string) (secretstore.Result, error) {
	if err := ctx.Err(); err != nil {
		return secretstore.Result{}, contextError(secretstore.OperationGet, name, err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	value, ok := m.values[name]
	return secretstore.Result{Value: value, Present: ok}, nil
}

func (m *Memory) Set(ctx context.Context, name, value string) error {
	if err := ctx.Err(); err != nil {
		return contextError(secretstore.OperationSet, name, err)
	}
	if err := secretstore.ValidateValue(value); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.values[name] = value
	return nil
}

func (m *Memory) Delete(ctx context.Context, name string) error {
	if err := ctx.Err(); err != nil {
		return contextError(secretstore.OperationDelete, name, err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.values, name)
	return nil
}

func contextError(operation secretstore.Operation, name string, cause error) error {
	condition := secretstore.ConditionUnexpected
	if errors.Is(cause, context.DeadlineExceeded) {
		condition = secretstore.ConditionTimedOut
	}
	return &secretstore.Error{
		Operation: operation,
		Provider:  "memory",
		Name:      name,
		Condition: condition,
		Cause:     cause,
	}
}

var _ secretstore.Provider = (*Memory)(nil)
