package store

import (
	"context"
	"sync"

	"github.com/yhw5231/fluxgate/internal/breaker"
)

// BreakerAdapter implements breaker.Store using SQLite persistence while
// retaining an in-memory snapshot for synchronous breaker decisions.
type BreakerAdapter struct {
	store  Store
	ctx    context.Context
	mu     sync.RWMutex
	states map[breaker.Scope]breaker.State
}

// NewBreakerAdapter restores persisted breaker state for use after restart.
func NewBreakerAdapter(ctx context.Context, persistent Store) (*BreakerAdapter, error) {
	states, err := persistent.LoadBreakerStates(ctx)
	if err != nil {
		return nil, err
	}
	return &BreakerAdapter{
		store:  persistent,
		ctx:    ctx,
		states: states,
	}, nil
}

// Load returns the current restored or persisted state for a breaker scope.
func (a *BreakerAdapter) Load(scope breaker.Scope) (breaker.State, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	state, ok := a.states[scope]
	return state, ok
}

// Update atomically applies and persists a breaker state transition. If the
// persistent update fails, the last known state is retained rather than
// allowing an unpersisted transition to diverge from restart behavior.
func (a *BreakerAdapter) Update(scope breaker.Scope, update func(breaker.State) breaker.State) breaker.State {
	a.mu.Lock()
	defer a.mu.Unlock()

	current := a.states[scope]
	state, err := a.store.UpdateBreakerState(a.ctx, scope, update)
	if err != nil {
		return current
	}
	a.states[scope] = state
	return state
}

// Snapshot returns a defensive copy of all breaker states for management views.
func (a *BreakerAdapter) Snapshot() map[breaker.Scope]breaker.State {
	a.mu.RLock()
	defer a.mu.RUnlock()

	states := make(map[breaker.Scope]breaker.State, len(a.states))
	for scope, state := range a.states {
		states[scope] = state
	}
	return states
}
