package tray

import (
	"context"
	"log/slog"
	"sync"
)

type nativeDriver interface {
	Apply(MenuModel) error
	Ready() <-chan struct{}
	Run() error
	Remove()
}

type NativeAdapter struct {
	driver nativeDriver
}

type nativeLatestModel struct {
	mu    sync.Mutex
	model *MenuModel
}

func (s *nativeLatestModel) publish(model MenuModel, wake chan<- struct{}) {
	owned := cloneMenuModel(model)
	s.mu.Lock()
	s.model = &owned
	s.mu.Unlock()
	select {
	case wake <- struct{}{}:
	default:
	}
}

func (s *nativeLatestModel) take() *MenuModel {
	s.mu.Lock()
	defer s.mu.Unlock()
	model := s.model
	s.model = nil
	return model
}

// Run owns the native loop on its caller. On macOS, construction, the initial
// apply, and Run must all execute on the startup thread locked by main.
func (a *NativeAdapter) Run(ctx context.Context, updates <-chan MenuModel) error {
	if ctx.Err() != nil {
		a.driver.Remove()
		return nil
	}

	if err := a.driver.Apply(BuildOfflineMenu()); err != nil {
		slog.Warn("tray: initial native apply failed", "err", err)
	}
	if ctx.Err() != nil {
		a.driver.Remove()
		return nil
	}

	shutdown := make(chan struct{})
	runDone := make(chan struct{})
	wake := make(chan struct{}, 1)
	coordinatorDone := make(chan struct{})
	applyDone := make(chan struct{})
	var latest nativeLatestModel

	go func() {
		defer close(coordinatorDone)
		for {
			select {
			case <-ctx.Done():
				close(shutdown)
				a.driver.Remove()
				close(wake)
				return
			case <-runDone:
				close(shutdown)
				a.driver.Remove()
				close(wake)
				return
			case model, ok := <-updates:
				if !ok {
					close(shutdown)
					a.driver.Remove()
					close(wake)
					return
				}
				latest.publish(model, wake)
			}
		}
	}()

	go func() {
		defer close(applyDone)
		select {
		case <-shutdown:
			return
		case <-a.driver.Ready():
		}
		for {
			select {
			case <-shutdown:
				return
			case _, ok := <-wake:
				if !ok {
					return
				}
			}

			model := latest.take()
			if model == nil {
				continue
			}
			if err := a.driver.Apply(*model); err != nil {
				select {
				case <-shutdown:
					return
				default:
					slog.Warn("tray: native apply failed", "err", err)
				}
			}
		}
	}()

	runErr := a.driver.Run()
	close(runDone)
	<-coordinatorDone
	<-applyDone
	return runErr
}

func cloneMenuModel(model MenuModel) MenuModel {
	return MenuModel{Icon: model.Icon, Items: cloneMenuItems(model.Items)}
}

func cloneMenuItems(items []MenuItem) []MenuItem {
	if items == nil {
		return nil
	}
	owned := make([]MenuItem, len(items))
	for i := range items {
		owned[i] = items[i]
		owned[i].Children = cloneMenuItems(items[i].Children)
	}
	return owned
}
