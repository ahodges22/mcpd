package secretstore

import (
	"context"
	"slices"
	"sort"

	"github.com/ahodges22/mcpd/internal/config"
)

type MutationHooks struct {
	Reset   func(config.SecretConsumer) bool
	Pending func(PendingConsumer)
}

func (c *ResolutionCoordinator) SetMutationHooks(hooks MutationHooks) {
	c.mutationHooks = hooks
}

func (c *ResolutionCoordinator) Set(ctx context.Context, name, value string) ([]ConsumerIdentity, error) {
	c.mutationMu.Lock()
	defer c.mutationMu.Unlock()
	if c.provider == nil {
		return nil, &Error{Operation: OperationSet, Provider: "configured", Name: name, Condition: ConditionUnavailable}
	}
	if err := c.provider.Set(ctx, name, value); err != nil {
		return nil, err
	}
	return c.refreshAfterMutation(ctx, name), nil
}

func (c *ResolutionCoordinator) Delete(ctx context.Context, name string) ([]ConsumerIdentity, error) {
	c.mutationMu.Lock()
	defer c.mutationMu.Unlock()
	if c.provider == nil {
		return nil, &Error{Operation: OperationDelete, Provider: "configured", Name: name, Condition: ConditionUnavailable}
	}
	if err := c.provider.Delete(ctx, name); err != nil {
		return nil, err
	}
	return c.refreshAfterMutation(ctx, name), nil
}

func (c *ResolutionCoordinator) refreshAfterMutation(ctx context.Context, name string) []ConsumerIdentity {
	c.InvalidatePresence(name)
	c.noteSuccess()
	return c.refreshConsumers(ctx, name)
}

func (c *ResolutionCoordinator) RefreshConsumers(ctx context.Context, name string) []ConsumerIdentity {
	c.mutationMu.Lock()
	defer c.mutationMu.Unlock()
	c.InvalidatePresence(name)
	return c.refreshConsumers(ctx, name)
}

func (c *ResolutionCoordinator) refreshConsumers(ctx context.Context, name string) []ConsumerIdentity {
	groups, dependents := c.dependentGroups(name)
	for _, group := range groups {
		if c.mutationHooks.Reset != nil && !c.mutationHooks.Reset(group) {
			continue
		}
		values, err := c.ResolveConsumer(ctx, group)
		if err != nil {
			condition, ok := ConditionOf(err)
			if !ok {
				condition = ConditionUnexpected
			}
			if c.mutationHooks.Pending != nil {
				c.mutationHooks.Pending(PendingConsumer{Consumer: group, Condition: condition})
			}
			continue
		}
		c.deliver(group, values)
	}
	c.signal()
	return dependents
}

func (c *ResolutionCoordinator) Dependents(name string) []ConsumerIdentity {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]ConsumerIdentity(nil), c.references[name]...)
}

func (c *ResolutionCoordinator) dependentGroups(name string) ([]config.SecretConsumer, []ConsumerIdentity) {
	c.mu.Lock()
	defer c.mu.Unlock()
	groups := make([]config.SecretConsumer, 0, len(c.references[name]))
	for _, group := range c.groups {
		if slices.Contains(group.References, name) {
			groups = append(groups, cloneConsumer(group))
		}
	}
	return groups, append([]ConsumerIdentity(nil), c.references[name]...)
}

func (c *ResolutionCoordinator) UpdateBackend(name string, spec *config.Backend) {
	c.mu.Lock()
	defer c.mu.Unlock()
	groups := make([]config.SecretConsumer, 0, len(c.groups)+1)
	for _, group := range c.groups {
		if group.Kind != config.ConsumerBackend || group.Name != name {
			groups = append(groups, cloneConsumer(group))
		}
	}
	if spec != nil {
		if references := spec.SecretReferences(); len(references) > 0 {
			groups = append(groups, config.SecretConsumer{Kind: config.ConsumerBackend, Name: name, References: references})
		}
	}
	c.replaceIndexLocked(groups)
	delete(c.pending, consumerKey(config.SecretConsumer{Kind: config.ConsumerBackend, Name: name}))
	c.compactOrderLocked()
}

func (c *ResolutionCoordinator) Reindex(cfg *config.Config) {
	var groups []config.SecretConsumer
	if cfg != nil {
		groups = cfg.SecretConsumers()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	nextGroups, references, referenceNames := buildConsumerIndex(groups)
	valid := make(map[string]config.SecretConsumer, len(nextGroups))
	for _, group := range nextGroups {
		valid[consumerKey(group)] = group
	}
	for key, pending := range c.pending {
		group, ok := valid[key]
		if !ok || !slices.Equal(group.References, pending.consumer.References) {
			delete(c.pending, key)
		}
	}
	c.groups, c.references, c.referenceNames = nextGroups, references, referenceNames
	c.compactOrderLocked()
	c.filterPresenceLocked()
}

func (c *ResolutionCoordinator) referenceSnapshot() ([]string, map[string][]ConsumerIdentity) {
	c.mu.Lock()
	defer c.mu.Unlock()
	names := append([]string(nil), c.referenceNames...)
	references := make(map[string][]ConsumerIdentity, len(c.references))
	for name, consumers := range c.references {
		references[name] = append([]ConsumerIdentity(nil), consumers...)
	}
	return names, references
}

func (c *ResolutionCoordinator) replaceIndexLocked(groups []config.SecretConsumer) {
	c.groups, c.references, c.referenceNames = buildConsumerIndex(groups)
	c.filterPresenceLocked()
}

func (c *ResolutionCoordinator) filterPresenceLocked() {
	for name := range c.presence {
		if _, configured := c.references[name]; !configured {
			delete(c.presence, name)
		}
	}
}

func (c *ResolutionCoordinator) compactOrderLocked() {
	seen := map[string]struct{}{}
	c.order = slices.DeleteFunc(c.order, func(key string) bool {
		if _, pending := c.pending[key]; !pending {
			return true
		}
		if _, duplicate := seen[key]; duplicate {
			return true
		}
		seen[key] = struct{}{}
		return false
	})
}

func buildConsumerIndex(groups []config.SecretConsumer) ([]config.SecretConsumer, map[string][]ConsumerIdentity, []string) {
	cloned := make([]config.SecretConsumer, 0, len(groups))
	references := map[string][]ConsumerIdentity{}
	for _, group := range groups {
		group = cloneConsumer(group)
		cloned = append(cloned, group)
		consumer := ConsumerIdentity{Kind: group.Kind, Name: group.Name}
		for _, name := range group.References {
			references[name] = append(references[name], consumer)
		}
	}
	sort.Slice(cloned, func(i, j int) bool {
		if cloned[i].Kind != cloned[j].Kind {
			return cloned[i].Kind < cloned[j].Kind
		}
		return cloned[i].Name < cloned[j].Name
	})
	referenceNames := make([]string, 0, len(references))
	for name := range references {
		referenceNames = append(referenceNames, name)
	}
	sort.Strings(referenceNames)
	return cloned, references, referenceNames
}

func cloneConsumer(consumer config.SecretConsumer) config.SecretConsumer {
	consumer.References = append([]string(nil), consumer.References...)
	return consumer
}
