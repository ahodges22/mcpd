//go:build linux

package secretstore

import (
	"context"
	"errors"
	"fmt"

	dbus "github.com/godbus/dbus/v5"
)

const (
	linuxSecretServiceName      = "org.freedesktop.secrets"
	linuxSecretServicePath      = dbus.ObjectPath("/org/freedesktop/secrets")
	linuxSecretServiceInterface = "org.freedesktop.Secret.Service"
	linuxCollectionInterface    = "org.freedesktop.Secret.Collection"
	linuxItemInterface          = "org.freedesktop.Secret.Item"
	linuxSessionInterface       = "org.freedesktop.Secret.Session"
	linuxPropertiesInterface    = "org.freedesktop.DBus.Properties"
)

var (
	errLinuxNoSessionBus   = errors.New("session D-Bus is unavailable")
	errLinuxNoServiceOwner = errors.New("Secret Service has no bus owner")
	errLinuxNoCollection   = errors.New("default Secret Service collection is unavailable")
	errLinuxLocked         = errors.New("Secret Service collection is locked")
	errLinuxPrompt         = errors.New("Secret Service operation requires interaction")
)

type linuxSecret struct {
	Session     dbus.ObjectPath
	Parameters  []byte
	Value       []byte
	ContentType string
}

type linuxSecretServiceBus interface {
	NameHasOwner(context.Context) (bool, error)
	ReadDefaultCollection(context.Context) (dbus.ObjectPath, error)
	IsLocked(context.Context, dbus.ObjectPath, string) (bool, error)
	OpenSession(context.Context) (dbus.ObjectPath, error)
	CloseSession(context.Context, dbus.ObjectPath) error
	SearchItems(context.Context, dbus.ObjectPath, map[string]string) ([]dbus.ObjectPath, error)
	GetSecret(context.Context, dbus.ObjectPath, dbus.ObjectPath) (linuxSecret, error)
	CreateItem(context.Context, dbus.ObjectPath, map[string]dbus.Variant, linuxSecret, bool) (dbus.ObjectPath, dbus.ObjectPath, error)
	DeleteItem(context.Context, dbus.ObjectPath) (dbus.ObjectPath, error)
	Close() error
}

type linuxAdapter struct {
	bus linuxSecretServiceBus
}

func (a *linuxAdapter) Execute(ctx context.Context, request HelperRequest, setValue string) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, linuxNativeError(request.Operation, request.Name, err)
	}
	if request.Operation == OperationSet {
		if err := ValidateValue(setValue); err != nil {
			return Result{}, err
		}
	}
	collection, err := a.health(ctx)
	if err != nil {
		return Result{}, linuxNativeError(OperationHealth, request.Name, err)
	}
	if request.Operation == OperationHealth {
		return Result{}, nil
	}
	attributes := map[string]string{
		"service":  nativeServiceName,
		"username": request.Name,
	}
	switch request.Operation {
	case OperationGet:
		session, err := a.bus.OpenSession(ctx)
		if err != nil {
			return Result{}, linuxNativeError(request.Operation, request.Name, err)
		}
		defer a.bus.CloseSession(ctx, session)
		items, err := a.bus.SearchItems(ctx, collection, attributes)
		if err != nil {
			return Result{}, linuxNativeError(request.Operation, request.Name, err)
		}
		if len(items) == 0 {
			return Result{}, nil
		}
		locked, err := a.bus.IsLocked(ctx, items[0], linuxItemInterface)
		if err != nil {
			return Result{}, linuxNativeError(request.Operation, request.Name, err)
		}
		if locked {
			return Result{}, linuxNativeError(request.Operation, request.Name, errLinuxLocked)
		}
		secret, err := a.bus.GetSecret(ctx, items[0], session)
		if err != nil {
			return Result{}, linuxNativeError(request.Operation, request.Name, err)
		}
		if err := ValidateValue(string(secret.Value)); err != nil {
			return Result{}, &Error{Operation: request.Operation, Provider: "native", Name: request.Name, Condition: ConditionInvalidValue, Cause: err}
		}
		return Result{Value: string(secret.Value), Present: true}, nil
	case OperationSet:
		session, err := a.bus.OpenSession(ctx)
		if err != nil {
			return Result{}, linuxNativeError(request.Operation, request.Name, err)
		}
		defer a.bus.CloseSession(ctx, session)
		properties := map[string]dbus.Variant{
			linuxItemInterface + ".Label":      dbus.MakeVariant(fmt.Sprintf("mcpd secret %s", request.Name)),
			linuxItemInterface + ".Attributes": dbus.MakeVariant(attributes),
		}
		secret := linuxSecret{
			Session:     session,
			Parameters:  []byte{},
			Value:       []byte(setValue),
			ContentType: "text/plain; charset=utf8",
		}
		item, prompt, err := a.bus.CreateItem(ctx, collection, properties, secret, true)
		if err != nil {
			return Result{}, linuxNativeError(request.Operation, request.Name, err)
		}
		if item == "/" || prompt != "/" {
			return Result{}, linuxNativeError(request.Operation, request.Name, errLinuxPrompt)
		}
		return Result{}, nil
	case OperationDelete:
		items, err := a.bus.SearchItems(ctx, collection, attributes)
		if err != nil {
			return Result{}, linuxNativeError(request.Operation, request.Name, err)
		}
		for _, item := range items {
			prompt, err := a.bus.DeleteItem(ctx, item)
			if err != nil {
				return Result{}, linuxNativeError(request.Operation, request.Name, err)
			}
			if prompt != "/" {
				return Result{}, linuxNativeError(request.Operation, request.Name, errLinuxPrompt)
			}
		}
		return Result{}, nil
	default:
		return Result{}, linuxNativeError(request.Operation, request.Name, fmt.Errorf("unsupported native operation"))
	}
}

func (a *linuxAdapter) health(ctx context.Context) (dbus.ObjectPath, error) {
	owner, err := a.bus.NameHasOwner(ctx)
	if err != nil {
		return "", err
	}
	if !owner {
		return "", errLinuxNoServiceOwner
	}
	collection, err := a.bus.ReadDefaultCollection(ctx)
	if err != nil {
		return "", err
	}
	if collection == "" || collection == "/" {
		return "", errLinuxNoCollection
	}
	locked, err := a.bus.IsLocked(ctx, collection, linuxCollectionInterface)
	if err != nil {
		return "", err
	}
	if locked {
		return "", errLinuxLocked
	}
	return collection, nil
}

func linuxNativeError(operation Operation, name string, cause error) error {
	condition := ConditionUnexpected
	switch {
	case errors.Is(cause, context.DeadlineExceeded), errors.Is(cause, context.Canceled):
		condition = ConditionTimedOut
	case errors.Is(cause, errLinuxNoSessionBus), errors.Is(cause, errLinuxNoServiceOwner), errors.Is(cause, errLinuxNoCollection):
		condition = ConditionUnavailable
	case errors.Is(cause, errLinuxLocked):
		condition = ConditionLocked
	case errors.Is(cause, errLinuxPrompt):
		condition = ConditionInteraction
	default:
		var dbusErrorName string
		var dbusErrValue dbus.Error
		var dbusErrPointer *dbus.Error
		switch {
		case errors.As(cause, &dbusErrValue):
			dbusErrorName = dbusErrValue.Name
		case errors.As(cause, &dbusErrPointer):
			dbusErrorName = dbusErrPointer.Name
		}
		if dbusErrorName != "" {
			switch dbusErrorName {
			case "org.freedesktop.Secret.Error.IsLocked":
				condition = ConditionLocked
			case "org.freedesktop.Secret.Error.NoSession":
				condition = ConditionRetryable
			case "org.freedesktop.DBus.Error.AccessDenied", "org.freedesktop.DBus.Error.AuthFailed":
				condition = ConditionDenied
			case "org.freedesktop.DBus.Error.ServiceUnknown", "org.freedesktop.DBus.Error.NameHasNoOwner":
				condition = ConditionUnavailable
			case "org.freedesktop.Secret.Error.NoSuchObject":
				if operation == OperationGet || operation == OperationDelete {
					condition = ConditionNotFound
				} else {
					condition = ConditionUnavailable
				}
			}
		}
	}
	return &Error{Operation: operation, Provider: "native", Name: name, Condition: condition, Cause: cause}
}

type godbusSecretServiceBus struct {
	conn *dbus.Conn
}

func newGodbusSecretServiceBus() (*godbusSecretServiceBus, error) {
	conn, err := dbus.SessionBusPrivateNoAutoStartup()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errLinuxNoSessionBus, err)
	}
	if err := conn.Auth(nil); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("%w: %v", errLinuxNoSessionBus, err)
	}
	if err := conn.Hello(); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("%w: %v", errLinuxNoSessionBus, err)
	}
	return &godbusSecretServiceBus{conn: conn}, nil
}

func (b *godbusSecretServiceBus) NameHasOwner(ctx context.Context) (bool, error) {
	var owner bool
	err := b.conn.BusObject().CallWithContext(ctx, "org.freedesktop.DBus.NameHasOwner", 0, linuxSecretServiceName).Store(&owner)
	return owner, err
}

func (b *godbusSecretServiceBus) ReadDefaultCollection(ctx context.Context) (dbus.ObjectPath, error) {
	var collection dbus.ObjectPath
	err := b.service().CallWithContext(ctx, linuxSecretServiceInterface+".ReadAlias", 0, "default").Store(&collection)
	return collection, err
}

func (b *godbusSecretServiceBus) IsLocked(ctx context.Context, path dbus.ObjectPath, iface string) (bool, error) {
	var property dbus.Variant
	err := b.conn.Object(linuxSecretServiceName, path).
		CallWithContext(ctx, linuxPropertiesInterface+".Get", 0, iface, "Locked").
		Store(&property)
	if err != nil {
		return false, err
	}
	locked, ok := property.Value().(bool)
	if !ok {
		return false, fmt.Errorf("Secret Service Locked property has unexpected type")
	}
	return locked, nil
}

func (b *godbusSecretServiceBus) OpenSession(ctx context.Context) (dbus.ObjectPath, error) {
	var output dbus.Variant
	var session dbus.ObjectPath
	err := b.service().CallWithContext(ctx, linuxSecretServiceInterface+".OpenSession", 0, "plain", dbus.MakeVariant("")).
		Store(&output, &session)
	return session, err
}

func (b *godbusSecretServiceBus) CloseSession(ctx context.Context, session dbus.ObjectPath) error {
	return b.conn.Object(linuxSecretServiceName, session).
		CallWithContext(ctx, linuxSessionInterface+".Close", 0).Err
}

func (b *godbusSecretServiceBus) SearchItems(ctx context.Context, collection dbus.ObjectPath, attributes map[string]string) ([]dbus.ObjectPath, error) {
	var items []dbus.ObjectPath
	err := b.conn.Object(linuxSecretServiceName, collection).
		CallWithContext(ctx, linuxCollectionInterface+".SearchItems", 0, attributes).
		Store(&items)
	return items, err
}

func (b *godbusSecretServiceBus) GetSecret(ctx context.Context, item, session dbus.ObjectPath) (linuxSecret, error) {
	var secret linuxSecret
	err := b.conn.Object(linuxSecretServiceName, item).
		CallWithContext(ctx, linuxItemInterface+".GetSecret", 0, session).
		Store(&secret)
	return secret, err
}

func (b *godbusSecretServiceBus) CreateItem(ctx context.Context, collection dbus.ObjectPath, properties map[string]dbus.Variant, secret linuxSecret, replace bool) (dbus.ObjectPath, dbus.ObjectPath, error) {
	var item dbus.ObjectPath
	var prompt dbus.ObjectPath
	err := b.conn.Object(linuxSecretServiceName, collection).
		CallWithContext(ctx, linuxCollectionInterface+".CreateItem", 0, properties, secret, replace).
		Store(&item, &prompt)
	return item, prompt, err
}

func (b *godbusSecretServiceBus) DeleteItem(ctx context.Context, item dbus.ObjectPath) (dbus.ObjectPath, error) {
	var prompt dbus.ObjectPath
	err := b.conn.Object(linuxSecretServiceName, item).
		CallWithContext(ctx, linuxItemInterface+".Delete", 0).
		Store(&prompt)
	return prompt, err
}

func (b *godbusSecretServiceBus) Close() error {
	return b.conn.Close()
}

func (b *godbusSecretServiceBus) service() dbus.BusObject {
	return b.conn.Object(linuxSecretServiceName, linuxSecretServicePath)
}
