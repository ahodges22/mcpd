package secretstore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func TestHelperProtocolRoundTrip(t *testing.T) {
	deadline := time.Now().Add(time.Minute).Round(time.Millisecond)
	request := HelperRequest{
		Operation: OperationGet,
		Name:      "API_TOKEN",
		Deadline:  deadline,
	}
	var requestPipe bytes.Buffer
	if err := WriteHelperRequest(&requestPipe, request, ""); err != nil {
		t.Fatalf("WriteHelperRequest: %v", err)
	}
	gotRequest, gotValue, err := ReadHelperRequest(&requestPipe)
	if err != nil {
		t.Fatalf("ReadHelperRequest: %v", err)
	}
	if gotRequest.Operation != request.Operation || gotRequest.Name != request.Name || !gotRequest.Deadline.Equal(request.Deadline) || gotValue != "" {
		t.Fatalf("request = %#v, %q; want %#v, empty value", gotRequest, gotValue, request)
	}

	response := HelperResponse{Result: Result{Value: "secret", Present: true}}
	var responsePipe bytes.Buffer
	if err := WriteHelperResponse(&responsePipe, response); err != nil {
		t.Fatalf("WriteHelperResponse: %v", err)
	}
	gotResponse, err := ReadHelperResponse(&responsePipe)
	if err != nil {
		t.Fatalf("ReadHelperResponse: %v", err)
	}
	if gotResponse != response {
		t.Fatalf("response = %#v, want %#v", gotResponse, response)
	}
}

func TestHelperResponseRejectsDelayedTrailingData(t *testing.T) {
	reader, writer := io.Pipe()
	go func() {
		_ = WriteHelperResponse(writer, HelperResponse{Result: Result{Value: "value", Present: true}})
		time.Sleep(20 * time.Millisecond)
		_, _ = io.WriteString(writer, "trailing")
		_ = writer.Close()
	}()
	defer reader.Close()
	if _, err := ReadHelperResponse(reader); err == nil {
		t.Fatal("ReadHelperResponse accepted delayed trailing data")
	}
}

func TestSetValueUsesPipeOnly(t *testing.T) {
	const secret = "do-not-place-in-process-metadata"
	request := HelperRequest{
		Operation: OperationSet,
		Name:      "API_TOKEN",
		Deadline:  time.Now().Add(time.Minute).Round(time.Millisecond),
	}
	if strings.Contains(request.String(), secret) {
		t.Fatal("request metadata contains the set value")
	}

	var pipe bytes.Buffer
	if err := WriteHelperRequest(&pipe, request, secret); err != nil {
		t.Fatalf("WriteHelperRequest: %v", err)
	}
	gotRequest, gotValue, err := ReadHelperRequest(&pipe)
	if err != nil {
		t.Fatalf("ReadHelperRequest: %v", err)
	}
	if gotRequest.Operation != request.Operation || gotRequest.Name != request.Name || !gotRequest.Deadline.Equal(request.Deadline) || gotValue != secret {
		t.Fatalf("decoded request = %#v, %q", gotRequest, gotValue)
	}
}

func TestHelperProtocolAcceptsMaximallyEscapedPortableValue(t *testing.T) {
	value := strings.Repeat("<", MaxValueBytes)
	request := HelperRequest{Operation: OperationSet, Name: "TOKEN", Deadline: time.Now().Add(time.Minute)}
	var pipe bytes.Buffer
	if err := WriteHelperRequest(&pipe, request, value); err != nil {
		t.Fatalf("WriteHelperRequest: %v", err)
	}
	_, got, err := ReadHelperRequest(&pipe)
	if err != nil {
		t.Fatalf("ReadHelperRequest: %v", err)
	}
	if got != value {
		t.Fatal("maximally escaped portable value did not round-trip")
	}
}

func TestHelperErrorResponseIsStructuredAndRedacted(t *testing.T) {
	const secret = "cause-must-not-cross-helper-response"
	providerErr := &Error{
		Operation: OperationSet,
		Provider:  "native",
		Name:      "API_TOKEN",
		Condition: ConditionDenied,
		Cause:     errors.New("provider rejected " + secret),
	}
	response := HelperResponseFromError(providerErr)
	var pipe bytes.Buffer
	if err := WriteHelperResponse(&pipe, response); err != nil {
		t.Fatalf("WriteHelperResponse: %v", err)
	}
	if strings.Contains(pipe.String(), secret) {
		t.Fatal("helper response disclosed provider cause text")
	}

	decoded, err := ReadHelperResponse(&pipe)
	if err != nil {
		t.Fatalf("ReadHelperResponse: %v", err)
	}
	err = decoded.Err()
	assertCondition(t, err, ConditionDenied)
	if strings.Contains(err.Error(), secret) {
		t.Fatal("decoded error disclosed provider cause text")
	}
}

func TestServeHelperOnceExecutesOneRequest(t *testing.T) {
	request := HelperRequest{
		Operation: OperationHealth,
		Deadline:  time.Now().Add(time.Minute).Round(time.Millisecond),
	}
	var input bytes.Buffer
	if err := WriteHelperRequest(&input, request, ""); err != nil {
		t.Fatalf("WriteHelperRequest: %v", err)
	}
	var output bytes.Buffer
	calls := 0
	err := ServeHelperOnce(context.Background(), &input, &output, func(ctx context.Context, got HelperRequest, value string) (Result, error) {
		calls++
		if got.Operation != request.Operation || got.Name != request.Name || !got.Deadline.Equal(request.Deadline) || value != "" {
			t.Fatalf("handler request = %#v, %q", got, value)
		}
		if deadline, ok := ctx.Deadline(); !ok || !deadline.Equal(request.Deadline) {
			t.Fatalf("handler deadline = %v, %v; want %v", deadline, ok, request.Deadline)
		}
		return Result{}, nil
	})
	if err != nil {
		t.Fatalf("ServeHelperOnce: %v", err)
	}
	if calls != 1 {
		t.Fatalf("handler calls = %d, want 1", calls)
	}
	response, err := ReadHelperResponse(&output)
	if err != nil {
		t.Fatalf("ReadHelperResponse: %v", err)
	}
	if err := response.Err(); err != nil {
		t.Fatalf("response error: %v", err)
	}
}

func TestReadHelperRequestReturnsBeforePipeEOF(t *testing.T) {
	reader, writer := io.Pipe()
	t.Cleanup(func() {
		_ = reader.Close()
		_ = writer.Close()
	})
	request := HelperRequest{Operation: OperationGet, Name: "TOKEN", Deadline: time.Now().Add(time.Minute)}
	writeErr := make(chan error, 1)
	go func() {
		writeErr <- WriteHelperRequest(writer, request, "")
	}()
	type result struct {
		request HelperRequest
		err     error
	}
	readResult := make(chan result, 1)
	go func() {
		got, _, err := ReadHelperRequest(reader)
		readResult <- result{request: got, err: err}
	}()

	select {
	case err := <-writeErr:
		if err != nil {
			t.Fatalf("WriteHelperRequest: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("WriteHelperRequest blocked")
	}
	select {
	case got := <-readResult:
		if got.err != nil {
			t.Fatalf("ReadHelperRequest: %v", got.err)
		}
		if got.request.Operation != request.Operation || got.request.Name != request.Name || !got.request.Deadline.Equal(request.Deadline) {
			t.Fatalf("request = %#v, want %#v", got.request, request)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("ReadHelperRequest waited for pipe EOF after a complete frame")
	}
}

func TestServeHelperOnceDoesNotExecuteExpiredRequest(t *testing.T) {
	request := HelperRequest{Operation: OperationGet, Name: "TOKEN", Deadline: time.Now().Add(-time.Second)}
	var input bytes.Buffer
	if err := WriteHelperRequest(&input, request, ""); err != nil {
		t.Fatalf("WriteHelperRequest: %v", err)
	}
	var output bytes.Buffer
	calls := 0
	if err := ServeHelperOnce(context.Background(), &input, &output, func(context.Context, HelperRequest, string) (Result, error) {
		calls++
		return Result{}, nil
	}); err != nil {
		t.Fatalf("ServeHelperOnce: %v", err)
	}
	if calls != 0 {
		t.Fatalf("expired request executed %d times", calls)
	}
	response, err := ReadHelperResponse(&output)
	if err != nil {
		t.Fatalf("ReadHelperResponse: %v", err)
	}
	assertCondition(t, response.Err(), ConditionTimedOut)
}

func TestServeHelperOnceNeverReturnsSetValue(t *testing.T) {
	const secret = "set-value-must-not-cross-response-pipe"
	request := HelperRequest{Operation: OperationSet, Name: "TOKEN", Deadline: time.Now().Add(time.Minute)}
	var input bytes.Buffer
	if err := WriteHelperRequest(&input, request, secret); err != nil {
		t.Fatalf("WriteHelperRequest: %v", err)
	}
	var output bytes.Buffer
	if err := ServeHelperOnce(context.Background(), &input, &output, func(context.Context, HelperRequest, string) (Result, error) {
		return Result{Value: secret, Present: true}, nil
	}); err != nil {
		t.Fatalf("ServeHelperOnce: %v", err)
	}
	if strings.Contains(output.String(), secret) {
		t.Fatal("set response disclosed the set value")
	}
}

func TestNativeHelperInvocationRequiresMatchingInstance(t *testing.T) {
	const instance = "0123456789abcdef0123456789abcdef"
	tests := []struct {
		name string
		args []string
		env  string
		want bool
	}{
		{name: "exact", args: []string{"mcpd", "--", nativeHelperArg, instance}, env: instance, want: true},
		{name: "ordinary command", args: []string{"mcpd", "install"}, env: instance},
		{name: "missing separator", args: []string{"mcpd", nativeHelperArg, instance}, env: instance},
		{name: "mismatched environment", args: []string{"mcpd", "--", nativeHelperArg, instance}, env: "different"},
		{name: "extra argument", args: []string{"mcpd", "--", nativeHelperArg, instance, "extra"}, env: instance},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := nativeHelperInvocation(test.args, test.env); got != test.want {
				t.Fatalf("nativeHelperInvocation = %v, want %v", got, test.want)
			}
		})
	}
}
