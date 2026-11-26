// Package concurrency bounds how many requests may be in flight through one
// upstream key at a time.
//
// An operator sets a per-key concurrency on an upstream — a value of 1 means that
// key serves one request at a time — and the gateway enforces it here. The
// counting lives in this package so the two parties that need it work from the
// same numbers: the selector, which places a request on a key that has room, and
// the engine, which takes the slot before it dispatches and hands it back when the
// answer is over.
//
// A limit belongs to a credential rather than to a line. One upstream key is one
// account, and every line presenting it — the same key on several routes — shares
// the one allowance, which is what makes "one request at a time" mean the key and
// not one route of it.
package concurrency

import (
	"context"
	"sync"

	"github.com/yhw5231/fluxgate/internal/domain"
)

// Limiter counts the requests in flight through each upstream key.
//
// It is safe for concurrent use and its zero value is not usable: build one with
// NewLimiter.
type Limiter struct {
	mu       sync.Mutex
	inFlight map[string]int
	// released is closed and replaced every time a slot is handed back. A waiting
	// request observes the channel that is current when it starts waiting, so it
	// is woken by the next release instead of polling — and being woken is only a
	// hint: it re-checks the line it wants and waits again if another request took
	// the slot first.
	released chan struct{}
}

// NewLimiter returns a limiter with nothing in flight.
func NewLimiter() *Limiter {
	return &Limiter{
		inFlight: make(map[string]int),
		released: make(chan struct{}),
	}
}

// Busy reports whether the line's key is already at the limit the line carries.
// A line whose upstream sets no concurrency — and a limiter that was never built,
// which a test or an embedder may pass — is never busy.
func (l *Limiter) Busy(channel domain.Channel) bool {
	if l == nil || channel.KeyConcurrency <= 0 {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.inFlight[channel.CredentialKey()] >= channel.KeyConcurrency
}

// Acquire takes one slot of the line's key and answers the release to call when
// the request is over. It answers false when the key is at its limit, which is
// the race the selector cannot close: two requests can be chosen for one key
// between being chosen and being taken. The release is safe to call more than
// once and from any goroutine, because the body of an answer is closed wherever
// the answer itself ends.
func (l *Limiter) Acquire(channel domain.Channel) (func(), bool) {
	if l == nil || channel.KeyConcurrency <= 0 {
		return func() {}, true
	}
	key := channel.CredentialKey()
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.inFlight[key] >= channel.KeyConcurrency {
		return nil, false
	}
	l.inFlight[key]++
	var once sync.Once
	return func() {
		once.Do(func() { l.release(key) })
	}, true
}

// release hands one slot back and wakes whoever is waiting for one.
func (l *Limiter) release(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.inFlight[key] <= 1 {
		// The key holds nothing any more, so its counter goes with the slot: a
		// gateway that cycles through many keys does not accumulate an entry for
		// every credential it has ever used.
		delete(l.inFlight, key)
	} else {
		l.inFlight[key]--
	}
	close(l.released)
	l.released = make(chan struct{})
}

// Wake returns the channel that is closed when the next slot is handed back. A
// caller takes it before it asks whether a key is busy, so a release that happens
// while the question is being answered — between asking and waiting — is not
// missed: the channel it holds is closed by that release, and its wait returns at
// once instead of sleeping until some later release that may never come.
func (l *Limiter) Wake() <-chan struct{} {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.released
}

// Wait blocks until the channel Wake returned is closed, or the context ends. It
// is how a request whose every line is at its key's limit waits for room instead
// of being refused: the limit is a throttle, and a request that waited a moment
// is one the operator would rather serve than fail.
//
// The wait is bounded by the caller's own context, which ends when the client
// hangs up: a queue that outlives its client is work nobody is waiting for.
func (l *Limiter) Wait(ctx context.Context, wake <-chan struct{}) error {
	if l == nil || wake == nil {
		return nil
	}
	select {
	case <-wake:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// InFlight reports how many requests the line's key is carrying right now.
func (l *Limiter) InFlight(channel domain.Channel) int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.inFlight[channel.CredentialKey()]
}

// The limiter is what the engine holds the slots with.
var _ domain.KeyConcurrencyLimiter = (*Limiter)(nil)
