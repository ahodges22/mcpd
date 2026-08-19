package tray

import (
	"errors"
	"fmt"
	"runtime"

	"github.com/ahodges22/systray"
)

type nativeMenuItemSink interface {
	SetDisabled(bool)
}

type nativeMenuSink interface {
	Add(string, func()) nativeMenuItemSink
	AddSeparator()
	AddSubmenu(string) (nativeMenuItemSink, nativeMenuSink)
}

func buildNativeMenu(menu nativeMenuSink, items []MenuItem, activate func(MenuCommand, string)) {
	for _, item := range items {
		if item.Separator {
			menu.AddSeparator()
			continue
		}

		if item.Children != nil {
			native, submenu := menu.AddSubmenu(item.Label)
			native.SetDisabled(item.Disabled)
			buildNativeMenu(submenu, item.Children, activate)
			continue
		}

		command, backend := item.Command, item.Backend
		var onClick func()
		if !item.Disabled && isActionableMenuCommand(command) {
			onClick = func() {
				if activate != nil {
					activate(command, backend)
				}
			}
		}
		native := menu.Add(item.Label, onClick)
		native.SetDisabled(item.Disabled)
	}
}

func isActionableMenuCommand(command MenuCommand) bool {
	switch command {
	case CommandReconnect, CommandAuthorize, CommandDashboard, CommandRetry, CommandQuit:
		return true
	default:
		return false
	}
}

type systrayMenuSink struct {
	menu *systray.Menu
}

func (s systrayMenuSink) Add(label string, onClick func()) nativeMenuItemSink {
	return s.menu.Add(label, onClick)
}

func (s systrayMenuSink) AddSeparator() {
	s.menu.AddSeparator()
}

func (s systrayMenuSink) AddSubmenu(label string) (nativeMenuItemSink, nativeMenuSink) {
	submenu := systray.NewMenu()
	return s.menu.AddSubmenu(label, submenu), systrayMenuSink{menu: submenu}
}

type nativeSystrayHandle interface {
	SetIconErr([]byte) error
	SetTemplateIconErr([]byte) error
	SetTooltipErr(string) error
	SetMenuErr(*systray.Menu) error
	ShowErr() error
	Ready() <-chan struct{}
	Run() error
	Remove()
}

type systrayDriver struct {
	tray            nativeSystrayHandle
	activate        func(MenuCommand, string)
	useTemplateIcon bool
	menuApplied     bool
	menuItems       []MenuItem
	shown           bool
}

func (d *systrayDriver) Apply(model MenuModel) error {
	var applyErrors []error
	if d.useTemplateIcon {
		if err := d.tray.SetTemplateIconErr(model.Icon.Bytes()); err != nil {
			applyErrors = append(applyErrors, fmt.Errorf("set template icon: %w", err))
		}
	} else if err := d.tray.SetIconErr(model.Icon.Bytes()); err != nil {
		applyErrors = append(applyErrors, fmt.Errorf("set icon: %w", err))
	}

	if err := d.tray.SetTooltipErr("mcpd"); err != nil {
		applyErrors = append(applyErrors, fmt.Errorf("set tooltip: %w", err))
	}

	if !d.menuApplied || !sameMenuItems(d.menuItems, model.Items) {
		menu := systray.NewMenu()
		buildNativeMenu(systrayMenuSink{menu: menu}, model.Items, d.activate)
		if err := d.tray.SetMenuErr(menu); err != nil {
			applyErrors = append(applyErrors, fmt.Errorf("set menu: %w", err))
		} else {
			d.menuItems = cloneMenuItems(model.Items)
			d.menuApplied = true
		}
	}

	if !d.shown {
		if err := d.tray.ShowErr(); err != nil {
			applyErrors = append(applyErrors, fmt.Errorf("show tray: %w", err))
		} else {
			d.shown = true
		}
	}
	return errors.Join(applyErrors...)
}

func (d *systrayDriver) Ready() <-chan struct{} { return d.tray.Ready() }
func (d *systrayDriver) Run() error             { return d.tray.Run() }
func (d *systrayDriver) Remove()                { d.tray.Remove() }

func sameMenuItems(left, right []MenuItem) bool {
	if len(left) != len(right) || (left == nil) != (right == nil) {
		return false
	}
	for i := range left {
		if left[i].Label != right[i].Label ||
			left[i].Command != right[i].Command ||
			left[i].Backend != right[i].Backend ||
			left[i].Disabled != right[i].Disabled ||
			left[i].Separator != right[i].Separator ||
			!sameMenuItems(left[i].Children, right[i].Children) {
			return false
		}
	}
	return true
}

// NewNativeAdapter constructs the native tray on its caller. On macOS this
// must be the startup thread already locked by the first statement of main.
func NewNativeAdapter(activate func(MenuCommand, string)) (*NativeAdapter, error) {
	tray, err := systray.NewWithError()
	if err != nil {
		return nil, err
	}
	return &NativeAdapter{driver: &systrayDriver{
		tray:            tray,
		activate:        activate,
		useTemplateIcon: runtime.GOOS == "darwin",
	}}, nil
}
