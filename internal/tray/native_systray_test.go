package tray

import (
	"bytes"
	"errors"
	"image"
	_ "image/png"
	"sync"
	"testing"

	"github.com/ahodges22/systray"
)

type recordingNativeMenu struct {
	items []*recordingNativeItem
}

type recordingNativeItem struct {
	label     string
	callback  func()
	disabled  bool
	separator bool
	children  *recordingNativeMenu
}

func (m *recordingNativeMenu) Add(label string, callback func()) nativeMenuItemSink {
	item := &recordingNativeItem{label: label, callback: callback}
	m.items = append(m.items, item)
	return item
}

func (m *recordingNativeMenu) AddSeparator() {
	m.items = append(m.items, &recordingNativeItem{separator: true})
}

func (m *recordingNativeMenu) AddSubmenu(label string) (nativeMenuItemSink, nativeMenuSink) {
	item := &recordingNativeItem{label: label, children: &recordingNativeMenu{}}
	m.items = append(m.items, item)
	return item, item.children
}

func (i *recordingNativeItem) SetDisabled(disabled bool) { i.disabled = disabled }

func TestBuildNativeMenu(t *testing.T) {
	var mu sync.Mutex
	var activated []string
	record := func(command MenuCommand, backend string) {
		mu.Lock()
		defer mu.Unlock()
		activated = append(activated, string(command)+":"+backend)
	}

	root := &recordingNativeMenu{}
	buildNativeMenu(root, []MenuItem{
		{Label: "Information", Disabled: true},
		{Separator: true},
		{Label: "Servers", Children: []MenuItem{
			{Label: "Reconnect alpha", Command: CommandReconnect, Backend: "alpha"},
			{Label: "Disabled beta", Command: CommandAuthorize, Backend: "beta", Disabled: true},
			{Label: "Unknown", Command: MenuCommand("unknown"), Backend: "unknown"},
			{Label: "No command"},
			{Label: "Authorize beta", Command: CommandAuthorize, Backend: "beta"},
		}},
	}, record)

	if len(root.items) != 3 {
		t.Fatalf("root item count = %d, want 3", len(root.items))
	}
	if !root.items[0].disabled || root.items[0].callback != nil {
		t.Fatal("informational item must be disabled with no callback")
	}
	if !root.items[1].separator {
		t.Fatal("second item is not a separator")
	}
	submenu := root.items[2]
	if submenu.children == nil || len(submenu.children.items) != 5 {
		t.Fatal("submenu shape was not preserved")
	}
	children := submenu.children.items
	if children[1].callback != nil || children[2].callback != nil || children[3].callback != nil {
		t.Fatal("disabled, unknown, and zero-command items must not have callbacks")
	}
	if children[0].callback == nil || children[4].callback == nil {
		t.Fatal("enabled actionable items must have callbacks")
	}

	children[4].callback()
	children[0].callback()
	mu.Lock()
	defer mu.Unlock()
	want := []string{"authorize:beta", "reconnect:alpha"}
	if len(activated) != len(want) || activated[0] != want[0] || activated[1] != want[1] {
		t.Fatalf("callbacks = %v, want %v", activated, want)
	}
}

type fakeSystrayHandle struct {
	iconErr       error
	templateErr   error
	tooltipErr    error
	menuErr       error
	showErr       error
	iconCalls     int
	templateCalls int
	tooltipCalls  int
	menuCalls     int
	showCalls     int
	lastMenu      *systray.Menu
	ready         chan struct{}
}

func newFakeSystrayHandle() *fakeSystrayHandle {
	ready := make(chan struct{})
	close(ready)
	return &fakeSystrayHandle{ready: ready}
}

func (h *fakeSystrayHandle) SetIconErr([]byte) error {
	h.iconCalls++
	return h.iconErr
}

func (h *fakeSystrayHandle) SetTemplateIconErr([]byte) error {
	h.templateCalls++
	return h.templateErr
}

func (h *fakeSystrayHandle) SetTooltipErr(string) error {
	h.tooltipCalls++
	return h.tooltipErr
}

func (h *fakeSystrayHandle) SetMenuErr(menu *systray.Menu) error {
	h.menuCalls++
	h.lastMenu = menu
	return h.menuErr
}

func (h *fakeSystrayHandle) ShowErr() error {
	h.showCalls++
	return h.showErr
}

func (h *fakeSystrayHandle) Ready() <-chan struct{} { return h.ready }
func (h *fakeSystrayHandle) Run() error             { return nil }
func (h *fakeSystrayHandle) Remove()                {}

func TestSystrayDriverApply(t *testing.T) {
	t.Run("attempts every field and joins setter errors", func(t *testing.T) {
		iconErr := errors.New("icon")
		tooltipErr := errors.New("tooltip")
		menuErr := errors.New("menu")
		showErr := errors.New("show")
		handle := newFakeSystrayHandle()
		handle.iconErr = iconErr
		handle.tooltipErr = tooltipErr
		handle.menuErr = menuErr
		handle.showErr = showErr
		driver := &systrayDriver{tray: handle, useTemplateIcon: false}

		err := driver.Apply(nativeTestModel("all fields"))
		for _, want := range []error{iconErr, tooltipErr, menuErr, showErr} {
			if !errors.Is(err, want) {
				t.Fatalf("Apply error %v does not contain %v", err, want)
			}
		}
		if handle.iconCalls != 1 || handle.tooltipCalls != 1 || handle.menuCalls != 1 || handle.showCalls != 1 {
			t.Fatalf("setter calls = icon:%d tooltip:%d menu:%d show:%d", handle.iconCalls, handle.tooltipCalls, handle.menuCalls, handle.showCalls)
		}
	})

	t.Run("uses template icon and preserves its error", func(t *testing.T) {
		wantErr := errors.New("template")
		handle := newFakeSystrayHandle()
		handle.templateErr = wantErr
		driver := &systrayDriver{tray: handle, useTemplateIcon: true}
		if err := driver.Apply(nativeTestModel("template")); !errors.Is(err, wantErr) {
			t.Fatalf("Apply error = %v, want %v", err, wantErr)
		}
		if handle.templateCalls != 1 || handle.iconCalls != 0 {
			t.Fatalf("icon calls = template:%d regular:%d", handle.templateCalls, handle.iconCalls)
		}
	})

	t.Run("retries failed show and failed menu", func(t *testing.T) {
		handle := newFakeSystrayHandle()
		handle.showErr = errors.New("show")
		handle.menuErr = errors.New("menu")
		driver := &systrayDriver{tray: handle}
		model := nativeTestModel("retry")
		_ = driver.Apply(model)
		handle.showErr = nil
		handle.menuErr = nil
		if err := driver.Apply(model); err != nil {
			t.Fatalf("second Apply: %v", err)
		}
		if handle.showCalls != 2 || handle.menuCalls != 2 {
			t.Fatalf("retry calls = show:%d menu:%d, want 2 each", handle.showCalls, handle.menuCalls)
		}
		if err := driver.Apply(model); err != nil {
			t.Fatalf("third Apply: %v", err)
		}
		if handle.showCalls != 2 || handle.menuCalls != 2 {
			t.Fatalf("successful fields were repeated = show:%d menu:%d", handle.showCalls, handle.menuCalls)
		}
	})

	base := MenuModel{Icon: IconHealthy, Items: []MenuItem{{Label: "root", Command: CommandRetry, Backend: "alpha", Children: []MenuItem{{Label: "child"}}}}}
	changes := map[string]func(*MenuModel){
		"label":     func(m *MenuModel) { m.Items[0].Label = "changed" },
		"command":   func(m *MenuModel) { m.Items[0].Command = CommandQuit },
		"backend":   func(m *MenuModel) { m.Items[0].Backend = "beta" },
		"disabled":  func(m *MenuModel) { m.Items[0].Disabled = true },
		"separator": func(m *MenuModel) { m.Items[0].Separator = true },
		"child":     func(m *MenuModel) { m.Items[0].Children[0].Label = "changed child" },
	}
	for name, change := range changes {
		t.Run("replaces changed "+name, func(t *testing.T) {
			handle := newFakeSystrayHandle()
			driver := &systrayDriver{tray: handle}
			if err := driver.Apply(base); err != nil {
				t.Fatalf("first Apply: %v", err)
			}
			if err := driver.Apply(cloneMenuModel(base)); err != nil {
				t.Fatalf("identical Apply: %v", err)
			}
			if handle.menuCalls != 1 {
				t.Fatalf("identical menu calls = %d, want 1", handle.menuCalls)
			}
			changed := cloneMenuModel(base)
			change(&changed)
			if err := driver.Apply(changed); err != nil {
				t.Fatalf("changed Apply: %v", err)
			}
			if handle.menuCalls != 2 {
				t.Fatalf("changed menu calls = %d, want 2", handle.menuCalls)
			}
		})
	}
}

func TestTemplateIconAlphaMasksAreDistinct(t *testing.T) {
	masks := map[TrayIcon][]byte{}
	for _, icon := range []TrayIcon{IconHealthy, IconAttention, IconOffline} {
		decoded, _, err := image.Decode(bytes.NewReader(icon.Bytes()))
		if err != nil {
			t.Fatalf("decode %s icon: %v", icon, err)
		}
		bounds := decoded.Bounds()
		mask := make([]byte, 0, bounds.Dx()*bounds.Dy()*2)
		for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
			for x := bounds.Min.X; x < bounds.Max.X; x++ {
				_, _, _, alpha := decoded.At(x, y).RGBA()
				mask = append(mask, byte(alpha>>8), byte(alpha))
			}
		}
		masks[icon] = mask
	}
	for _, pair := range [][2]TrayIcon{{IconHealthy, IconAttention}, {IconHealthy, IconOffline}, {IconAttention, IconOffline}} {
		if bytes.Equal(masks[pair[0]], masks[pair[1]]) {
			t.Fatalf("%s and %s icons have identical alpha masks", pair[0], pair[1])
		}
	}
}
