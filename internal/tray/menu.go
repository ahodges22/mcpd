package tray

import (
	_ "embed"
	"fmt"

	"github.com/ahodges22/mcpd/internal/config"
)

type TrayIcon string

const (
	IconHealthy   TrayIcon = "healthy"
	IconAttention TrayIcon = "attention"
	IconOffline   TrayIcon = "offline"
)

//go:embed assets/healthy.png
var healthyIcon []byte

//go:embed assets/attention.png
var attentionIcon []byte

//go:embed assets/offline.png
var offlineIcon []byte

func (i TrayIcon) Bytes() []byte {
	switch i {
	case IconHealthy:
		return healthyIcon
	case IconAttention:
		return attentionIcon
	case IconOffline:
		return offlineIcon
	default:
		return nil
	}
}

type MenuCommand string

const (
	CommandReconnect MenuCommand = "reconnect"
	CommandAuthorize MenuCommand = "authorize"
	CommandDashboard MenuCommand = "dashboard"
	CommandRetry     MenuCommand = "retry"
	CommandQuit      MenuCommand = "quit"
)

type MenuItem struct {
	Label     string
	Command   MenuCommand
	Backend   string
	Disabled  bool
	Separator bool
	Children  []MenuItem
}

type MenuModel struct {
	Icon  TrayIcon
	Items []MenuItem
}

func BuildMenu(status Status, actionFailed bool) MenuModel {
	model := MenuModel{Icon: IconHealthy}
	model.Items = append(model.Items, MenuItem{
		Label:    fmt.Sprintf("%d of %d backends serving", status.Serving, len(status.Backends)),
		Disabled: true,
	})

	var repairs []MenuItem
	var servers []MenuItem
	for _, backend := range status.Backends {
		if !config.ValidName(backend.Name) {
			continue
		}
		servers = append(servers, MenuItem{Label: backend.Name + " - " + backend.Label, Disabled: true})
		switch backend.RecommendedAction {
		case ActionAuthorize:
			repairs = append(repairs, MenuItem{Label: "Authorize " + backend.Name, Command: CommandAuthorize, Backend: backend.Name})
		case ActionReconnect:
			repairs = append(repairs, MenuItem{Label: "Reconnect " + backend.Name, Command: CommandReconnect, Backend: backend.Name})
		}
	}
	if len(repairs) != 0 {
		model.Icon = IconAttention
		model.Items = append(model.Items, repairs...)
		model.Items = append(model.Items, MenuItem{Separator: true})
	}
	model.Items = append(model.Items, MenuItem{Label: "All servers", Children: servers}, MenuItem{Separator: true})
	if actionFailed {
		model.Items = append(model.Items, MenuItem{Label: "Repair failed. Open dashboard for details.", Disabled: true})
	}
	model.Items = append(model.Items,
		MenuItem{Label: "Open dashboard", Command: CommandDashboard},
		MenuItem{Label: "Refresh status", Command: CommandRetry},
		MenuItem{Label: "Quit status icon", Command: CommandQuit},
	)
	return model
}

func BuildOfflineMenu() MenuModel {
	return MenuModel{
		Icon: IconOffline,
		Items: []MenuItem{
			{Label: "mcpd is unreachable", Disabled: true},
			{Label: "Open dashboard", Command: CommandDashboard},
			{Label: "Retry now", Command: CommandRetry},
			{Label: "Quit status icon", Command: CommandQuit},
		},
	}
}
