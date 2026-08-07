package secretstore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"time"
)

const helperProtocolVersion = 1
const helperFrameLimit = 6*MaxValueBytes + 4096

var helperName = regexp.MustCompile(`^[A-Za-z0-9_]+$`)

type HelperRequest struct {
	Operation Operation `json:"operation"`
	Name      string    `json:"name,omitempty"`
	Deadline  time.Time `json:"deadline"`
}

func (r HelperRequest) String() string {
	return fmt.Sprintf("helper request operation=%s name=%q deadline=%s", r.Operation, r.Name, r.Deadline.Format(time.RFC3339Nano))
}

type HelperResponse struct {
	Result    Result    `json:"result,omitempty"`
	Operation Operation `json:"operation,omitempty"`
	Provider  string    `json:"provider,omitempty"`
	Name      string    `json:"name,omitempty"`
	Condition Condition `json:"condition,omitempty"`
	Limit     int       `json:"limit,omitempty"`
}

type helperRequestFrame struct {
	Version  int           `json:"version"`
	Request  HelperRequest `json:"request"`
	SetValue string        `json:"set_value,omitempty"`
}

type helperResponseFrame struct {
	Version  int            `json:"version"`
	Response HelperResponse `json:"response"`
}

func WriteHelperRequest(w io.Writer, request HelperRequest, setValue string) error {
	if err := validateHelperRequest(request, setValue); err != nil {
		return err
	}
	return json.NewEncoder(w).Encode(helperRequestFrame{
		Version:  helperProtocolVersion,
		Request:  request,
		SetValue: setValue,
	})
}

func ReadHelperRequest(r io.Reader) (HelperRequest, string, error) {
	var frame helperRequestFrame
	if err := decodeHelperFrame(r, &frame); err != nil {
		return HelperRequest{}, "", fmt.Errorf("decode helper request: %w", err)
	}
	if frame.Version != helperProtocolVersion {
		return HelperRequest{}, "", fmt.Errorf("unsupported helper protocol version %d", frame.Version)
	}
	if err := validateHelperRequest(frame.Request, frame.SetValue); err != nil {
		return HelperRequest{}, "", err
	}
	return frame.Request, frame.SetValue, nil
}

func WriteHelperResponse(w io.Writer, response HelperResponse) error {
	if response.Condition != "" && (response.Result.Present || response.Result.Value != "") {
		return fmt.Errorf("helper error response contains a result")
	}
	return json.NewEncoder(w).Encode(helperResponseFrame{
		Version:  helperProtocolVersion,
		Response: response,
	})
}

func ReadHelperResponse(r io.Reader) (HelperResponse, error) {
	var frame helperResponseFrame
	if err := decodeHelperFrame(r, &frame); err != nil {
		return HelperResponse{}, fmt.Errorf("decode helper response: %w", err)
	}
	if frame.Version != helperProtocolVersion {
		return HelperResponse{}, fmt.Errorf("unsupported helper protocol version %d", frame.Version)
	}
	if frame.Response.Condition != "" && (frame.Response.Result.Present || frame.Response.Result.Value != "") {
		return HelperResponse{}, fmt.Errorf("helper error response contains a result")
	}
	return frame.Response, nil
}

func HelperResponseFromError(err error) HelperResponse {
	if err == nil {
		return HelperResponse{}
	}
	var providerErr *Error
	if !errors.As(err, &providerErr) {
		return HelperResponse{Condition: ConditionUnexpected}
	}
	condition := providerErr.Condition
	if condition == "" {
		condition = ConditionUnexpected
	}
	return HelperResponse{
		Operation: providerErr.Operation,
		Provider:  providerErr.Provider,
		Name:      providerErr.Name,
		Condition: condition,
		Limit:     providerErr.Limit,
	}
}

func (r HelperResponse) Err() error {
	if r.Condition == "" {
		return nil
	}
	return &Error{
		Operation: r.Operation,
		Provider:  r.Provider,
		Name:      r.Name,
		Condition: r.Condition,
		Limit:     r.Limit,
	}
}

func ServeHelperOnce(parent context.Context, in io.Reader, out io.Writer, execute func(context.Context, HelperRequest, string) (Result, error)) error {
	request, setValue, err := ReadHelperRequest(in)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithDeadline(parent, request.Deadline)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return WriteHelperResponse(out, HelperResponseFromError(&Error{
			Operation: request.Operation,
			Provider:  "native",
			Name:      request.Name,
			Condition: ConditionTimedOut,
			Cause:     err,
		}))
	}
	result, operationErr := execute(ctx, request, setValue)
	if request.Operation != OperationGet {
		result = Result{}
	}
	response := HelperResponse{Result: result}
	if operationErr != nil {
		response = HelperResponseFromError(operationErr)
	}
	return WriteHelperResponse(out, response)
}

func validateHelperRequest(request HelperRequest, setValue string) error {
	if request.Deadline.IsZero() {
		return fmt.Errorf("helper request deadline is required")
	}
	switch request.Operation {
	case OperationGet, OperationDelete:
		if !helperName.MatchString(request.Name) {
			return fmt.Errorf("helper request name is invalid")
		}
		if setValue != "" {
			return fmt.Errorf("helper %s request contains a set value", request.Operation)
		}
	case OperationSet:
		if !helperName.MatchString(request.Name) {
			return fmt.Errorf("helper request name is invalid")
		}
		if err := ValidateValue(setValue); err != nil {
			return err
		}
	case OperationHealth:
		if request.Name != "" || setValue != "" {
			return fmt.Errorf("helper health request contains operation data")
		}
	default:
		return fmt.Errorf("unsupported helper operation %q", request.Operation)
	}
	return nil
}

func decodeHelperFrame(r io.Reader, dst any) error {
	limited := &io.LimitedReader{R: r, N: helperFrameLimit + 1}
	decoder := json.NewDecoder(limited)
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	buffered, err := io.ReadAll(decoder.Buffered())
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(buffered)) != 0 {
		return fmt.Errorf("helper stream contains trailing data")
	}
	if limited.N <= 0 {
		return fmt.Errorf("helper frame exceeds %d bytes", helperFrameLimit)
	}
	return nil
}
