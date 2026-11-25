package main

import (
	"strconv"
	"sync"

	"github.com/yhw5231/fluxgate/internal/proxypool"
	"github.com/yhw5231/fluxgate/internal/store"
)

// Which account uses which pool, and what the gateway calls that account.
//
// A pool is configured on the upstream, so every line of that upstream draws an
// exit from it. The lease is per account, not per line: two models served by one
// key are one account and go out through one address, while two keys of the same
// upstream must not share one — that is the point of the pool.
type poolBinding struct {
	Pool     proxypool.Pool
	SiteID   int64
	OwnerKey string
}

// poolBindings maps a line's credential to the pool lease it should dial through.
// It is rebuilt whenever the configuration changes, under a lock, because requests
// read it while a console write replaces it.
type poolBindings struct {
	mu    sync.RWMutex
	byKey map[string]poolBinding
}

func newPoolBindings() *poolBindings {
	return &poolBindings{byKey: make(map[string]poolBinding)}
}

// Set installs the bindings a configuration produces.
func (b *poolBindings) Set(configuration store.Configuration) {
	pools := make(map[int64]proxypool.Pool, len(configuration.Pools))
	for _, pool := range configuration.Pools {
		pools[pool.ID] = pool
	}
	next := make(map[string]poolBinding)
	for _, channel := range configuration.Channels {
		if channel.SitePoolID == 0 || channel.APIKey == "" {
			continue
		}
		pool, known := pools[channel.SitePoolID]
		if !known || !pool.Enabled {
			continue
		}
		owner := "a" + strconv.FormatInt(channel.AccountID, 10)
		if channel.TokenID != nil {
			owner = "t" + strconv.FormatInt(*channel.TokenID, 10)
		}
		next[channel.APIKey] = poolBinding{Pool: pool, SiteID: channel.SiteID, OwnerKey: owner}
	}
	b.mu.Lock()
	b.byKey = next
	b.mu.Unlock()
}

// Lookup reports the pool binding of one credential.
func (b *poolBindings) Lookup(credential string) (poolBinding, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	binding, ok := b.byKey[credential]
	return binding, ok
}
