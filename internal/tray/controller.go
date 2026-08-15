package tray

import (
	"context"
	"time"
)

const controllerPollInterval = 5 * time.Second

type controllerStatusClient interface {
	Status(context.Context) (Status, error)
}

type Controller struct {
	client       controllerStatusClient
	polls        <-chan time.Time
	retry        chan struct{}
	updates      chan MenuModel
	pollInterval time.Duration
}

func NewController(client controllerStatusClient) *Controller {
	return &Controller{
		client:       client,
		retry:        make(chan struct{}, 1),
		updates:      make(chan MenuModel, 1),
		pollInterval: controllerPollInterval,
	}
}

func newController(client controllerStatusClient, polls <-chan time.Time) *Controller {
	controller := NewController(client)
	controller.polls = polls
	return controller
}

func (c *Controller) Updates() <-chan MenuModel {
	return c.updates
}

// Retry queues a refresh without waiting for an active status request.
func (c *Controller) Retry() {
	select {
	case c.retry <- struct{}{}:
	default:
	}
}

// Run owns controller state until ctx is canceled and closes Updates on exit.
func (c *Controller) Run(ctx context.Context) {
	defer close(c.updates)

	polls := c.polls
	var ticker *time.Ticker
	if polls == nil {
		ticker = time.NewTicker(c.pollInterval)
		defer ticker.Stop()
		polls = ticker.C
	}

	if !c.refresh(ctx) {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-polls:
		case <-c.retry:
		}
		if !c.refresh(ctx) {
			return
		}
	}
}

func (c *Controller) refresh(ctx context.Context) bool {
	status, err := c.client.Status(ctx)
	if ctx.Err() != nil {
		return false
	}
	if err != nil {
		c.publish(BuildOfflineMenu())
		return true
	}
	c.publish(BuildMenu(status, false))
	return true
}

func (c *Controller) publish(model MenuModel) {
	select {
	case c.updates <- model:
		return
	default:
	}

	select {
	case <-c.updates:
	default:
	}
	select {
	case c.updates <- model:
	default:
	}
}
