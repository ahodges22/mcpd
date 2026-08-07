package secretstore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	MaxValueBytes     = 2048
	nativeServiceName = "io.mcpd.secrets"
)

type Provider interface {
	Get(context.Context, string) (Result, error)
	Set(context.Context, string, string) error
	Delete(context.Context, string) error
}

type NativeProvider interface {
	Provider
	Retry()
}

type nativeOperationRunner interface {
	Run(context.Context, HelperRequest, string) (Result, error)
}

type Result struct {
	Value   string
	Present bool
}

type Operation string

const (
	OperationGet      Operation = "get"
	OperationSet      Operation = "set"
	OperationDelete   Operation = "delete"
	OperationHealth   Operation = "health"
	OperationValidate Operation = "validate"
)

type Condition string

const (
	ConditionNotFound      Condition = "not_found"
	ConditionUnavailable   Condition = "unavailable"
	ConditionLocked        Condition = "locked"
	ConditionDenied        Condition = "denied"
	ConditionBusy          Condition = "busy"
	ConditionTimedOut      Condition = "timed_out"
	ConditionWedged        Condition = "wedged"
	ConditionCorrupt       Condition = "corrupt"
	ConditionPermission    Condition = "permission_validation"
	ConditionLockContended Condition = "lock_contended"
	ConditionInteraction   Condition = "interaction_required"
	ConditionValueTooLarge Condition = "value_too_large"
	ConditionInvalidValue  Condition = "invalid_value"
	ConditionIndeterminate Condition = "indeterminate_mutation"
	ConditionRetryable     Condition = "retryable"
	ConditionUnexpected    Condition = "unexpected"
)

type Error struct {
	Operation Operation
	Provider  string
	Name      string
	Condition Condition
	Limit     int
	Cause     error
}

// Error omits Cause because provider diagnostics can contain credential material.
func (e *Error) Error() string {
	parts := make([]string, 0, 4)
	if e.Provider != "" {
		parts = append(parts, "provider "+e.Provider)
	}
	if e.Operation != "" {
		parts = append(parts, string(e.Operation))
	}
	if e.Name != "" {
		parts = append(parts, "name "+fmt.Sprintf("%q", e.Name))
	}
	condition := e.Condition
	if condition == "" {
		condition = ConditionUnexpected
	}
	message := string(condition)
	if prefix := strings.Join(parts, " "); prefix != "" {
		message = prefix + ": " + message
	}
	if e.Limit > 0 {
		message += fmt.Sprintf(" (limit %d bytes)", e.Limit)
	}
	return message
}

func (e *Error) Unwrap() error { return e.Cause }

func ConditionOf(err error) (Condition, bool) {
	var providerErr *Error
	if !errors.As(err, &providerErr) {
		return "", false
	}
	condition := providerErr.Condition
	if condition == "" {
		condition = ConditionUnexpected
	}
	return condition, true
}

func IsProviderHealthError(err error) bool {
	condition, ok := ConditionOf(err)
	if !ok {
		return false
	}
	switch condition {
	case ConditionUnavailable,
		ConditionLocked,
		ConditionDenied,
		ConditionTimedOut,
		ConditionWedged,
		ConditionCorrupt,
		ConditionPermission,
		ConditionInteraction,
		ConditionUnexpected:
		return true
	default:
		return false
	}
}

func ValidateValue(value string) error {
	if value == "" {
		return invalidValue("value is empty")
	}
	if len(value) > MaxValueBytes {
		return &Error{
			Operation: OperationSet,
			Condition: ConditionInvalidValue,
			Limit:     MaxValueBytes,
			Cause:     fmt.Errorf("value exceeds portable limit"),
		}
	}
	if !utf8.ValidString(value) {
		return invalidValue("value is not valid UTF-8")
	}
	for _, r := range value {
		if !unicode.IsPrint(r) {
			return invalidValue("value contains a non-printing character")
		}
	}
	return nil
}

func invalidValue(reason string) error {
	return &Error{
		Operation: OperationSet,
		Condition: ConditionInvalidValue,
		Cause:     errors.New(reason),
	}
}
