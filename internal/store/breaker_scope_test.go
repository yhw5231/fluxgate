package store

import (
	"context"
	"testing"
	"time"

	"github.com/yhw5231/fluxgate/internal/breaker"
)

// addSite inserts one upstream with one account holding the given credential, which
// is what a circuit filed under that credential is resolved against.
func addSite(t *testing.T, store *SQLiteStore, name, credential string) int64 {
	t.Helper()
	ctx := context.Background()
	result, err := store.db.ExecContext(ctx,
		`INSERT INTO sites (name, url, platform, status, global_weight) VALUES (?, ?, 'openai', 'active', 1)`,
		name, "https://"+name+".example.com")
	if err != nil {
		t.Fatalf("insert site %s: %v", name, err)
	}
	siteID, err := result.LastInsertId()
	if err != nil {
		t.Fatalf("read site id: %v", err)
	}
	if _, err := store.db.ExecContext(ctx,
		`INSERT INTO accounts (site_id, access_token, status) VALUES (?, ?, 'active')`, siteID, credential); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	return siteID
}

// recordCircuit writes one circuit the way a release that knew no upstream would:
// a circuit filed under a credential alone carries no site, and its key is the
// shape it has always had.
func recordCircuit(t *testing.T, store *SQLiteStore, scope breaker.Scope, state breaker.State) {
	t.Helper()
	if _, err := store.UpdateBreakerState(context.Background(), scope, func(breaker.State) breaker.State { return state }); err != nil {
		t.Fatalf("record circuit %+v: %v", scope, err)
	}
}

// A circuit recorded under a credential alone is one account per upstream that
// holds that credential, and the migration has to file it under each of them —
// with the state it was recorded with — so that an upgrade neither cools a line
// down again nor brings a held one back. A credential the configuration no longer
// holds leaves nothing behind.
func TestMigrateBreakerScopesFilesACredentialCircuitUnderItsUpstreams(t *testing.T) {
	store := prepareUpstreamStore(t)
	ctx := context.Background()
	if err := store.EnsureBreakerSchema(ctx); err != nil {
		t.Fatalf("EnsureBreakerSchema() error = %v", err)
	}

	alpha := addSite(t, store, "alpha", "shared-key")
	beta := addSite(t, store, "beta", "shared-key")

	blockedUntil := time.Now().UTC().Add(10 * time.Minute).Truncate(time.Nanosecond)
	wholeKey := breaker.State{ConsecutiveFailures: 3, CooldownLevel: 2, BlockedUntil: blockedUntil}
	recordCircuit(t, store, breaker.Scope{KeyID: "shared-key"}, wholeKey)
	recordCircuit(t, store, breaker.Scope{KeyID: "shared-key", Model: "gpt-4o"}, breaker.State{Disabled: true})
	// A credential nobody holds any more: its line is gone, and so is its circuit.
	recordCircuit(t, store, breaker.Scope{KeyID: "retired-key"}, breaker.State{CooldownLevel: 1})

	filed, dropped, err := store.MigrateBreakerScopes(ctx)
	if err != nil {
		t.Fatalf("MigrateBreakerScopes() error = %v", err)
	}
	if filed != 2 || dropped != 1 {
		t.Fatalf("MigrateBreakerScopes() = filed %d, dropped %d; want 2 filed and 1 dropped", filed, dropped)
	}

	states, err := store.LoadBreakerStates(ctx)
	if err != nil {
		t.Fatalf("LoadBreakerStates() error = %v", err)
	}
	if len(states) != 4 {
		t.Fatalf("stored circuits = %#v, want both accounts of both circuits", states)
	}
	for _, siteID := range []int64{alpha, beta} {
		whole := breaker.Scope{SiteID: siteID, KeyID: "shared-key"}
		if got := states[whole]; got != wholeKey {
			t.Errorf("circuit %+v = %#v, want the recorded state %#v", whole, got, wholeKey)
		}
		perModel := breaker.Scope{SiteID: siteID, KeyID: "shared-key", Model: "gpt-4o"}
		if got, known := states[perModel]; !known || !got.Disabled {
			t.Errorf("circuit %+v = %#v (known %v), want the disabled state it was recorded with", perModel, got, known)
		}
	}
	if _, known := states[breaker.Scope{KeyID: "shared-key"}]; known {
		t.Error("the circuit filed under the credential alone survived the migration")
	}
	if _, known := states[breaker.Scope{KeyID: "retired-key"}]; known {
		t.Error("a circuit of a credential the configuration no longer holds survived the migration")
	}

	// Running it again changes nothing, which is what a restart after an upgrade
	// does.
	filed, dropped, err = store.MigrateBreakerScopes(ctx)
	if err != nil {
		t.Fatalf("second MigrateBreakerScopes() error = %v", err)
	}
	if filed != 0 || dropped != 0 {
		t.Fatalf("second MigrateBreakerScopes() = filed %d, dropped %d; want nothing left to do", filed, dropped)
	}
}

// A database written by an earlier release has the breaker table without the
// upstream column. It is brought up to the current shape in place, and the circuits
// it recorded under a credential are filed under their upstream — while the ones
// filed under a channel, which need no migration, are read exactly as they were
// written.
func TestBreakerSchemaUpgradesADatabaseFromAnEarlierRelease(t *testing.T) {
	store := prepareUpstreamStore(t)
	ctx := context.Background()
	if _, err := store.db.ExecContext(ctx, `CREATE TABLE gateway_breaker_states (
		scope_key TEXT PRIMARY KEY,
		channel_id TEXT NOT NULL DEFAULT '',
		key_id TEXT NOT NULL DEFAULT '',
		model TEXT NOT NULL DEFAULT '',
		consecutive_failures INTEGER NOT NULL DEFAULT 0,
		cooldown_level INTEGER NOT NULL DEFAULT 0,
		blocked_until TEXT,
		disabled INTEGER NOT NULL DEFAULT 0,
		updated_at TEXT NOT NULL
	)`); err != nil {
		t.Fatalf("create the previous release's breaker table: %v", err)
	}
	alpha := addSite(t, store, "alpha", "shared-key")

	// The rows exactly as that release wrote them: the key of a credential-scoped
	// circuit carries no upstream, and the key of a channel-scoped one carries the
	// channel.
	updatedAt := time.Now().UTC().Format(time.RFC3339Nano)
	for _, row := range []struct {
		scopeKey  string
		channelID string
		keyID     string
		disabled  int
	}{
		{"\x00shared-key\x00", "", "shared-key", 1},
		{"42\x00\x00", "42", "", 0},
	} {
		if _, err := store.db.ExecContext(ctx, `INSERT INTO gateway_breaker_states (
			scope_key, channel_id, key_id, model, consecutive_failures, cooldown_level, disabled, updated_at
		) VALUES (?, ?, ?, '', 2, 1, ?, ?)`, row.scopeKey, row.channelID, row.keyID, row.disabled, updatedAt); err != nil {
			t.Fatalf("insert a circuit of the previous release: %v", err)
		}
	}

	if err := store.EnsureBreakerSchema(ctx); err != nil {
		t.Fatalf("EnsureBreakerSchema() on an older database error = %v", err)
	}
	filed, dropped, err := store.MigrateBreakerScopes(ctx)
	if err != nil {
		t.Fatalf("MigrateBreakerScopes() error = %v", err)
	}
	if filed != 1 || dropped != 0 {
		t.Fatalf("MigrateBreakerScopes() = filed %d, dropped %d; want the one key circuit filed", filed, dropped)
	}

	states, err := store.LoadBreakerStates(ctx)
	if err != nil {
		t.Fatalf("LoadBreakerStates() error = %v", err)
	}
	filedScope := breaker.Scope{SiteID: alpha, KeyID: "shared-key"}
	if state, known := states[filedScope]; !known || !state.Disabled || state.ConsecutiveFailures != 2 {
		t.Fatalf("filed circuit = %#v (known %v), want the state the earlier release recorded", state, known)
	}
	if state, known := states[breaker.Scope{ChannelID: "42"}]; !known || state.ConsecutiveFailures != 2 {
		t.Fatalf("channel circuit = %#v (known %v), want it read as it was written", state, known)
	}
	if len(states) != 2 {
		t.Fatalf("stored circuits = %#v, want the migrated one and the untouched one", states)
	}
}

// A circuit filed under a channel needs no migration: a channel id names one line
// of one upstream, so it is read exactly as it was written.
func TestMigrateBreakerScopesLeavesChannelCircuitsAlone(t *testing.T) {
	store := prepareUpstreamStore(t)
	ctx := context.Background()
	if err := store.EnsureBreakerSchema(ctx); err != nil {
		t.Fatalf("EnsureBreakerSchema() error = %v", err)
	}

	channel := breaker.Scope{ChannelID: "42"}
	recordCircuit(t, store, channel, breaker.State{Disabled: true})

	filed, dropped, err := store.MigrateBreakerScopes(ctx)
	if err != nil {
		t.Fatalf("MigrateBreakerScopes() error = %v", err)
	}
	if filed != 0 || dropped != 0 {
		t.Fatalf("MigrateBreakerScopes() = filed %d, dropped %d; want a channel circuit left alone", filed, dropped)
	}
	states, err := store.LoadBreakerStates(ctx)
	if err != nil {
		t.Fatalf("LoadBreakerStates() error = %v", err)
	}
	if state, known := states[channel]; !known || !state.Disabled {
		t.Fatalf("channel circuit = %#v (known %v), want the disabled state it was recorded with", state, known)
	}
}

// A circuit filed under an account carries its upstream, so two upstreams using one
// key string keep two circuits and a restart reads them back apart.
func TestBreakerStateKeepsTheUpstreamOfACredentialCircuit(t *testing.T) {
	store := prepareUpstreamStore(t)
	ctx := context.Background()
	if err := store.EnsureBreakerSchema(ctx); err != nil {
		t.Fatalf("EnsureBreakerSchema() error = %v", err)
	}

	blockedUntil := time.Now().UTC().Add(time.Minute).Truncate(time.Nanosecond)
	alpha := breaker.Scope{SiteID: 1, KeyID: "shared-key", Model: "gpt-4o"}
	beta := breaker.Scope{SiteID: 2, KeyID: "shared-key", Model: "gpt-4o"}
	recordCircuit(t, store, alpha, breaker.State{CooldownLevel: 2, BlockedUntil: blockedUntil})
	recordCircuit(t, store, beta, breaker.State{CooldownLevel: 1})

	states, err := store.LoadBreakerStates(ctx)
	if err != nil {
		t.Fatalf("LoadBreakerStates() error = %v", err)
	}
	if got := states[alpha]; got.CooldownLevel != 2 || !got.BlockedUntil.Equal(blockedUntil) {
		t.Errorf("circuit of the first upstream = %#v, want the state it was recorded with", got)
	}
	if got := states[beta]; got.CooldownLevel != 1 || !got.BlockedUntil.IsZero() {
		t.Errorf("circuit of the second upstream = %#v, want the state it was recorded with", got)
	}
}

// Deleting an upstream takes its accounts' circuits with it, and leaves another
// upstream's circuit alone even when both were configured with the same key.
func TestDeletingAnUpstreamRemovesItsKeyCircuits(t *testing.T) {
	store := prepareUpstreamStore(t)
	ctx := context.Background()
	if err := store.EnsureBreakerSchema(ctx); err != nil {
		t.Fatalf("EnsureBreakerSchema() error = %v", err)
	}

	alpha := createUpstream(t, store, map[string]any{
		"name": "alpha", "url": "https://alpha.example.com",
		"keys": []any{"shared-key"}, "models": []any{"gpt-4.1"},
	})
	beta := createUpstream(t, store, map[string]any{
		"name": "beta", "url": "https://beta.example.com",
		"keys": []any{"shared-key"}, "models": []any{"gpt-4.1"},
	})
	alphaID := alpha["id"].(int64)
	betaID := beta["id"].(int64)

	alphaCircuit := breaker.Scope{SiteID: alphaID, KeyID: "shared-key"}
	betaCircuit := breaker.Scope{SiteID: betaID, KeyID: "shared-key"}
	recordCircuit(t, store, alphaCircuit, breaker.State{Disabled: true})
	recordCircuit(t, store, betaCircuit, breaker.State{Disabled: true})

	if _, err := store.DeleteResource(ctx, "upstreams", alphaID); err != nil {
		t.Fatalf("DeleteResource(upstreams) error = %v", err)
	}

	states, err := store.LoadBreakerStates(ctx)
	if err != nil {
		t.Fatalf("LoadBreakerStates() error = %v", err)
	}
	if _, known := states[alphaCircuit]; known {
		t.Error("the deleted upstream's circuit survived it")
	}
	if _, known := states[betaCircuit]; !known {
		t.Error("the other upstream's circuit went with the deleted one")
	}
}
