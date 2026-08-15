package tray

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ahodges22/mcpd/internal/config"
)

const controllerPollInterval = 5 * time.Second

type controllerStatusClient interface {
	Status(context.Context) (Status, error)
}

type controllerActionClient interface {
	Reconnect(context.Context, string) error
	Authorize(context.Context, string) (string, error)
}

type controllerClient interface {
	controllerStatusClient
	controllerActionClient
}

type actionRequest struct {
	command MenuCommand
	backend string
}

type Controller struct {
	client         controllerStatusClient
	actions        controllerActionClient
	openURL        func(context.Context, string) error
	polls          <-chan time.Time
	retry          chan struct{}
	actionRequests chan actionRequest
	actionResults  chan error
	updates        chan MenuModel
	pollInterval   time.Duration
	actionActive   atomic.Bool
	actionWG       sync.WaitGroup
	actionFailed   bool
}

func NewController(client *Client, openURL func(context.Context, string) error) *Controller {
	return newActionController(client, openURL, nil)
}

func newController(client controllerStatusClient, polls <-chan time.Time) *Controller {
	return &Controller{
		client:         client,
		polls:          polls,
		retry:          make(chan struct{}, 1),
		actionRequests: make(chan actionRequest, 1),
		actionResults:  make(chan error, 1),
		updates:        make(chan MenuModel, 1),
		pollInterval:   controllerPollInterval,
	}
}

func newActionController(client controllerClient, openURL func(context.Context, string) error, polls <-chan time.Time) *Controller {
	controller := newController(client, polls)
	controller.actions = client
	controller.openURL = openURL
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

// Repair accepts at most one reconnect or authorize action at a time.
func (c *Controller) Repair(command MenuCommand, backend string) {
	if c.actions == nil || !config.ValidName(backend) {
		return
	}
	if command != CommandReconnect && command != CommandAuthorize {
		return
	}
	if !c.actionActive.CompareAndSwap(false, true) {
		return
	}
	select {
	case c.actionRequests <- actionRequest{command: command, backend: backend}:
	default:
		c.actionActive.Store(false)
	}
}

// Run owns controller state until ctx is canceled and closes Updates on exit.
func (c *Controller) Run(ctx context.Context) {
	defer func() {
		c.actionWG.Wait()
		close(c.updates)
	}()

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
		case request := <-c.actionRequests:
			c.startAction(ctx, request)
			continue
		case err := <-c.actionResults:
			c.actionActive.Store(false)
			c.actionFailed = err != nil
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
	c.publish(BuildMenu(status, c.actionFailed))
	return true
}

func (c *Controller) startAction(ctx context.Context, request actionRequest) {
	c.actionWG.Add(1)
	go func() {
		defer c.actionWG.Done()
		err := c.runAction(ctx, request)
		select {
		case c.actionResults <- err:
		case <-ctx.Done():
		}
	}()
}

func (c *Controller) runAction(ctx context.Context, request actionRequest) error {
	switch request.command {
	case CommandReconnect:
		return c.actions.Reconnect(ctx, request.backend)
	case CommandAuthorize:
		target, err := c.actions.Authorize(ctx, request.backend)
		if err != nil || target == "" {
			return err
		}
		return OpenAuthorizeURL(ctx, target, c.openURL)
	default:
		return nil
	}
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
