package secretstore

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/ahodges22/mcpd/internal/config"
)

const (
	defaultStartupResolutionBudget = 5 * time.Second
	defaultProviderCallTimeout     = 5 * time.Second
	defaultStatusBudget            = 5 * time.Second
	defaultBusyBackoffBase         = 250 * time.Millisecond
	defaultBusyBackoffMax          = 2 * time.Second
	defaultFailureBackoff          = time.Second
	defaultFailureBackoffMax       = 5 * time.Minute
	defaultPresenceTTL             = 5 * time.Minute
	defaultFileWatchDebounce       = 100 * time.Millisecond
	defaultFileWatchPollInterval   = 30 * time.Second
)

type ResolutionTuning struct {
	StartupBudget         time.Duration
	CallTimeout           time.Duration
	BusyBackoffBase       time.Duration
	BusyBackoffMax        time.Duration
	FailureBackoff        time.Duration
	FailureBackoffMax     time.Duration
	PresenceTTL           time.Duration
	StatusBudget          time.Duration
	FileWatchDebounce     time.Duration
	FileWatchPollInterval time.Duration
}

type ResolvedConsumer struct {
	Consumer config.SecretConsumer
	Values   map[string]string
}

type PendingConsumer struct {
	Consumer  config.SecretConsumer
	Condition Condition
}

type pendingResolution struct {
	consumer  config.SecretConsumer
	condition Condition
	nextAt    time.Time
	failures  int
}

type providerHealth struct {
	condition Condition
	nextAt    time.Time
	suspended bool
	failures  int
}

type ResolutionCoordinator struct {
	provider       Provider
	lookup         func(string) (string, bool)
	tuning         ResolutionTuning
	resolved       func(ResolvedConsumer)
	groups         []config.SecretConsumer
	references     map[string][]ConsumerIdentity
	referenceNames []string

	startOnce sync.Once
	workerWG  sync.WaitGroup
	wake      chan struct{}
	statusMu  sync.Mutex

	mu            sync.Mutex
	pending       map[string]*pendingResolution
	order         []string
	health        *providerHealth
	busyBackoff   time.Duration
	busyUntil     time.Time
	busyCondition Condition
	presence      map[string]presenceEntry

	mutationMu    sync.Mutex
	mutationHooks MutationHooks
}

func NewResolutionCoordinator(
	cfg *config.Config,
	provider Provider,
	lookup func(string) (string, bool),
	tuning ResolutionTuning,
	resolved func(ResolvedConsumer),
) *ResolutionCoordinator {
	if lookup == nil {
		lookup = func(string) (string, bool) { return "", false }
	}
	if tuning.StartupBudget <= 0 {
		tuning.StartupBudget = defaultStartupResolutionBudget
	}
	if tuning.CallTimeout <= 0 {
		tuning.CallTimeout = defaultProviderCallTimeout
	}
	if tuning.BusyBackoffBase <= 0 {
		tuning.BusyBackoffBase = defaultBusyBackoffBase
	}
	if tuning.BusyBackoffMax <= 0 {
		tuning.BusyBackoffMax = defaultBusyBackoffMax
	}
	if tuning.BusyBackoffMax < tuning.BusyBackoffBase {
		tuning.BusyBackoffMax = tuning.BusyBackoffBase
	}
	if tuning.FailureBackoff <= 0 {
		tuning.FailureBackoff = defaultFailureBackoff
	}
	if tuning.FailureBackoffMax <= 0 {
		tuning.FailureBackoffMax = defaultFailureBackoffMax
	}
	if tuning.FailureBackoffMax < tuning.FailureBackoff {
		tuning.FailureBackoffMax = tuning.FailureBackoff
	}
	if tuning.PresenceTTL <= 0 {
		tuning.PresenceTTL = defaultPresenceTTL
	}
	if tuning.StatusBudget <= 0 {
		tuning.StatusBudget = defaultStatusBudget
	}
	if tuning.FileWatchDebounce <= 0 {
		tuning.FileWatchDebounce = defaultFileWatchDebounce
	}
	if tuning.FileWatchPollInterval <= 0 {
		tuning.FileWatchPollInterval = defaultFileWatchPollInterval
	}
	var groups []config.SecretConsumer
	if cfg != nil {
		groups = cfg.SecretConsumers()
		if cfg.Secrets == nil || !cfg.Secrets.Enabled() {
			provider = nil
		}
	} else {
		provider = nil
	}
	groups, references, referenceNames := buildConsumerIndex(groups)
	return &ResolutionCoordinator{
		provider:       provider,
		lookup:         lookup,
		tuning:         tuning,
		resolved:       resolved,
		groups:         groups,
		references:     references,
		referenceNames: referenceNames,
		wake:           make(chan struct{}, 1),
		pending:        map[string]*pendingResolution{},
		busyBackoff:    tuning.BusyBackoffBase,
		presence:       map[string]presenceEntry{},
	}
}

func (c *ResolutionCoordinator) Start(ctx context.Context) {
	c.startOnce.Do(func() {
		providerGroups := make([]config.SecretConsumer, 0, len(c.groups))
		for _, group := range c.groups {
			if c.environmentOnly(group) {
				c.deliver(group, c.environmentValues(group))
				continue
			}
			providerGroups = append(providerGroups, group)
		}

		startupCtx, cancel := context.WithTimeout(ctx, c.tuning.StartupBudget)
		defer cancel()
		for i, group := range providerGroups {
			if err := startupCtx.Err(); err != nil {
				c.enqueue(group, ConditionTimedOut, c.failureDelay(), 0)
				for _, later := range providerGroups[i+1:] {
					c.enqueue(later, ConditionTimedOut, c.currentFailureDelay(), 0)
				}
				break
			}
			if condition, blocked := c.blockedHealth(); blocked {
				c.enqueue(group, condition, c.currentFailureDelay(), 0)
				continue
			}
			values, err := c.resolveGroup(startupCtx, group)
			if err != nil {
				condition, delay, failures := c.noteFailure(group, err)
				c.enqueue(group, condition, delay, failures)
				continue
			}
			c.noteSuccess()
			c.deliver(group, values)
		}
		c.workerWG.Add(1)
		go func() { defer c.workerWG.Done(); c.run(ctx) }()
		if store, ok := c.provider.(*FileStore); ok {
			done := store.startWatching(ctx, c.tuning.FileWatchDebounce, c.tuning.FileWatchPollInterval, c.refreshExternalChanges)
			c.workerWG.Add(1)
			go func() { defer c.workerWG.Done(); <-done }()
		}
	})
}

// Wait returns after every worker started by Start has stopped.
func (c *ResolutionCoordinator) Wait() { c.workerWG.Wait() }

func (c *ResolutionCoordinator) Pending() []PendingConsumer {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]PendingConsumer, 0, len(c.order))
	for _, key := range c.order {
		entry, ok := c.pending[key]
		if !ok {
			continue
		}
		out = append(out, PendingConsumer{Consumer: entry.consumer, Condition: entry.condition})
	}
	return out
}

func (c *ResolutionCoordinator) ProviderHealth() (Condition, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.health == nil {
		return "", false
	}
	return c.health.condition, true
}

func (c *ResolutionCoordinator) EnvironmentHas(name string) bool {
	_, present := c.lookup(name)
	return present
}

func (c *ResolutionCoordinator) ResolveConsumer(ctx context.Context, consumer config.SecretConsumer) (map[string]string, error) {
	providerBacked := !c.environmentOnly(consumer)
	if providerBacked {
		if condition, blocked := c.statusBlocked(); blocked {
			c.enqueue(consumer, condition, c.currentFailureDelay(), 0)
			c.signal()
			return nil, &Error{Operation: OperationGet, Provider: "configured", Condition: condition}
		}
	}
	values, err := c.resolveGroup(ctx, consumer)
	if err != nil {
		condition, delay, failures := c.noteFailure(consumer, err)
		c.enqueue(consumer, condition, delay, failures)
		c.signal()
		return nil, err
	}
	if providerBacked {
		c.noteSuccess()
	}
	c.removePending(consumer)
	c.signal()
	return values, nil
}

func (c *ResolutionCoordinator) Retry() {
	c.statusMu.Lock()
	defer c.statusMu.Unlock()
	if native, ok := c.provider.(NativeProvider); ok {
		native.Retry()
	}
	c.mu.Lock()
	now := time.Now()
	if c.health != nil {
		c.health.suspended = false
		c.health.nextAt = now
	}
	for _, entry := range c.pending {
		entry.nextAt = now
	}
	c.busyBackoff = c.tuning.BusyBackoffBase
	c.busyUntil = time.Time{}
	c.busyCondition = ""
	clear(c.presence)
	c.mu.Unlock()
	c.signal()
}

func (c *ResolutionCoordinator) run(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		entry, wait, ok := c.nextPending()
		if !ok {
			select {
			case <-ctx.Done():
				return
			case <-c.wake:
				continue
			}
		}
		if wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				if !timer.Stop() {
					<-timer.C
				}
				return
			case <-c.wake:
				if !timer.Stop() {
					<-timer.C
				}
				continue
			case <-timer.C:
			}
		}

		values, err := c.resolveGroup(ctx, entry.consumer)
		if err != nil {
			condition, delay, failures := c.noteFailure(entry.consumer, err)
			c.reschedule(entry.consumer, condition, delay, failures)
			continue
		}
		c.noteSuccess()
		c.removePending(entry.consumer)
		c.deliver(entry.consumer, values)
	}
}

func (c *ResolutionCoordinator) resolveGroup(ctx context.Context, group config.SecretConsumer) (map[string]string, error) {
	values := make(map[string]string, len(group.References))
	for _, name := range group.References {
		if value, ok := c.lookup(name); ok {
			values[name] = value
			continue
		}
		if c.provider == nil {
			values[name] = ""
			continue
		}
		callCtx, cancel := context.WithTimeout(ctx, c.tuning.CallTimeout)
		result, err := c.provider.Get(callCtx, name)
		cancel()
		if err != nil {
			c.recordPresenceError(name, err)
			clear(values)
			return nil, err
		}
		c.recordPresence(name, result.Present)
		if result.Present {
			values[name] = result.Value
		} else {
			values[name] = ""
			if group.Kind == config.ConsumerBackend {
				slog.Warn("declaration references a variable the daemon environment does not hold", "backend", group.Name, "variable", name)
			}
		}
	}
	return values, nil
}

func (c *ResolutionCoordinator) deliver(group config.SecretConsumer, values map[string]string) {
	if c.resolved != nil {
		c.resolved(ResolvedConsumer{Consumer: group, Values: values})
	}
	clear(values)
}

func (c *ResolutionCoordinator) environmentOnly(group config.SecretConsumer) bool {
	for _, name := range group.References {
		if _, ok := c.lookup(name); !ok {
			return c.provider == nil
		}
	}
	return true
}

func (c *ResolutionCoordinator) environmentValues(group config.SecretConsumer) map[string]string {
	values := make(map[string]string, len(group.References))
	for _, name := range group.References {
		values[name], _ = c.lookup(name)
	}
	return values
}

func (c *ResolutionCoordinator) blockedHealth() (Condition, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.health == nil {
		return "", false
	}
	return c.health.condition, true
}

func (c *ResolutionCoordinator) noteFailure(group config.SecretConsumer, err error) (Condition, time.Duration, int) {
	condition, ok := ConditionOf(err)
	if !ok {
		condition = ConditionUnexpected
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if condition == ConditionBusy || condition == ConditionLockContended {
		delay := c.busyBackoff
		c.busyBackoff = minDuration(c.busyBackoff*2, c.tuning.BusyBackoffMax)
		c.busyUntil = time.Now().Add(delay)
		c.busyCondition = condition
		return condition, delay, 0
	}
	c.busyBackoff = c.tuning.BusyBackoffBase
	if IsProviderHealthError(err) {
		failures := 1
		if c.health != nil {
			failures = c.health.failures + 1
		}
		delay := exponentialDelay(c.tuning.FailureBackoff, c.tuning.FailureBackoffMax, failures)
		c.health = &providerHealth{
			condition: condition,
			nextAt:    time.Now().Add(delay),
			suspended: condition == ConditionInteraction,
			failures:  failures,
		}
		return condition, delay, 0
	}
	c.health = nil
	failures := 1
	if entry, ok := c.pending[consumerKey(group)]; ok {
		failures = entry.failures + 1
	}
	return condition, exponentialDelay(c.tuning.FailureBackoff, c.tuning.FailureBackoffMax, failures), failures
}

func (c *ResolutionCoordinator) noteSuccess() {
	c.mu.Lock()
	c.health = nil
	c.busyBackoff = c.tuning.BusyBackoffBase
	c.busyUntil = time.Time{}
	c.busyCondition = ""
	c.mu.Unlock()
}

func (c *ResolutionCoordinator) failureDelay() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.health != nil {
		return maxDuration(time.Until(c.health.nextAt), 0)
	}
	failures := 1
	delay := exponentialDelay(c.tuning.FailureBackoff, c.tuning.FailureBackoffMax, failures)
	c.health = &providerHealth{condition: ConditionTimedOut, nextAt: time.Now().Add(delay), failures: failures}
	return delay
}

func (c *ResolutionCoordinator) currentFailureDelay() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.health == nil {
		return c.tuning.FailureBackoff
	}
	return time.Until(c.health.nextAt)
}

func (c *ResolutionCoordinator) enqueue(group config.SecretConsumer, condition Condition, delay time.Duration, failures int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := consumerKey(group)
	if entry, ok := c.pending[key]; ok {
		entry.condition = condition
		entry.nextAt = time.Now().Add(maxDuration(delay, 0))
		entry.failures = failures
		return
	}
	c.pending[key] = &pendingResolution{consumer: group, condition: condition, nextAt: time.Now().Add(maxDuration(delay, 0)), failures: failures}
	c.order = append(c.order, key)
}

func (c *ResolutionCoordinator) reschedule(group config.SecretConsumer, condition Condition, delay time.Duration, failures int) {
	c.enqueue(group, condition, delay, failures)
}

func (c *ResolutionCoordinator) removePending(group config.SecretConsumer) {
	c.mu.Lock()
	delete(c.pending, consumerKey(group))
	c.mu.Unlock()
}

func (c *ResolutionCoordinator) nextPending() (pendingResolution, time.Duration, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.health != nil && c.health.suspended {
		return pendingResolution{}, 0, false
	}
	var next *pendingResolution
	for _, key := range c.order {
		entry, ok := c.pending[key]
		if !ok {
			continue
		}
		if next == nil || entry.nextAt.Before(next.nextAt) {
			next = entry
		}
	}
	if next == nil {
		return pendingResolution{}, 0, false
	}
	nextAt := next.nextAt
	if c.health != nil && c.health.nextAt.After(nextAt) {
		nextAt = c.health.nextAt
	}
	return *next, maxDuration(time.Until(nextAt), 0), true
}

func (c *ResolutionCoordinator) signal() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

func consumerKey(group config.SecretConsumer) string {
	return string(group.Kind) + "\x00" + group.Name
}

func exponentialDelay(base, cap time.Duration, failures int) time.Duration {
	delay := base
	for i := 1; i < failures && delay < cap; i++ {
		delay = minDuration(delay*2, cap)
	}
	return delay
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}
