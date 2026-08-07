//go:build linux

package secretstore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	dbus "github.com/godbus/dbus/v5"
)

func TestLinuxHealthMapping(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want Condition
	}{
		{name: "no session bus", err: errLinuxNoSessionBus, want: ConditionUnavailable},
		{name: "no service owner", err: errLinuxNoServiceOwner, want: ConditionUnavailable},
		{name: "no default collection", err: errLinuxNoCollection, want: ConditionUnavailable},
		{name: "locked collection", err: errLinuxLocked, want: ConditionLocked},
		{name: "access denied value", err: dbus.Error{Name: "org.freedesktop.DBus.Error.AccessDenied"}, want: ConditionDenied},
		{name: "access denied pointer", err: &dbus.Error{Name: "org.freedesktop.DBus.Error.AccessDenied"}, want: ConditionDenied},
		{name: "secret service locked", err: dbus.Error{Name: "org.freedesktop.Secret.Error.IsLocked"}, want: ConditionLocked},
		{name: "deadline", err: context.DeadlineExceeded, want: ConditionTimedOut},
		{name: "unknown", err: errors.New("unknown"), want: ConditionUnexpected},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := linuxNativeError(OperationHealth, "", test.err)
			if condition, ok := ConditionOf(err); !ok || condition != test.want {
				t.Fatalf("condition = %q, %v; want %q; error: %v", condition, ok, test.want, err)
			}
		})
	}
}

type fakeLinuxSecretServiceBus struct {
	hasOwner       bool
	ownerErr       error
	collection     dbus.ObjectPath
	collectionErr  error
	locked         bool
	lockedErr      error
	nameOwnerCalls int
	aliasCalls     int
	lockedCalls    int
	lockedIfaces   []string
	openCalls      int
	searchCalls    int
	searchItems    []dbus.ObjectPath
	getSecret      linuxSecret
	createCalls    int
	createReplace  bool
	createProps    map[string]dbus.Variant
	createSecret   linuxSecret
	createItem     dbus.ObjectPath
	createPrompt   dbus.ObjectPath
	deleteCalls    int
	deletePrompt   dbus.ObjectPath
}

func (b *fakeLinuxSecretServiceBus) NameHasOwner(context.Context) (bool, error) {
	b.nameOwnerCalls++
	return b.hasOwner, b.ownerErr
}

func (b *fakeLinuxSecretServiceBus) ReadDefaultCollection(context.Context) (dbus.ObjectPath, error) {
	b.aliasCalls++
	return b.collection, b.collectionErr
}

func (b *fakeLinuxSecretServiceBus) IsLocked(_ context.Context, _ dbus.ObjectPath, iface string) (bool, error) {
	b.lockedCalls++
	b.lockedIfaces = append(b.lockedIfaces, iface)
	return b.locked, b.lockedErr
}

func (b *fakeLinuxSecretServiceBus) OpenSession(context.Context) (dbus.ObjectPath, error) {
	b.openCalls++
	return "/session/test", nil
}

func (b *fakeLinuxSecretServiceBus) CloseSession(context.Context, dbus.ObjectPath) error {
	return nil
}

func (b *fakeLinuxSecretServiceBus) SearchItems(context.Context, dbus.ObjectPath, map[string]string) ([]dbus.ObjectPath, error) {
	b.searchCalls++
	return b.searchItems, nil
}

func (b *fakeLinuxSecretServiceBus) GetSecret(context.Context, dbus.ObjectPath, dbus.ObjectPath) (linuxSecret, error) {
	return b.getSecret, nil
}

func (b *fakeLinuxSecretServiceBus) CreateItem(_ context.Context, _ dbus.ObjectPath, properties map[string]dbus.Variant, secret linuxSecret, replace bool) (dbus.ObjectPath, dbus.ObjectPath, error) {
	b.createCalls++
	b.createReplace = replace
	b.createProps = properties
	b.createSecret = secret
	item := b.createItem
	if item == "" {
		item = "/item/test"
	}
	prompt := b.createPrompt
	if prompt == "" {
		prompt = "/"
	}
	return item, prompt, nil
}

func (b *fakeLinuxSecretServiceBus) DeleteItem(context.Context, dbus.ObjectPath) (dbus.ObjectPath, error) {
	b.deleteCalls++
	if b.deletePrompt == "" {
		return "/", nil
	}
	return b.deletePrompt, nil
}

func (b *fakeLinuxSecretServiceBus) Close() error {
	return nil
}

func TestLinuxCreateItemReplacesAtomically(t *testing.T) {
	bus := &fakeLinuxSecretServiceBus{
		hasOwner:   true,
		collection: "/collection/default",
	}
	adapter := linuxAdapter{bus: bus}
	_, err := adapter.Execute(context.Background(), HelperRequest{Operation: OperationSet, Name: "API_TOKEN"}, "exact value")
	if err != nil {
		t.Fatalf("Execute set: %v", err)
	}
	if bus.createCalls != 1 || !bus.createReplace {
		t.Fatalf("CreateItem calls = %d, replace = %v; want one atomic replacement", bus.createCalls, bus.createReplace)
	}
	if bus.deleteCalls != 0 {
		t.Fatalf("Delete calls before replacement = %d", bus.deleteCalls)
	}
	if string(bus.createSecret.Value) != "exact value" {
		t.Fatal("CreateItem did not receive the exact value")
	}
	attributes, ok := bus.createProps[linuxItemInterface+".Attributes"].Value().(map[string]string)
	if !ok || attributes["service"] != nativeServiceName || attributes["username"] != "API_TOKEN" || len(attributes) != 2 {
		t.Fatalf("attributes = %#v", attributes)
	}
}

func TestLinuxHealthChecksSessionServiceCollectionAndLockState(t *testing.T) {
	tests := []struct {
		name       string
		bus        fakeLinuxSecretServiceBus
		want       Condition
		aliasCalls int
		lockCalls  int
	}{
		{
			name: "service absent",
			bus:  fakeLinuxSecretServiceBus{},
			want: ConditionUnavailable,
		},
		{
			name:       "default collection absent",
			bus:        fakeLinuxSecretServiceBus{hasOwner: true},
			want:       ConditionUnavailable,
			aliasCalls: 1,
		},
		{
			name:       "default collection locked",
			bus:        fakeLinuxSecretServiceBus{hasOwner: true, collection: "/collection/default", locked: true},
			want:       ConditionLocked,
			aliasCalls: 1,
			lockCalls:  1,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			bus := test.bus
			_, err := (&linuxAdapter{bus: &bus}).Execute(context.Background(), HelperRequest{Operation: OperationHealth}, "")
			if condition, ok := ConditionOf(err); !ok || condition != test.want {
				t.Fatalf("condition = %q, %v; want %q; error: %v", condition, ok, test.want, err)
			}
			if bus.nameOwnerCalls != 1 || bus.aliasCalls != test.aliasCalls || bus.lockedCalls != test.lockCalls {
				t.Fatalf("health calls = owner:%d alias:%d locked:%d", bus.nameOwnerCalls, bus.aliasCalls, bus.lockedCalls)
			}
			if bus.openCalls != 0 || bus.searchCalls != 0 || bus.createCalls != 0 || bus.deleteCalls != 0 {
				t.Fatal("health failure did not stop before item operations")
			}
		})
	}
}

func TestLinuxGetPreservesExactValueAndCleanMiss(t *testing.T) {
	for _, test := range []struct {
		name  string
		items []dbus.ObjectPath
		value string
		want  Result
	}{
		{name: "miss", want: Result{}},
		{name: "present", items: []dbus.ObjectPath{"/item/test"}, value: "  exact café 雪  ", want: Result{Value: "  exact café 雪  ", Present: true}},
	} {
		t.Run(test.name, func(t *testing.T) {
			bus := &fakeLinuxSecretServiceBus{
				hasOwner:    true,
				collection:  "/collection/default",
				searchItems: test.items,
				getSecret:   linuxSecret{Value: []byte(test.value)},
			}
			got, err := (&linuxAdapter{bus: bus}).Execute(context.Background(), HelperRequest{Operation: OperationGet, Name: "API_TOKEN"}, "")
			if err != nil {
				t.Fatalf("Execute get: %v", err)
			}
			if got != test.want {
				t.Fatalf("result = %#v; want %#v", got, test.want)
			}
			if test.name == "miss" && bus.lockedCalls != 1 {
				t.Fatalf("miss performed item work; locked calls = %d", bus.lockedCalls)
			}
			if test.name == "present" {
				want := []string{linuxCollectionInterface, linuxItemInterface}
				if len(bus.lockedIfaces) != len(want) || bus.lockedIfaces[0] != want[0] || bus.lockedIfaces[1] != want[1] {
					t.Fatalf("locked interfaces = %#v; want %#v", bus.lockedIfaces, want)
				}
			}
		})
	}
}

func TestLinuxPromptIsInteractionRequired(t *testing.T) {
	for _, operation := range []Operation{OperationSet, OperationDelete} {
		t.Run(string(operation), func(t *testing.T) {
			bus := &fakeLinuxSecretServiceBus{
				hasOwner:     true,
				collection:   "/collection/default",
				searchItems:  []dbus.ObjectPath{"/item/test"},
				createPrompt: "/prompt/test",
				deletePrompt: "/prompt/test",
			}
			_, err := (&linuxAdapter{bus: bus}).Execute(context.Background(), HelperRequest{Operation: operation, Name: "API_TOKEN"}, "value")
			if condition, ok := ConditionOf(err); !ok || condition != ConditionInteraction {
				t.Fatalf("condition = %q, %v; want %q; error: %v", condition, ok, ConditionInteraction, err)
			}
		})
	}
}

func TestLinuxStoreRequiresDeadline(t *testing.T) {
	var _ NativeProvider = newLinuxStore(nil)
	_, err := newLinuxStore(nil).Get(context.Background(), "API_TOKEN")
	if condition, ok := ConditionOf(err); !ok || condition != ConditionTimedOut {
		t.Fatalf("condition = %q, %v; want %q; error: %v", condition, ok, ConditionTimedOut, err)
	}
}

func TestLinuxNativeRoundTrip(t *testing.T) {
	if os.Getenv("MCPD_TEST_LINUX_NATIVE") != "1" {
		t.Skip("set MCPD_TEST_LINUX_NATIVE=1 to allow a temporary Secret Service item")
	}
	store, err := NewNativeStore(filepath.Join(stateSandbox(t), "state"), os.Args[0], "-test.run=^TestLinuxRealNativeHelper$")
	if err != nil {
		t.Fatalf("NewNativeStore: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	const name = "MCPD_TEST_LINUX"
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		_ = store.Delete(cleanupCtx, name)
	})
	const value = "  quotes ' \" slash \\ dollar $ tick \x60 café 雪  "
	if err := store.Set(ctx, name, value); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := store.Get(ctx, name)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !got.Present || got.Value != value {
		t.Fatalf("Get = %#v, want %q", got, value)
	}
}

func TestLinuxRealNativeHelper(t *testing.T) {
	if os.Getenv(nativeHelperIDEnv) == "" {
		return
	}
	handled, err := ServeNativeHelperIfRequested(context.Background(), os.Args, os.Stdin, os.Stdout)
	if !handled || err != nil {
		os.Exit(2)
	}
	os.Exit(0)
}
