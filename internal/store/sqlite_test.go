package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yhw5231/fluxgate/internal/breaker"
	"github.com/yhw5231/fluxgate/internal/domain"
)

func int64Pointer(value int64) *int64 {
	return &value
}

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
		`[{"kind":"account_token","siteId":1,"accountId":11,"tokenId":22}]`,
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
	if len(key.ExcludedCredentials) != 1 {
		t.Fatalf("ExcludedCredentials = %#v", key.ExcludedCredentials)
	}
	credential := key.ExcludedCredentials[0]
	if credential.Kind != "account_token" || credential.SiteID != 1 || credential.AccountID != 11 || credential.TokenID != 22 {
		t.Fatalf("ExcludedCredentials[0] = %#v", credential)
	}
	// supported_models is an exclusion list, so it becomes the deny patterns.
	policy := key.Policy()
	if !policy.DeniesModel("gpt-4.1") || !policy.DeniesModel("claude-sonnet") {
		t.Fatalf("deny patterns = %#v, want both configured models denied", policy.DeniedModelPatterns)
	}
	if policy.DeniesModel("claude-opus") {
		t.Fatal("policy denied a model that is not on the exclusion list")
	}
	if !policy.ExcludesCredential(domain.Channel{SiteID: 1, AccountID: 11, TokenID: int64Pointer(22)}) {
		t.Fatal("excluded credential did not match its channel")
	}
	if policy.ExcludesCredential(domain.Channel{SiteID: 1, AccountID: 11}) {
		t.Fatal("a channel without a token matched a token-scoped exclusion")
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
			status TEXT NOT NULL DEFAULT 'active',
			global_weight REAL DEFAULT 1
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
			enabled INTEGER DEFAULT 1,
			display_name TEXT,
			route_mode TEXT DEFAULT 'pattern'
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
		`CREATE TABLE route_group_sources (
			id INTEGER PRIMARY KEY,
			group_route_id INTEGER NOT NULL,
			source_route_id INTEGER NOT NULL
		)`,
		`INSERT INTO sites (id, name, url, platform, proxy_url, use_system_proxy, custom_headers, status, global_weight)
		 VALUES (1, 'site', 'https://API.Example.com/v1', 'openai', 'http://site-proxy.example:8080', 1, '{}', 'active', 2.5)`,
		`INSERT INTO accounts (id, site_id, access_token, api_token, extra_config, status)
		 VALUES (10, 1, 'account-access', 'account-api', '{"proxyUrl":"http://account-proxy.example:8080","useSystemProxy":true}', 'active')`,
		`INSERT INTO account_tokens (id, account_id, token, proxy_url, use_system_proxy, enabled)
		 VALUES (20, 10, 'token-key', 'http://token-proxy.example:8080', 0, 1)`,
		// Declared order matters: the first matching pattern must win.
		`INSERT INTO token_routes (id, model_pattern, model_mapping, routing_strategy, enabled)
		 VALUES (30, 'gpt-*', '{"gpt-*":"mapped-model","gpt-4.1":"exact-target"}', 'round_robin', 1)`,
		`INSERT INTO route_channels (id, route_id, account_id, token_id, source_model, priority, weight, enabled)
		 VALUES (40, 30, 10, 20, NULL, 5, 7, 1)`,
		// An exact route, and a group that covers it through route_group_sources.
		`INSERT INTO token_routes (id, model_pattern, routing_strategy, enabled, display_name, route_mode)
		 VALUES (31, 'claude-opus-4-5', 'weighted', 1, NULL, 'pattern')`,
		`INSERT INTO route_channels (id, route_id, account_id, token_id, source_model, priority, weight, enabled)
		 VALUES (41, 31, 10, 20, NULL, 0, 10, 1)`,
		`INSERT INTO token_routes (id, model_pattern, routing_strategy, enabled, display_name, route_mode)
		 VALUES (32, '', 'weighted', 1, 'claude-opus-4-6', 'explicit_group')`,
		`INSERT INTO route_group_sources (group_route_id, source_route_id) VALUES (32, 31)`,
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
	if len(configuration.Routes) != 3 {
		t.Fatalf("route count = %d, want 3", len(configuration.Routes))
	}
	if len(configuration.Channels) != 2 {
		t.Fatalf("channel count = %d, want 2", len(configuration.Channels))
	}

	globRoute := configuration.Routes[0]
	if globRoute.ID != 30 || globRoute.Mode != domain.RouteModePattern {
		t.Fatalf("route = %#v", globRoute)
	}
	if len(globRoute.ModelMapping) != 2 {
		t.Fatalf("model mapping = %#v, want two ordered entries", globRoute.ModelMapping)
	}
	if globRoute.ModelMapping[0].Pattern != "gpt-*" || globRoute.ModelMapping[1].Pattern != "gpt-4.1" {
		t.Fatalf("model mapping order = %#v, want declaration order preserved", globRoute.ModelMapping)
	}
	if got := globRoute.ModelMapping.Resolve("gpt-4.1"); got != "exact-target" {
		t.Fatalf("Resolve(gpt-4.1) = %q, want exact-target", got)
	}
	if got := globRoute.ModelMapping.Resolve("gpt-4o"); got != "mapped-model" {
		t.Fatalf("Resolve(gpt-4o) = %q, want mapped-model", got)
	}
	if got := globRoute.ModelMapping.Resolve("claude-3"); got != "claude-3" {
		t.Fatalf("Resolve(claude-3) = %q, want the requested model", got)
	}

	channel := globRoute.Channels[0]
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
	if channel.SiteID != 1 || channel.AccountID != 10 || channel.TokenID == nil || *channel.TokenID != 20 {
		t.Fatalf("channel identity = site:%d account:%d token:%v", channel.SiteID, channel.AccountID, channel.TokenID)
	}
	if channel.SiteGlobalWeight != 2.5 {
		t.Fatalf("site global weight = %v, want 2.5", channel.SiteGlobalWeight)
	}

	// A channel without an explicit source_model inherits its exact route pattern.
	exactChannel := configuration.Routes[1].Channels[0]
	if exactChannel.SourceModel != "claude-opus-4-5" {
		t.Fatalf("source model = %q, want the exact route pattern", exactChannel.SourceModel)
	}
	if got := configuration.Routes[0].Channels[0].SourceModel; got != "" {
		t.Fatalf("glob route source model = %q, want empty", got)
	}

	group := configuration.Routes[2]
	if group.Mode != domain.RouteModeExplicitGroup || group.DisplayName != "claude-opus-4-6" {
		t.Fatalf("group route = %#v", group)
	}
	if len(group.SourceRouteIDs) != 1 || group.SourceRouteIDs[0] != 31 {
		t.Fatalf("group source routes = %#v, want [31]", group.SourceRouteIDs)
	}

	// The covered exact route is hidden behind the group alias.
	models := configuration.Models
	if len(models) != 2 || models[0] != "claude-opus-4-6" || models[1] != "gpt-*" {
		t.Fatalf("models = %#v, want [claude-opus-4-6 gpt-*]", models)
	}
}

func TestParseExcludedCredentialsRejectsMalformedEntries(t *testing.T) {
	credentials, err := parseExcludedCredentials(`[
		{"kind":"account_token","siteId":1,"accountId":2,"tokenId":3},
		{"kind":"account_token","siteId":1,"accountId":2,"tokenId":3},
		{"kind":"account","siteId":1,"accountId":2,"tokenId":3},
		{"kind":"account_token","siteId":0,"accountId":2,"tokenId":3},
		{"kind":"account_token","siteId":1,"accountId":2},
		{"kind":"account_token","siteId":1.5,"accountId":2,"tokenId":3}
	]`)
	if err != nil {
		t.Fatalf("parseExcludedCredentials() error = %v", err)
	}
	if len(credentials) != 1 {
		t.Fatalf("credentials = %#v, want only the first valid unique entry", credentials)
	}
	if credentials[0].TokenID != 3 {
		t.Fatalf("credentials[0] = %#v", credentials[0])
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

// createUpstreamTables builds the empty upstream configuration schema using the
// same DDL the gateway applies to a fresh database.
func createUpstreamTables(t *testing.T, store *SQLiteStore) {
	t.Helper()
	for _, statement := range upstreamSchemaDDL {
		if _, err := store.db.Exec(statement); err != nil {
			t.Fatalf("execute test schema statement: %v", err)
		}
	}
}

// The upstream schema leaves api_token, extra_config and custom_headers NULL in
// normal rows, so every nullable column must be scanned through sql.NullString.
func TestLoadRoutesToleratesNullableAccountAndSiteColumns(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	createUpstreamTables(t, store)
	statements := []string{
		`INSERT INTO sites (id, name, url, platform) VALUES (1, 'site', 'https://example.test/v1', 'openai')`,
		// api_token and extra_config deliberately NULL.
		`INSERT INTO accounts (id, site_id, access_token) VALUES (1, 1, 'account-access')`,
		`INSERT INTO token_routes (id, model_pattern) VALUES (1, 'model-a')`,
		// token_id NULL as well.
		`INSERT INTO route_channels (id, route_id, account_id) VALUES (1, 1, 1)`,
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
	channel := configuration.Routes[0].Channels[0]
	if channel.APIKey != "account-access" {
		t.Fatalf("APIKey = %q, want the account access token", channel.APIKey)
	}
	if channel.TokenID != nil {
		t.Fatalf("TokenID = %v, want nil for a token-less channel", channel.TokenID)
	}
	if !channel.Enabled {
		t.Fatal("channel with NULL optional columns was treated as disabled")
	}
}

// A fresh deployment points the gateway at ../data/hub.db without creating the
// directory first, so OpenSQLite must create it instead of surfacing the opaque
// SQLite "unable to open database file" error.
func TestOpenSQLiteCreatesMissingParentDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "nested", "hub.db")
	store, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite() error = %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("database file was not created: %v", err)
	}
}

func TestOpenSQLiteRejectsParentDirectoryThatIsAFile(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("occupied"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	_, err := OpenSQLite(filepath.Join(blocker, "hub.db"))
	if err == nil {
		t.Fatal("OpenSQLite() succeeded with a file in place of the data directory")
	}
	if !strings.Contains(err.Error(), "is not a directory") {
		t.Fatalf("error = %v, want the not-a-directory diagnosis", err)
	}
}

// Docker bind mounts make a missing or root-owned host directory unwritable for
// the container user, and SQLite reports that as "out of memory (14)". The
// store must name the real cause instead.
func TestOpenSQLiteRejectsDirectoryAsDatabasePath(t *testing.T) {
	_, err := OpenSQLite(t.TempDir())
	if err == nil {
		t.Fatal("OpenSQLite() succeeded with a directory as the database path")
	}
	if !strings.Contains(err.Error(), "is a directory") {
		t.Fatalf("error = %v, want the is-a-directory diagnosis", err)
	}
}

func TestEnsureUpstreamSchemaAcceptsCompleteSchema(t *testing.T) {
	store := openTestStore(t)
	createUpstreamTables(t, store)
	if err := store.VerifyIntegrity(context.Background()); err != nil {
		t.Fatalf("VerifyIntegrity() error = %v", err)
	}
	if err := store.EnsureUpstreamSchema(context.Background()); err != nil {
		t.Fatalf("EnsureUpstreamSchema() error = %v", err)
	}
}

// A first-time deployment starts against a database the gateway created empty,
// so the full upstream schema must be created instead of failing configuration
// loading with opaque "no such table" SQL errors.
func TestEnsureUpstreamSchemaCreatesMissingTablesOnFreshDatabase(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	if err := store.EnsureBreakerSchema(ctx); err != nil {
		t.Fatalf("EnsureBreakerSchema() error = %v", err)
	}

	if err := store.EnsureUpstreamSchema(ctx); err != nil {
		t.Fatalf("EnsureUpstreamSchema() error = %v", err)
	}
	for _, table := range requiredUpstreamTables {
		var name string
		err := store.db.QueryRow(`SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&name)
		if err != nil {
			t.Fatalf("table %s was not created: %v", table, err)
		}
	}
}

// A database that carries part of the schema is a wrong or truncated file, not
// a fresh deployment; it must be rejected with the tables it has and lacks.
func TestEnsureUpstreamSchemaRejectsPartialSchema(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	if err := store.EnsureBreakerSchema(ctx); err != nil {
		t.Fatalf("EnsureBreakerSchema() error = %v", err)
	}
	if _, err := store.db.Exec(`CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT)`); err != nil {
		t.Fatalf("create partial schema: %v", err)
	}

	err := store.EnsureUpstreamSchema(ctx)
	if err == nil {
		t.Fatal("EnsureUpstreamSchema() accepted a partially populated schema")
	}
	for _, fragment := range []string{"sites", "token_routes", "settings", "gateway_breaker_states", "hub.db"} {
		if !strings.Contains(err.Error(), fragment) {
			t.Fatalf("error %q does not mention %q", err, fragment)
		}
	}
}
