package breaker

import (
	"sync"
	"time"

	"github.com/yhw5231/fluxgate/internal/domain"
)

// Mode controls the scope and duration of circuit-breaker blocking.
type Mode string

const (
	ModeCooldown         Mode = "cooldown"
	ModeDisable          Mode = "disable"
	ModeKeyCooldown      Mode = "key_cooldown"
	ModeKeyModelCooldown Mode = "key_model_cooldown"
)

// Policy configures failure threshold and exponential cooldown behavior.
type Policy struct {
	Mode         Mode
	Threshold    int
	BaseCooldown time.Duration
	MaxCooldown  time.Duration
}

// Scope identifies one independently tracked breaker circuit.
type Scope struct {
	ChannelID string
	KeyID     string
	Model     string
}

// State is the persisted state for one circuit.
type State struct {
	ConsecutiveFailures int
	CooldownLevel       int
	BlockedUntil        time.Time
	Disabled            bool
}

// Store separates breaker behavior from state persistence.
type Store interface {
	Load(scope Scope) (State, bool)
	Update(scope Scope, update func(State) State) State
}

// MemoryStore is a concurrent in-memory Store implementation.
type MemoryStore struct {
	mu     sync.RWMutex
	states map[Scope]State
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{states: make(map[Scope]State)}
}

func (s *MemoryStore) Load(scope Scope) (State, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	state, ok := s.states[scope]
	return state, ok
}

func (s *MemoryStore) Update(scope Scope, update func(State) State) State {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := update(s.states[scope])
	s.states[scope] = state
	return state
}

// Breaker implements runtime filtering and FailureObserver.
type Breaker struct {
	Policy Policy
	Store  Store
	Now    func() time.Time
}

func (b *Breaker) IsBlocked(channel domain.Channel, model string) bool {
	if b == nil || b.Store == nil {
		return false
	}
	state, ok := b.Store.Load(b.scope(channel, model))
	if !ok {
		return false
	}
	if state.Disabled {
		return true
	}
	now := time.Now
	if b.Now != nil {
		now = b.Now
	}
	return !state.BlockedUntil.IsZero() && now().Before(state.BlockedUntil)
}

func (b *Breaker) RecordFailure(failure domain.Failure) {
	if b == nil || b.Store == nil || !failure.Retryable {
		return
	}
	policy := normalizePolicy(b.policyForMode(modeOrDefault(failure.Attempt.BreakerMode, b.Policy.Mode)))
	scope := scopeForMode(policy.Mode, failure.Attempt.ChannelID, failure.Attempt.KeyID, failure.Attempt.Model)
	now := time.Now
	if b.Now != nil {
		now = b.Now
	}
	b.Store.Update(scope, func(state State) State {
		state.ConsecutiveFailures++
		if state.ConsecutiveFailures < policy.Threshold {
			return state
		}
		state.ConsecutiveFailures = 0
		if policy.Mode == ModeDisable {
			state.Disabled = true
			return state
		}
		state.CooldownLevel++
		state.BlockedUntil = now().Add(cooldown(policy, state.CooldownLevel))
		return state
	})
}

func (b *Breaker) RecordSuccess(attempt domain.Attempt) {
	if b == nil || b.Store == nil {
		return
	}
	mode := modeOrDefault(attempt.BreakerMode, b.Policy.Mode)
	b.Store.Update(scopeForMode(mode, attempt.ChannelID, attempt.KeyID, attempt.Model), func(State) State { return State{} })
}

func (b *Breaker) scope(channel domain.Channel, model string) Scope {
	mode := modeOrDefault(channel.BreakerMode, b.Policy.Mode)
	return scopeForMode(mode, channel.ID, channel.APIKey, model)
}

func (b *Breaker) policyForMode(mode Mode) Policy {
	policy := b.Policy
	policy.Mode = mode
	return policy
}

func modeOrDefault(configured string, fallback Mode) Mode {
	switch Mode(configured) {
	case ModeCooldown, ModeDisable, ModeKeyCooldown, ModeKeyModelCooldown:
		return Mode(configured)
	default:
		return fallback
	}
}

func scopeForMode(mode Mode, channelID, keyID, model string) Scope {
	switch mode {
	case ModeKeyCooldown:
		return Scope{KeyID: keyID}
	case ModeKeyModelCooldown:
		return Scope{KeyID: keyID, Model: model}
	default:
		return Scope{ChannelID: channelID}
	}
}

func normalizePolicy(policy Policy) Policy {
	if policy.Threshold <= 0 {
		policy.Threshold = 1
	}
	if policy.BaseCooldown <= 0 {
		policy.BaseCooldown = time.Second
	}
	if policy.MaxCooldown <= 0 {
		policy.MaxCooldown = policy.BaseCooldown
	}
	return policy
}

func cooldown(policy Policy, level int) time.Duration {
	delay := policy.BaseCooldown
	for index := 1; index < level; index++ {
		if delay >= policy.MaxCooldown/2 {
			return policy.MaxCooldown
		}
		delay *= 2
	}
	if delay > policy.MaxCooldown {
		return policy.MaxCooldown
	}
	return delay
}
