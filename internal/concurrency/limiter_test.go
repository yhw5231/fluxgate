package concurrency

import (
	"context"
	"testing"
	"time"

	"github.com/yhw5231/fluxgate/internal/domain"
)

// keyChannel is one line presenting one key of one upstream, with the concurrency
// limit the operator configured on that upstream.
func keyChannel(siteID int64, lineID, key string, limit int) domain.Channel {
	return domain.Channel{ID: lineID, SiteID: siteID, APIKey: key, KeyConcurrency: limit}
}

func TestLimiterServesOneRequestAtATimePerKey(t *testing.T) {
	limiter := NewLimiter()
	channel := keyChannel(1, "line", "secret", 1)

	release, ok := limiter.Acquire(channel)
	if !ok {
		t.Fatal("Acquire() = false while the key was free")
	}
	if !limiter.Busy(channel) {
		t.Fatal("Busy() = false while the key carries a request")
	}
	if got := limiter.InFlight(channel); got != 1 {
		t.Fatalf("InFlight() = %d, want 1", got)
	}
	if _, ok := limiter.Acquire(channel); ok {
		t.Fatal("Acquire() = true while the key is at its limit of 1")
	}

	release()
	if limiter.Busy(channel) {
		t.Fatal("Busy() = true after the request was released")
	}
	if _, ok := limiter.Acquire(channel); !ok {
		t.Fatal("Acquire() = false after the slot was handed back")
	}
}

func TestLimiterCountsUpToTheConfiguredLimit(t *testing.T) {
	limiter := NewLimiter()
	channel := keyChannel(1, "line", "secret", 3)

	releases := make([]func(), 0, 3)
	for index := 0; index < 3; index++ {
		release, ok := limiter.Acquire(channel)
		if !ok {
			t.Fatalf("Acquire() = false on request %d of a limit of 3", index+1)
		}
		releases = append(releases, release)
	}
	if _, ok := limiter.Acquire(channel); ok {
		t.Fatal("Acquire() = true beyond the configured limit")
	}

	// Releasing one request makes room for exactly one more.
	releases[0]()
	if _, ok := limiter.Acquire(channel); !ok {
		t.Fatal("Acquire() = false after one of three slots was handed back")
	}
	if _, ok := limiter.Acquire(channel); ok {
		t.Fatal("Acquire() = true while the key is back at its limit")
	}
}

// A key is an account: every line presenting it draws from the same allowance,
// whichever route it is on. Two upstreams that happen to use one key string are
// two accounts, and are counted separately.
func TestLimiterCountsAKeyAcrossItsLines(t *testing.T) {
	limiter := NewLimiter()
	first := keyChannel(1, "line-a", "secret", 1)
	second := keyChannel(1, "line-b", "secret", 1)
	otherSite := keyChannel(2, "line-c", "secret", 1)

	release, ok := limiter.Acquire(first)
	if !ok {
		t.Fatal("Acquire() = false on a free key")
	}
	if !limiter.Busy(second) {
		t.Fatal("Busy() = false for another line presenting the same key")
	}
	if limiter.Busy(otherSite) {
		t.Fatal("Busy() = true for another upstream using the same key string")
	}
	release()
	if limiter.Busy(second) {
		t.Fatal("Busy() = true after the shared slot was handed back")
	}
}

// A line whose upstream sets no concurrency is not limited at all, which is what
// every upstream did before the setting existed.
func TestLimiterLeavesAnUnlimitedKeyAlone(t *testing.T) {
	limiter := NewLimiter()
	channel := keyChannel(1, "line", "secret", 0)

	for index := 0; index < 100; index++ {
		release, ok := limiter.Acquire(channel)
		if !ok {
			t.Fatalf("Acquire() = false on request %d of an unlimited key", index+1)
		}
		if limiter.Busy(channel) {
			t.Fatal("Busy() = true for an unlimited key")
		}
		if got := limiter.InFlight(channel); got != 0 {
			t.Fatalf("InFlight() = %d for an unlimited key, want 0", got)
		}
		release()
	}
}

func TestLimiterWaitReturnsWhenASlotIsHandedBack(t *testing.T) {
	limiter := NewLimiter()
	channel := keyChannel(1, "line", "secret", 1)
	release, ok := limiter.Acquire(channel)
	if !ok {
		t.Fatal("Acquire() = false on a free key")
	}

	wake := limiter.Wake()
	done := make(chan error, 1)
	go func() { done <- limiter.Wait(context.Background(), wake) }()
	select {
	case <-done:
		t.Fatal("Wait() returned while the key was still at its limit")
	case <-time.After(20 * time.Millisecond):
	}

	release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Wait() error = %v, want nil after a release", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Wait() did not return after a slot was handed back")
	}
}

// A waiter that took its wakeup before the slot was handed back is woken by that
// release rather than waiting for the next one, which may never come.
func TestLimiterWakeTakenBeforeTheReleaseIsNotMissed(t *testing.T) {
	limiter := NewLimiter()
	channel := keyChannel(1, "line", "secret", 1)
	release, ok := limiter.Acquire(channel)
	if !ok {
		t.Fatal("Acquire() = false on a free key")
	}

	// The order a waiting request follows: take the wakeup, ask whether the key is
	// busy, and only then wait. The slot is handed back while it is asking.
	wake := limiter.Wake()
	busy := limiter.Busy(channel)
	release()
	if !busy {
		t.Fatal("Busy() = false while the key was still taken")
	}
	if err := limiter.Wait(context.Background(), wake); err != nil {
		t.Fatalf("Wait() error = %v, want nil because the release closed the taken wakeup", err)
	}
	if limiter.Busy(channel) {
		t.Fatal("Busy() = true after the only request was released")
	}
}

// A caller that hung up does not leave a request queued behind a key.
func TestLimiterWaitEndsWithItsContext(t *testing.T) {
	limiter := NewLimiter()
	channel := keyChannel(1, "line", "secret", 1)
	release, ok := limiter.Acquire(channel)
	if !ok {
		t.Fatal("Acquire() = false on a free key")
	}
	defer release()

	ctx, cancel := context.WithCancel(context.Background())
	waited := make(chan error, 1)
	wake := limiter.Wake()
	go func() { waited <- limiter.Wait(ctx, wake) }()
	cancel()

	select {
	case err := <-waited:
		if err == nil {
			t.Fatal("Wait() error = nil, want the context's error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Wait() did not return after its context was canceled")
	}
}

// The release a request carries is idempotent, because the end of an answer can
// be reached by more than one path — a fully read body and a canceled one.
func TestLimiterReleaseIsIdempotent(t *testing.T) {
	limiter := NewLimiter()
	channel := keyChannel(1, "line", "secret", 1)
	release, ok := limiter.Acquire(channel)
	if !ok {
		t.Fatal("Acquire() = false on a free key")
	}
	release()
	release()

	if got := limiter.InFlight(channel); got != 0 {
		t.Fatalf("InFlight() = %d after two releases, want 0", got)
	}
	first, ok := limiter.Acquire(channel)
	if !ok {
		t.Fatal("Acquire() = false after a double release, which is a single slot")
	}
	first()
}
