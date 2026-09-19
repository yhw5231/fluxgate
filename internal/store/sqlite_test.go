package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/yhw5231/fluxgate/internal/breaker"
)

func openTestStore(t *testing.T) *SQLiteStore {
	t.Helper()
	store, err := OpenSQLite(filepath.Join(t.TempDir(), "gateway-test.db"))
	if err != nil {
		t.Fatalf("OpenSQLite() error = %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	return store
}

func createDownstreamKeysTable(t *testing.T, store *SQLiteStore) {
	t.Helper()
	_, err := store.db.Exec(`CREATE TABLE downstream_api_keys (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL,
		key TEXT NOT NULL,
		enabled INTEGER DEFAULT 1,
		expires_at TEXT,
		max_cost REAL,
		used_cost REAL DEFAULT 0,
		max_requests INTEGER,
		used_requests INTEGER DEFAULT 0,
		supported_models TEXT,
		allowed_route_ids TEXT,
		site_weight_multipliers TEXT,
		excluded_site_ids TEXT,
		excluded_credential_refs TEXT
	)`)
	if err != nil {
		t.Fatalf("create downstream_api_keys: %v", err)
	}
}

func TestAuthenticateDownstreamKeyAcceptsValidCredentialAndDecodesRestrictions(t *testing.T) {
	store := openTestStore(t)
	createDownstreamKeysTable(t, store)

	expiresAt := time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)
	_, err := store.db.Exec(`INSERT INTO downstream_api_keys (
		name, key, enabled, expires_at, max_cost, used_cost, max_requests,
		used_requests, supported_models, allowed_route_ids,
		site_weight_multipliers, excluded_site_ids, excluded_credential_refs
	) VALUES (?, ?, 1, ?, 10, 2, 100, 5, ?, ?, ?, ?, ?)`,
		"test-client",
		"downstream-secret",
		expiresAt,
		`["gpt-4.1","claude-sonnet"]`,
		`[1,2]`,
		`{"7":1.5}`,
		`[9]`,
		`[{"accountId":11,"reference":"account-11"}]`,
	)
	if err != nil {
		t.Fatalf("insert downstream key: %v", err)
	}

	key, err := store.AuthenticateDownstreamKey(context.Background(), "downstream-secret", time.Now().UTC())
	if err != nil {
		t.Fatalf("AuthenticateDownstreamKey() error = %v", err)
	}
	if key.Name != "test-client" || !key.Enabled {
		t.Fatalf("authenticated key = %#v", key)
	}
	if len(key.SupportedModels) != 2 || key.SupportedModels[0] != "gpt-4.1" {
		t.Fatalf("SupportedModels = %#v", key.SupportedModels)
	}
	if len(key.AllowedRouteIDs) != 2 || key.AllowedRouteIDs[1] != 2 {
		t.Fatalf("AllowedRouteIDs = %#v", key.AllowedRouteIDs)
	}
	if got := key.SiteMultipliers[7]; got != 1.5 {
		t.Fatalf("SiteMultipliers[7] = %v, want 1.5", got)
	}
	if len(key.ExcludedSiteIDs) != 1 || key.ExcludedSiteIDs[0] != 9 {
		t.Fatalf("ExcludedSiteIDs = %#v", key.ExcludedSiteIDs)
	}
	if len(key.ExcludedCredentials) != 1 || key.ExcludedCredentials[0].Reference != "account-11" {
		t.Fatalf("ExcludedCredentials = %#v", key.ExcludedCredentials)
	}
}

func TestAuthenticateDownstreamKeyRejectsInvalidExpiredAndExhaustedCredentials(t *testing.T) {
	store := openTestStore(t)
	createDownstreamKeysTable(t, store)

	now := time.Now().UTC()
	rows := []struct {
		name         string
		credential   string
		expiresAt    any
		maxCost      any
		usedCost     float64
		maxRequests  any
		usedRequests int64
	}{
		{name: "expired", credential: "expired-secret", expiresAt: now.Add(-time.Minute).Format(time.RFC3339Nano)},
		{name: "request-limit", credential: "request-secret", maxRequests: int64(3), usedRequests: 3},
		{name: "cost-limit", credential: "cost-secret", maxCost: 2.5, usedCost: 2.5},
	}
	for _, row := range rows {
		_, err := store.db.Exec(`INSERT INTO downstream_api_keys (
			name, key, enabled, expires_at, max_cost, used_cost, max_requests,
			used_requests, supported_models, allowed_route_ids,
			site_weight_multipliers, excluded_site_ids, excluded_credential_refs
		) VALUES (?, ?, 1, ?, ?, ?, ?, ?, NULL, NULL, NULL, NULL, NULL)`,
			row.name, row.credential, row.expiresAt, row.maxCost, row.usedCost, row.maxRequests, row.usedRequests)
		if err != nil {
			t.Fatalf("insert %s key: %v", row.name, err)
		}
	}

	for _, credential := range []string{"missing-secret", "expired-secret", "request-secret", "cost-secret"} {
		_, err := store.AuthenticateDownstreamKey(context.Background(), credential, now)
		if !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("AuthenticateDownstreamKey(%q) error = %v, want ErrUnauthorized", credential, err)
		}
	}
}

func TestLoadConfigurationModelsChannelPolicyAndProxyPrecedence(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	statements := []string{
		`CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT)`,
		`CREATE TABLE proxy_profiles (
			id INTEGER PRIMARY KEY,
			name TEXT NOT NULL,
			protocol TEXT NOT NULL,
			url TEXT NOT NULL,
			is_default INTEGER DEFAULT 0,
			enabled INTEGER DEFAULT 1
		)`,
		`CREATE TABLE downstream_api_keys (
			id INTEGER PRIMARY KEY,
			name TEXT NOT NULL,
			key TEXT NOT NULL,
			enabled INTEGER DEFAULT 1,
			expires_at TEXT,
			max_cost REAL,
			used_cost REAL DEFAULT 0,
			max_requests INTEGER,
			used_requests INTEGER DEFAULT 0,
			supported_models TEXT,
			allowed_route_ids TEXT,
			site_weight_multipliers TEXT,
			excluded_site_ids TEXT,
			excluded_credential_refs TEXT
		)`,
		`CREATE TABLE sites (
			id INTEGER PRIMARY KEY,
			name TEXT NOT NULL,
			url TEXT NOT NULL,
			platform TEXT NOT NULL,
			forced_upstream_endpoint TEXT,
			proxy_url TEXT,
			use_system_proxy INTEGER DEFAULT 0,
			custom_headers TEXT,
			status TEXT NOT NULL DEFAULT 'active'
		)`,
		`CREATE TABLE accounts (
			id INTEGER PRIMARY KEY,
			site_id INTEGER NOT NULL,
			access_token TEXT NOT NULL,
			api_token TEXT,
			extra_config TEXT,
			status TEXT NOT NULL DEFAULT 'active'
		)`,
		`CREATE TABLE account_tokens (
			id INTEGER PRIMARY KEY,
			account_id INTEGER NOT NULL,
			token TEXT NOT NULL,
			proxy_url TEXT,
			use_system_proxy INTEGER DEFAULT 0,
			enabled INTEGER DEFAULT 1
		)`,
		`CREATE TABLE token_routes (
			id INTEGER PRIMARY KEY,
			model_pattern TEXT NOT NULL,
			model_mapping TEXT,
			routing_strategy TEXT DEFAULT 'weighted',
			enabled INTEGER DEFAULT 1
		)`,
		`CREATE TABLE route_channels (
			id INTEGER PRIMARY KEY,
			route_id INTEGER NOT NULL,
			account_id INTEGER NOT NULL,
			token_id INTEGER,
			source_model TEXT,
			priority INTEGER DEFAULT 0,
			weight INTEGER DEFAULT 10,
			enabled INTEGER DEFAULT 1
		)`,
		`INSERT INTO sites (id, name, url, platform, proxy_url, use_system_proxy, custom_headers, status)
		 VALUES (1, 'site', 'https://API.Example.com/v1', 'openai', 'http://site-proxy.example:8080', 1, '{}', 'active')`,
		`INSERT INTO accounts (id, site_id, access_token, api_token, extra_config, status)
		 VALUES (10, 1, 'account-access', 'account-api', '{"proxyUrl":"http://account-proxy.example:8080","useSystemProxy":true}', 'active')`,
		`INSERT INTO account_tokens (id, account_id, token, proxy_url, use_system_proxy, enabled)
		 VALUES (20, 10, 'token-key', 'http://token-proxy.example:8080', 0, 1)`,
		`INSERT INTO token_routes (id, model_pattern, model_mapping, routing_strategy, enabled)
		 VALUES (30, 'gpt-*', '{"gpt-*":"mapped-model"}', 'round_robin', 1)`,
		`INSERT INTO route_channels (id, route_id, account_id, token_id, source_model, priority, weight, enabled)
		 VALUES (40, 30, 10, 20, NULL, 5, 7, 1)`,
	}
	for _, statement := range statements {
		if _, err := store.db.Exec(statement); err != nil {
			t.Fatalf("execute test schema statement: %v", err)
		}
	}

	configuration, err := store.LoadConfiguration(ctx)
	if err != nil {
		t.Fatalf("LoadConfiguration() error = %v", err)
	}
	if len(configuration.Channels) != 1 {
		t.Fatalf("channel count = %d, want 1", len(configuration.Channels))
	}

	channel := configuration.Channels[0]
	if channel.RoutingStrategy != "round_robin" {
		t.Fatalf("routing strategy = %q, want round_robin", channel.RoutingStrategy)
	}
	if channel.BreakerMode != string(breaker.ModeKeyModelCooldown) {
		t.Fatalf("breaker mode = %q, want %q", channel.BreakerMode, breaker.ModeKeyModelCooldown)
	}
	if channel.APIKey != "token-key" {
		t.Fatalf("channel key = %q, want token key", channel.APIKey)
	}
	if channel.ProxyURL != "http://token-proxy.example:8080" {
		t.Fatalf("channel proxy = %q, want token proxy", channel.ProxyURL)
	}
	if channel.UseSystemProxy {
		t.Fatal("explicit token proxy retained the inherited system-proxy decision")
	}
	if channel.SiteProxyURL != "http://site-proxy.example:8080" {
		t.Fatalf("site proxy = %q, want configured site proxy", channel.SiteProxyURL)
	}
	if !channel.SiteUseSystemProxy {
		t.Fatal("site use_system_proxy was not loaded")
	}
	if channel.ModelMapping["gpt-4.1"] != "" && channel.ModelMapping["gpt-*"] != "mapped-model" {
		t.Fatalf("model mapping = %#v", channel.ModelMapping)
	}
}

func TestBreakerStatePersistsAndRestoresAcrossStoreRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "breaker.db")
	ctx := context.Background()

	first, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("first OpenSQLite() error = %v", err)
	}
	if err := first.EnsureBreakerSchema(ctx); err != nil {
		t.Fatalf("EnsureBreakerSchema() error = %v", err)
	}

	scope := breaker.Scope{ChannelID: "channel-a", KeyID: "key-a", Model: "model-a"}
	blockedUntil := time.Now().UTC().Add(5 * time.Minute).Truncate(time.Nanosecond)
	want := breaker.State{
		ConsecutiveFailures: 2,
		CooldownLevel:       3,
		BlockedUntil:        blockedUntil,
		Disabled:            true,
	}
	got, err := first.UpdateBreakerState(ctx, scope, func(breaker.State) breaker.State { return want })
	if err != nil {
		t.Fatalf("UpdateBreakerState() error = %v", err)
	}
	if got != want {
		t.Fatalf("updated state = %#v, want %#v", got, want)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("first Close() error = %v", err)
	}

	second, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("second OpenSQLite() error = %v", err)
	}
	defer second.Close()

	adapter, err := NewBreakerAdapter(ctx, second)
	if err != nil {
		t.Fatalf("NewBreakerAdapter() error = %v", err)
	}
	restored, ok := adapter.Load(scope)
	if !ok {
		t.Fatal("restored breaker state not found")
	}
	if restored != want {
		t.Fatalf("restored state = %#v, want %#v", restored, want)
	}
}

func TestCleanupBreakerStatesRemovesOnlyInactiveExpiredRows(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	if err := store.EnsureBreakerSchema(ctx); err != nil {
		t.Fatalf("EnsureBreakerSchema() error = %v", err)
	}

	now := time.Now().UTC()
	inactive := breaker.Scope{ChannelID: "inactive"}
	active := breaker.Scope{ChannelID: "active"}
	disabled := breaker.Scope{ChannelID: "disabled"}

	if _, err := store.UpdateBreakerState(ctx, inactive, func(breaker.State) breaker.State { return breaker.State{} }); err != nil {
		t.Fatalf("insert inactive state: %v", err)
	}
	if _, err := store.UpdateBreakerState(ctx, active, func(breaker.State) breaker.State {
		return breaker.State{BlockedUntil: now.Add(time.Hour)}
	}); err != nil {
		t.Fatalf("insert active state: %v", err)
	}
	if _, err := store.UpdateBreakerState(ctx, disabled, func(breaker.State) breaker.State {
		return breaker.State{Disabled: true}
	}); err != nil {
		t.Fatalf("insert disabled state: %v", err)
	}

	removed, err := store.CleanupBreakerStates(ctx, now)
	if err != nil {
		t.Fatalf("CleanupBreakerStates() error = %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed rows = %d, want 1", removed)
	}

	states, err := store.LoadBreakerStates(ctx)
	if err != nil {
		t.Fatalf("LoadBreakerStates() error = %v", err)
	}
	if _, exists := states[inactive]; exists {
		t.Fatal("inactive breaker state was not removed")
	}
	if _, exists := states[active]; !exists {
		t.Fatal("active breaker state was removed")
	}
	if _, exists := states[disabled]; !exists {
		t.Fatal("disabled breaker state was removed")
	}
}
