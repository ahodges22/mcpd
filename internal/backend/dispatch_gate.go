package backend

import (
	"context"

	"golang.org/x/sync/semaphore"
)

const dispatchGateCapacity int64 = 1 << 62

// dispatchGate is a writer-preferred, context-cancellable RW lock. A tool call
// holds one unit; a lifecycle transition takes the whole capacity. Weighted's
// waiter ordering prevents new calls from passing a queued transition.
type dispatchGate struct{ sem *semaphore.Weighted }

func newDispatchGate() dispatchGate {
	return dispatchGate{sem: semaphore.NewWeighted(dispatchGateCapacity)}
}

func (g dispatchGate) RLock() { _ = g.RLockContext(context.Background()) }
func (g dispatchGate) RLockContext(ctx context.Context) error {
	return g.sem.Acquire(ctx, 1)
}
func (g dispatchGate) RUnlock()       { g.sem.Release(1) }
func (g dispatchGate) TryRLock() bool { return g.sem.TryAcquire(1) }
func (g dispatchGate) Lock()          { _ = g.LockContext(context.Background()) }
func (g dispatchGate) LockContext(ctx context.Context) error {
	return g.sem.Acquire(ctx, dispatchGateCapacity)
}
func (g dispatchGate) Unlock() { g.sem.Release(dispatchGateCapacity) }
