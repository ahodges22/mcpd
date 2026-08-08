package secretstore

import (
	"context"
	"time"

	"github.com/ahodges22/mcpd/internal/config"
)

type EffectiveSource string

const (
	EffectiveSourceEnvironment EffectiveSource = "environment; provider not checked"
	EffectiveSourceProvider    EffectiveSource = "provider-present"
	EffectiveSourceAbsent      EffectiveSource = "absent"
	EffectiveSourceCondition   EffectiveSource = "provider-condition"
)

type ConsumerIdentity struct {
	Kind config.ConsumerKind `json:"kind"`
	Name string              `json:"name"`
}

type SecretStatus struct {
	Name      string             `json:"name"`
	Consumers []ConsumerIdentity `json:"consumers"`
	Source    EffectiveSource    `json:"source"`
	Condition Condition          `json:"condition,omitempty"`
}

type presenceEntry struct {
	source    EffectiveSource
	condition Condition
	checkedAt time.Time
	retryAt   time.Time
	failures  int
}

func (c *ResolutionCoordinator) Status(ctx context.Context) []SecretStatus {
	c.statusMu.Lock()
	defer c.statusMu.Unlock()
	return c.status(ctx)
}

func (c *ResolutionCoordinator) status(ctx context.Context) []SecretStatus {
	sweepCtx, cancelSweep := context.WithTimeout(ctx, c.tuning.StatusBudget)
	defer cancelSweep()
	out := make([]SecretStatus, 0, len(c.referenceNames))
	for _, name := range c.referenceNames {
		status := SecretStatus{
			Name:      name,
			Consumers: append([]ConsumerIdentity(nil), c.references[name]...),
		}
		if _, ok := c.lookup(name); ok {
			status.Source = EffectiveSourceEnvironment
			out = append(out, status)
			continue
		}
		if c.provider == nil {
			status.Source = EffectiveSourceAbsent
			out = append(out, status)
			continue
		}
		if cached, ok := c.cachedPresence(name); ok {
			status.Source = cached.source
			status.Condition = cached.condition
			out = append(out, status)
			continue
		}
		if sweepCtx.Err() != nil {
			status.Source = EffectiveSourceCondition
			status.Condition = ConditionTimedOut
			out = append(out, status)
			continue
		}
		if condition, blocked := c.statusBlocked(); blocked {
			status.Source = EffectiveSourceCondition
			status.Condition = condition
			out = append(out, status)
			continue
		}

		callCtx, cancel := context.WithTimeout(sweepCtx, c.tuning.CallTimeout)
		result, err := c.provider.Get(callCtx, name)
		truncated := sweepCtx.Err() != nil
		cancel()
		if err != nil {
			status.Source = EffectiveSourceCondition
			if truncated {
				condition, ok := ConditionOf(err)
				if !ok {
					condition = ConditionTimedOut
				}
				status.Condition = condition
				c.recordStatusPacing(name, condition)
			} else {
				status.Condition = c.recordStatusError(name, err)
			}
			out = append(out, status)
			continue
		}
		c.noteSuccess()
		c.signal()
		c.recordPresence(name, result.Present)
		if result.Present {
			status.Source = EffectiveSourceProvider
		} else {
			status.Source = EffectiveSourceAbsent
		}
		out = append(out, status)
	}
	return out
}

func (c *ResolutionCoordinator) recordStatusPacing(name string, condition Condition) {
	now := time.Now()
	c.mu.Lock()
	c.presence[name] = presenceEntry{
		source:    EffectiveSourceCondition,
		condition: condition,
		checkedAt: now,
		retryAt:   now.Add(c.tuning.FailureBackoff),
	}
	c.mu.Unlock()
}

func (c *ResolutionCoordinator) RefreshStatus(ctx context.Context) []SecretStatus {
	c.statusMu.Lock()
	defer c.statusMu.Unlock()
	c.invalidatePresence(nil)
	return c.status(ctx)
}

func (c *ResolutionCoordinator) InvalidatePresence(names ...string) {
	c.statusMu.Lock()
	defer c.statusMu.Unlock()
	c.invalidatePresence(names)
}

func (c *ResolutionCoordinator) invalidatePresence(names []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(names) == 0 {
		clear(c.presence)
		return
	}
	for _, name := range names {
		delete(c.presence, name)
	}
}

func (c *ResolutionCoordinator) cachedPresence(name string) (presenceEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.presence[name]
	if !ok {
		return presenceEntry{}, false
	}
	now := time.Now()
	if entry.source == EffectiveSourceCondition {
		return entry, now.Before(entry.retryAt)
	}
	return entry, now.Before(entry.checkedAt.Add(c.tuning.PresenceTTL))
}

func (c *ResolutionCoordinator) recordPresence(name string, present bool) {
	source := EffectiveSourceAbsent
	if present {
		source = EffectiveSourceProvider
	}
	c.mu.Lock()
	c.presence[name] = presenceEntry{source: source, checkedAt: time.Now()}
	c.mu.Unlock()
}

func (c *ResolutionCoordinator) recordPresenceError(name string, err error) {
	condition, ok := ConditionOf(err)
	if !ok {
		condition = ConditionUnexpected
	}
	if condition == ConditionBusy || condition == ConditionLockContended {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	failures := 1
	if entry, exists := c.presence[name]; exists && entry.source == EffectiveSourceCondition {
		failures = entry.failures + 1
	}
	now := time.Now()
	delay := exponentialDelay(c.tuning.FailureBackoff, c.tuning.FailureBackoffMax, failures)
	c.presence[name] = presenceEntry{
		source:    EffectiveSourceCondition,
		condition: condition,
		checkedAt: now,
		retryAt:   now.Add(delay),
		failures:  failures,
	}
}

func (c *ResolutionCoordinator) statusBlocked() (Condition, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if now.Before(c.busyUntil) {
		condition := c.busyCondition
		if condition == "" {
			condition = ConditionBusy
		}
		return condition, true
	}
	if c.health == nil {
		return "", false
	}
	if c.health.suspended || now.Before(c.health.nextAt) {
		return c.health.condition, true
	}
	return "", false
}

func (c *ResolutionCoordinator) recordStatusError(name string, err error) Condition {
	condition, ok := ConditionOf(err)
	if !ok {
		condition = ConditionUnexpected
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if condition == ConditionBusy || condition == ConditionLockContended {
		delay := c.busyBackoff
		c.busyBackoff = minDuration(c.busyBackoff*2, c.tuning.BusyBackoffMax)
		c.busyUntil = now.Add(delay)
		c.busyCondition = condition
		return condition
	}
	c.busyBackoff = c.tuning.BusyBackoffBase
	c.busyUntil = time.Time{}
	c.busyCondition = ""
	if IsProviderHealthError(err) {
		failures := 1
		if c.health != nil {
			failures = c.health.failures + 1
		}
		delay := exponentialDelay(c.tuning.FailureBackoff, c.tuning.FailureBackoffMax, failures)
		c.health = &providerHealth{
			condition: condition,
			nextAt:    now.Add(delay),
			suspended: condition == ConditionInteraction,
			failures:  failures,
		}
	} else {
		c.health = nil
	}
	failures := 1
	if entry, exists := c.presence[name]; exists && entry.source == EffectiveSourceCondition {
		failures = entry.failures + 1
	}
	delay := exponentialDelay(c.tuning.FailureBackoff, c.tuning.FailureBackoffMax, failures)
	c.presence[name] = presenceEntry{
		source:    EffectiveSourceCondition,
		condition: condition,
		checkedAt: now,
		retryAt:   now.Add(delay),
		failures:  failures,
	}
	return condition
}
