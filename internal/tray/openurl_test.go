package tray

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestOpenURLArgumentBoundary(t *testing.T) {
	const target = "https://login.example/authorize?q=hello%20world;$(touch%20/tmp/nope)&next=1"
	for _, test := range []struct {
		name       string
		goos       string
		wantBinary string
	}{
		{name: "darwin", goos: "darwin", wantBinary: "/usr/bin/open"},
		{name: "linux", goos: "linux", wantBinary: "xdg-open"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var gotBinary string
			var gotArgs []string
			run := func(_ context.Context, binary string, args ...string) error {
				gotBinary = binary
				gotArgs = append([]string(nil), args...)
				return nil
			}
			if err := openURLWith(t.Context(), target, test.goos, run); err != nil {
				t.Fatalf("openURLWith: %v", err)
			}
			if gotBinary != test.wantBinary {
				t.Fatalf("binary = %q, want %q", gotBinary, test.wantBinary)
			}
			if want := []string{target}; !reflect.DeepEqual(gotArgs, want) {
				t.Fatalf("arguments = %#v, want %#v", gotArgs, want)
			}
		})
	}

	t.Run("unsupported platform", func(t *testing.T) {
		called := false
		err := openURLWith(t.Context(), target, "windows", func(context.Context, string, ...string) error {
			called = true
			return nil
		})
		if err == nil || called {
			t.Fatalf("error = %v, runner called = %v", err, called)
		}
	})

	t.Run("unsafe target", func(t *testing.T) {
		called := false
		err := openURLWith(t.Context(), "http://example.com/authorize", "linux", func(context.Context, string, ...string) error {
			called = true
			return nil
		})
		if err == nil || called {
			t.Fatalf("error = %v, runner called = %v", err, called)
		}
	})

	t.Run("cancelled context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		called := false
		err := openURLWith(ctx, target, "linux", func(context.Context, string, ...string) error {
			called = true
			return nil
		})
		if !errors.Is(err, context.Canceled) || called {
			t.Fatalf("error = %v, runner called = %v", err, called)
		}
	})
}
