package store

import (
	"context"
	"testing"
	"time"

	"github.com/yhw5231/fluxgate/internal/proxypool"
)

func preparePoolStore(t *testing.T) *SQLiteStore {
	t.Helper()
	store := prepareUpstreamStore(t)
	return store
}

// A pool is one more configuration table: its management token is write-only, and
// an upstream names the pool its accounts draw addresses from.
func TestPoolIsStoredAndAnUpstreamNamesIt(t *testing.T) {
	store := preparePoolStore(t)
	ctx := context.Background()
	pool, err := store.CreateResource(ctx, "pools", map[string]any{
		"name":       "IPv6 pool",
		"admin_url":  "http://10.0.0.5:8080",
		"token":      "pool-secret",
		"socks_host": "10.0.0.5",
		"mode":       "auto",
		"enabled":    true,
	})
	if err != nil {
		t.Fatalf("CreateResource(pools) error = %v", err)
	}
	poolID, _ := pool["id"].(int64)
	// The store answers with the row it wrote; masking a secret is the management
	// API's job, because only it knows which callers may see one.
	if pool["token"] != "pool-secret" {
		t.Fatalf("token = %v, want the stored value", pool["token"])
	}

	row := createUpstream(t, store, map[string]any{
		"name":    "Example",
		"url":     "https://api.example.com",
		"keys":    []any{"key-one"},
		"models":  []any{"gpt-4.1"},
		"pool_id": poolID,
	})
	if row["pool_id"] != poolID {
		t.Fatalf("upstream pool_id = %v, want %d", row["pool_id"], poolID)
	}
	configuration, err := store.LoadConfiguration(ctx)
	if err != nil {
		t.Fatalf("LoadConfiguration() error = %v", err)
	}
	if len(configuration.Pools) != 1 || configuration.Pools[0].AdminURL != "http://10.0.0.5:8080" {
		t.Fatalf("pools = %#v", configuration.Pools)
	}
	if configuration.Pools[0].Token != "pool-secret" {
		t.Fatalf("the stored token was not read back: %#v", configuration.Pools[0])
	}
	for _, channel := range configuration.Channels {
		if channel.SitePoolID != poolID {
			t.Fatalf("channel pool = %d, want %d", channel.SitePoolID, poolID)
		}
	}
}

// The lease table is what keeps an account's address stable across a restart.
func TestPoolAssignmentsRoundTripAndCascade(t *testing.T) {
	store := preparePoolStore(t)
	ctx := context.Background()
	pool, err := store.CreateResource(ctx, "pools", map[string]any{
		"name": "IPv6 pool", "admin_url": "http://10.0.0.5:8080", "enabled": true,
	})
	if err != nil {
		t.Fatalf("CreateResource(pools) error = %v", err)
	}
	poolID, _ := pool["id"].(int64)

	assignments := []proxypool.Assignment{{
		PoolID:    poolID,
		OwnerKey:  "t12",
		LeaseID:   "gw-1-ipv6-pool-t12",
		IPv6:      "2001:db8::1",
		Port:      20001,
		SiteID:    7,
		SocksURL:  "socks5h://10.0.0.5:20001",
		UpdatedAt: time.Now().UTC(),
	}}
	if err := store.SavePoolAssignments(ctx, assignments); err != nil {
		t.Fatalf("SavePoolAssignments() error = %v", err)
	}
	loaded, err := store.LoadPoolAssignments(ctx)
	if err != nil {
		t.Fatalf("LoadPoolAssignments() error = %v", err)
	}
	if len(loaded) != 1 || loaded[0].OwnerKey != "t12" || loaded[0].SocksURL != "socks5h://10.0.0.5:20001" {
		t.Fatalf("assignments = %#v", loaded)
	}

	// Removing the pool takes its assignments with it: a lease of a pool that no
	// longer exists would never be dialed.
	removed, err := store.DeleteResource(ctx, "pools", poolID)
	if err != nil {
		t.Fatalf("DeleteResource(pools) error = %v", err)
	}
	if removed["pool_assignments"] != 1 {
		t.Fatalf("removed = %#v, want the assignment counted", removed)
	}
	loaded, err = store.LoadPoolAssignments(ctx)
	if err != nil {
		t.Fatalf("LoadPoolAssignments() after delete error = %v", err)
	}
	if len(loaded) != 0 {
		t.Fatalf("assignments after deleting the pool = %#v", loaded)
	}
}

func TestPoolRecordRejectsWhatItCannotUse(t *testing.T) {
	store := preparePoolStore(t)
	ctx := context.Background()
	if _, err := store.CreateResource(ctx, "pools", map[string]any{
		"name": "no address",
	}); err == nil {
		t.Fatal("a pool without a management address was accepted")
	}
	if _, err := store.CreateResource(ctx, "pools", map[string]any{
		"name": "bad mode", "admin_url": "http://10.0.0.5:8080", "mode": "carrier-pigeon",
	}); err == nil {
		t.Fatal("a pool with an unknown mode was accepted")
	}
	if _, err := store.CreateResource(ctx, "pools", map[string]any{
		"name": "bad socks", "admin_url": "http://10.0.0.5:8080", "socks_host": "http://10.0.0.5",
	}); err == nil {
		t.Fatal("a pool whose SOCKS host is a URL was accepted")
	}
}
