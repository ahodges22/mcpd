package main

import (
	"context"
	"log"
	"os"
	"runtime"
	"time"

	"github.com/ahodges22/mcpd/internal/tray"
)

func main() {
	runtime.LockOSThread()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	updates := make(chan tray.MenuModel, 1)
	adapter, err := tray.NewNativeAdapter(func(command tray.MenuCommand, backend string) {
		log.Printf("activate command=%s backend=%s", command, backend)
		if command == tray.CommandQuit {
			cancel()
		}
	})
	if err != nil {
		log.Fatal(err)
	}

	go publishSnapshots(ctx, updates)
	if delay, err := time.ParseDuration(os.Getenv("MCPD_TRAY_SESSION_AUTO_CANCEL")); err == nil && delay > 0 {
		go func() {
			select {
			case <-time.After(delay):
				cancel()
			case <-ctx.Done():
			}
		}()
	}
	if err := adapter.Run(ctx, updates); err != nil {
		log.Fatal(err)
	}
}

func publishSnapshots(ctx context.Context, updates chan<- tray.MenuModel) {
	defer close(updates)
	models := []tray.MenuModel{
		testMenu(tray.IconHealthy, "2 of 2 backends serving", "alpha - connected"),
		testMenu(tray.IconAttention, "1 of 2 backends serving", "alpha - authorization required"),
		testMenu(tray.IconOffline, "mcpd is unreachable", "alpha - unavailable"),
	}
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for index := 0; ; index = (index + 1) % len(models) {
		select {
		case updates <- models[index]:
		case <-ctx.Done():
			return
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return
		}
	}
}

func testMenu(icon tray.TrayIcon, summary, backend string) tray.MenuModel {
	return tray.MenuModel{
		Icon: icon,
		Items: []tray.MenuItem{
			{Label: summary, Disabled: true},
			{Label: "All servers", Children: []tray.MenuItem{{Label: backend, Disabled: true}}},
			{Separator: true},
			{Label: "Enabled zero-command row"},
			{Label: "Disabled actionable row", Command: tray.CommandRetry, Disabled: true},
			{Label: "Run callback probe", Command: tray.CommandRetry},
			{Label: "Quit status icon", Command: tray.CommandQuit},
		},
	}
}
