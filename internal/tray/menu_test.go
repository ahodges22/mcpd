package tray

import (
	"bytes"
	"image"
	"image/color"
	_ "image/png"
	"reflect"
	"testing"
)

func TestMenuModel(t *testing.T) {
	status := Status{
		Serving: 2,
		Backends: []BackendStatus{
			{Name: "alpha", State: "up", Label: "Serving"},
			{Name: "secured", State: "needs-auth", Label: "Needs authorizing", RecommendedAction: ActionAuthorize},
			{Name: "broken", State: "down", Label: "Not answering", RecommendedAction: ActionReconnect},
			{Name: "turned-off", State: "disabled", Label: "Turned off"},
			{Name: "invalid/name", State: "down", Label: "untrusted", RecommendedAction: ActionReconnect},
		},
	}

	got := BuildMenu(status, true)
	want := MenuModel{
		Icon: IconAttention,
		Items: []MenuItem{
			{Label: "2 of 5 backends serving", Disabled: true},
			{Label: "Authorize secured", Command: CommandAuthorize, Backend: "secured"},
			{Label: "Reconnect broken", Command: CommandReconnect, Backend: "broken"},
			{Separator: true},
			{Label: "All servers", Children: []MenuItem{
				{Label: "alpha - Serving", Disabled: true},
				{Label: "secured - Needs authorizing", Disabled: true},
				{Label: "broken - Not answering", Disabled: true},
				{Label: "turned-off - Turned off", Disabled: true},
			}},
			{Separator: true},
			{Label: "Repair failed. Open dashboard for details.", Disabled: true},
			{Label: "Open dashboard", Command: CommandDashboard},
			{Label: "Refresh status", Command: CommandRetry},
			{Label: "Quit status icon", Command: CommandQuit},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("BuildMenu() =\n%#v\nwant\n%#v", got, want)
	}

	status.Backends[1].RecommendedAction = "unknown"
	status.Backends[2].RecommendedAction = ""
	neutral := BuildMenu(status, false)
	if neutral.Icon != IconHealthy {
		t.Errorf("neutral icon = %q, want healthy", neutral.Icon)
	}
	for _, item := range neutral.Items {
		if item.Command == CommandAuthorize || item.Command == CommandReconnect {
			t.Errorf("neutral menu includes repair item %+v", item)
		}
	}
}

func TestMenuModelOffline(t *testing.T) {
	got := BuildOfflineMenu()
	want := MenuModel{
		Icon: IconOffline,
		Items: []MenuItem{
			{Label: "mcpd is unreachable", Disabled: true},
			{Label: "Open dashboard", Command: CommandDashboard},
			{Label: "Retry now", Command: CommandRetry},
			{Label: "Quit status icon", Command: CommandQuit},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("BuildOfflineMenu() = %#v, want %#v", got, want)
	}
	for _, item := range got.Items {
		if item.Command == CommandAuthorize || item.Command == CommandReconnect || len(item.Children) != 0 {
			t.Errorf("offline menu kept a backend repair item: %+v", item)
		}
	}
}

func TestTrayAssets(t *testing.T) {
	icons := map[TrayIcon][]byte{
		IconHealthy:   IconHealthy.Bytes(),
		IconAttention: IconAttention.Bytes(),
		IconOffline:   IconOffline.Bytes(),
	}
	seen := make([][]byte, 0, len(icons))
	seenColors := make(map[color.NRGBA]TrayIcon, len(icons))
	for name, data := range icons {
		if len(data) == 0 {
			t.Fatalf("%s icon is empty", name)
		}
		for _, prior := range seen {
			if bytes.Equal(data, prior) {
				t.Fatalf("%s icon duplicates another state", name)
			}
		}
		seen = append(seen, data)

		img, format, err := image.Decode(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("decode %s icon: %v", name, err)
		}
		if format != "png" || img.Bounds() != image.Rect(0, 0, 22, 22) {
			t.Errorf("%s icon = %s %v, want 22x22 PNG", name, format, img.Bounds())
		}
		visible := 0
		outlined := 0
		colored := 0
		var foreground color.NRGBA
		for y := img.Bounds().Min.Y; y < img.Bounds().Max.Y; y++ {
			for x := img.Bounds().Min.X; x < img.Bounds().Max.X; x++ {
				pixel := color.NRGBAModel.Convert(img.At(x, y)).(color.NRGBA)
				if pixel.A == 0 {
					continue
				}
				visible++
				if pixel == (color.NRGBA{A: 0xff}) {
					outlined++
				} else if pixel.A != 0xff {
					t.Errorf("%s icon has unexpected pixel at %d,%d: %#v", name, x, y, pixel)
				} else {
					colored++
					if foreground == (color.NRGBA{}) {
						foreground = pixel
					} else if pixel != foreground {
						t.Errorf("%s icon has multiple status colors: %#v and %#v", name, foreground, pixel)
					}
				}
			}
		}
		if visible == 0 {
			t.Errorf("%s icon has no visible pixels", name)
		}
		if colored == 0 {
			t.Errorf("%s icon has no status-colored pixels", name)
		}
		if outlined == 0 {
			t.Errorf("%s icon has no black outline", name)
		}
		if prior, exists := seenColors[foreground]; exists {
			t.Errorf("%s and %s icons use the same status color %#v", prior, name, foreground)
		}
		seenColors[foreground] = name
	}
}
