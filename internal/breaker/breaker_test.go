package breaker

import (
	"sync"
	"testing"
	"time"

	"github.com/yhw5231/fluxgate/internal/domain"
)

func newTestBreaker(mode Mode, now *time.Time) *Breaker {
	return &Breaker{
		Policy: Policy{
			Mode:         mode,
			Threshold:    2,
			BaseCooldown: time.Minute,
			MaxCooldown:  4 * time.Minute,
		},
		Store: NewMemoryStore(),
		Now:   func() time.Time { return *now },
	}
}

func retryableFailure(channelID, keyID, model string) domain.Failure {
	return domain.Failure{
		Attempt: domain.Attempt{
			ChannelID: channelID,
			KeyID:     keyID,
			Model:     model,
		},
		Retryable: true,
	}
}

func TestBreakerChannelScopeThresholdExpiryAndRecovery(t *testing.T) {
	now := time.Date(2026, time.September, 18, 12, 0, 0, 0, time.UTC)
	breaker := newTestBreaker(ModeCooldown, &now)
	channel := domain.Channel{ID: "channel-a", APIKey: "key-a"}
	failure := retryableFailure(channel.ID, channel.APIKey, "model-a")

	breaker.RecordFailure(failure)
	if breaker.IsBlocked(channel, "model-a") {
		t.Fatal("channel blocked before consecutive-failure threshold")
	}

	breaker.RecordFailure(failure)
	if !breaker.IsBlocked(channel, "model-a") {
		t.Fatal("channel not blocked at consecutive-failure threshold")
	}
	if !breaker.IsBlocked(channel, "different-model") {
		t.Fatal("channel cooldown did not apply across models")
	}

	now = now.Add(time.Minute)
	if breaker.IsBlocked(channel, "model-a") {
		t.Fatal("channel remained blocked at cooldown expiry")
	}

	breaker.RecordFailure(failure)
	breaker.RecordSuccess(failure.Attempt)
	state, ok := breaker.Store.Load(Scope{ChannelID: channel.ID})
	if !ok {
		t.Fatal("channel state missing after success")
	}
	if state != (State{}) {
		t.Fatalf("state after success = %#v, want zero state", state)
	}
}

func TestBreakerDisableModeIsPermanentUntilRecovery(t *testing.T) {
	now := time.Date(2026, time.September, 18, 12, 0, 0, 0, time.UTC)
	breaker := newTestBreaker(ModeDisable, &now)
	channel := domain.Channel{ID: "channel-a", APIKey: "key-a"}
	failure := retryableFailure(channel.ID, channel.APIKey, "model-a")

	breaker.RecordFailure(failure)
	breaker.RecordFailure(failure)
	if !breaker.IsBlocked(channel, "model-a") {
		t.Fatal("disabled channel is not blocked")
	}

	now = now.Add(24 * time.Hour)
	if !breaker.IsBlocked(channel, "model-a") {
		t.Fatal("disabled channel became eligible as time advanced")
	}

	breaker.RecordSuccess(failure.Attempt)
	if breaker.IsBlocked(channel, "model-a") {
		t.Fatal("successful recovery did not clear disabled state")
	}
}

func TestBreakerKeyScopeAppliesAcrossChannels(t *testing.T) {
	now := time.Date(2026, time.September, 18, 12, 0, 0, 0, time.UTC)
	breaker := newTestBreaker(ModeKeyCooldown, &now)
	first := domain.Channel{ID: "channel-a", APIKey: "shared-key"}
	second := domain.Channel{ID: "channel-b", APIKey: "shared-key"}
	other := domain.Channel{ID: "channel-c", APIKey: "other-key"}
	failure := retryableFailure(first.ID, first.APIKey, "model-a")

	breaker.RecordFailure(failure)
	breaker.RecordFailure(failure)
	if !breaker.IsBlocked(second, "model-b") {
		t.Fatal("shared key was not blocked across channels and models")
	}
	if breaker.IsBlocked(other, "model-a") {
		t.Fatal("unrelated key was blocked")
	}
}

func TestBreakerKeyModelScopeIsIsolatedByModel(t *testing.T) {
	now := time.Date(2026, time.September, 18, 12, 0, 0, 0, time.UTC)
	breaker := newTestBreaker(ModeKeyModelCooldown, &now)
	first := domain.Channel{ID: "channel-a", APIKey: "shared-key"}
	second := domain.Channel{ID: "channel-b", APIKey: "shared-key"}
	failure := retryableFailure(first.ID, first.APIKey, "model-a")

	breaker.RecordFailure(failure)
	breaker.RecordFailure(failure)
	if !breaker.IsBlocked(second, "model-a") {
		t.Fatal("key and model scope was not shared across channels")
	}
	if breaker.IsBlocked(second, "model-b") {
		t.Fatal("different model was blocked for key and model scope")
	}
}

func TestBreakerCooldownIncreasesExponentially(t *testing.T) {
	now := time.Date(2026, time.September, 18, 12, 0, 0, 0, time.UTC)
	breaker := newTestBreaker(ModeCooldown, &now)
	channel := domain.Channel{ID: "channel-a", APIKey: "key-a"}
	failure := retryableFailure(channel.ID, channel.APIKey, "model-a")

	breaker.RecordFailure(failure)
	breaker.RecordFailure(failure)
	first, _ := breaker.Store.Load(Scope{ChannelID: channel.ID})
	if got := first.BlockedUntil.Sub(now); got != time.Minute {
		t.Fatalf("first cooldown = %s, want 1m", got)
	}

	now = first.BlockedUntil
	breaker.RecordFailure(failure)
	breaker.RecordFailure(failure)
	second, _ := breaker.Store.Load(Scope{ChannelID: channel.ID})
	if got := second.BlockedUntil.Sub(now); got != 2*time.Minute {
		t.Fatalf("second cooldown = %s, want 2m", got)
	}

	now = second.BlockedUntil
	breaker.RecordFailure(failure)
	breaker.RecordFailure(failure)
	third, _ := breaker.Store.Load(Scope{ChannelID: channel.ID})
	if got := third.BlockedUntil.Sub(now); got != 4*time.Minute {
		t.Fatalf("third cooldown = %s, want capped 4m", got)
	}
}

func TestBreakerIgnoresNonRetryableFailure(t *testing.T) {
	now := time.Date(2026, time.September, 18, 12, 0, 0, 0, time.UTC)
	breaker := newTestBreaker(ModeCooldown, &now)
	channel := domain.Channel{ID: "channel-a", APIKey: "key-a"}
	failure := retryableFailure(channel.ID, channel.APIKey, "model-a")
	failure.Retryable = false

	breaker.RecordFailure(failure)
	breaker.RecordFailure(failure)
	if breaker.IsBlocked(channel, "model-a") {
		t.Fatal("non-retryable failures opened circuit")
	}
}

func TestBreakerChannelPoliciesCoexistAndRecoverIndependently(t *testing.T) {
	now := time.Date(2026, time.September, 18, 12, 0, 0, 0, time.UTC)
	breaker := newTestBreaker(ModeDisable, &now)

	channelScoped := domain.Channel{ID: "channel-scope", APIKey: "shared-key", BreakerMode: string(ModeCooldown)}
	keyScoped := domain.Channel{ID: "key-scope", APIKey: "shared-key", BreakerMode: string(ModeKeyCooldown)}
	modelScoped := domain.Channel{ID: "model-scope", APIKey: "shared-key", BreakerMode: string(ModeKeyModelCooldown)}

	channelFailure := retryableFailure(channelScoped.ID, channelScoped.APIKey, "model-a")
	channelFailure.Attempt.BreakerMode = channelScoped.BreakerMode
	keyFailure := retryableFailure(keyScoped.ID, keyScoped.APIKey, "model-a")
	keyFailure.Attempt.BreakerMode = keyScoped.BreakerMode
	modelFailure := retryableFailure(modelScoped.ID, modelScoped.APIKey, "model-b")
	modelFailure.Attempt.BreakerMode = modelScoped.BreakerMode

	for _, failure := range []domain.Failure{channelFailure, keyFailure, modelFailure} {
		breaker.RecordFailure(failure)
		breaker.RecordFailure(failure)
	}

	if !breaker.IsBlocked(channelScoped, "different-model") {
		t.Fatal("channel-scoped circuit was not blocked")
	}
	if !breaker.IsBlocked(keyScoped, "different-model") {
		t.Fatal("key-scoped circuit was not blocked across models")
	}
	if !breaker.IsBlocked(modelScoped, "model-b") {
		t.Fatal("key-and-model-scoped circuit was not blocked")
	}
	if breaker.IsBlocked(modelScoped, "model-c") {
		t.Fatal("key-and-model-scoped circuit blocked an unrelated model")
	}

	breaker.RecordSuccess(modelFailure.Attempt)
	if breaker.IsBlocked(modelScoped, "model-b") {
		t.Fatal("success did not recover the key-and-model-scoped circuit")
	}
	if !breaker.IsBlocked(channelScoped, "model-a") {
		t.Fatal("recovering one scope cleared the independent channel scope")
	}
	if !breaker.IsBlocked(keyScoped, "model-a") {
		t.Fatal("recovering one scope cleared the independent key scope")
	}

	if _, ok := breaker.Store.Load(Scope{ChannelID: channelScoped.ID}); !ok {
		t.Fatal("channel-scoped state does not coexist in the store")
	}
	if _, ok := breaker.Store.Load(Scope{KeyID: keyScoped.APIKey}); !ok {
		t.Fatal("key-scoped state does not coexist in the store")
	}
	if state, ok := breaker.Store.Load(Scope{KeyID: modelScoped.APIKey, Model: "model-b"}); !ok || state != (State{}) {
		t.Fatalf("recovered key-and-model state = %#v, exists = %v", state, ok)
	}
}

func TestMemoryStoreConcurrentUpdates(t *testing.T) {
	store := NewMemoryStore()
	scope := Scope{ChannelID: "channel-a"}
	const workers = 100

	var wait sync.WaitGroup
	wait.Add(workers)
	for index := 0; index < workers; index++ {
		go func() {
			defer wait.Done()
			store.Update(scope, func(state State) State {
				state.ConsecutiveFailures++
				return state
			})
		}()
	}
	wait.Wait()

	state, ok := store.Load(scope)
	if !ok {
		t.Fatal("concurrent state missing")
	}
	if state.ConsecutiveFailures != workers {
		t.Fatalf("concurrent updates = %d, want %d", state.ConsecutiveFailures, workers)
	}
}
