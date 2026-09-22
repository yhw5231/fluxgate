package store

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/yhw5231/fluxgate/internal/breaker"
)

// prepareUpstreamStore returns a store with the upstream schema, which is what
// the console writes an upstream through.
func prepareUpstreamStore(t *testing.T) *SQLiteStore {
	t.Helper()
	store := openTestStore(t)
	if err := store.EnsureUpstreamSchema(context.Background()); err != nil {
		t.Fatalf("EnsureUpstreamSchema() error = %v", err)
	}
	return store
}

func createUpstream(t *testing.T, store *SQLiteStore, values map[string]any) map[string]any {
	t.Helper()
	row, err := store.CreateResource(context.Background(), "upstreams", values)
	if err != nil {
		t.Fatalf("CreateResource(upstreams) error = %v", err)
	}
	return row
}

// listUpstreams reads the decorated listing, which is what the console renders.
func listUpstreams(t *testing.T, store *SQLiteStore) []map[string]any {
	t.Helper()
	rows, err := store.ListResource(context.Background(), "upstreams")
	if err != nil {
		t.Fatalf("ListResource(upstreams) error = %v", err)
	}
	return rows
}

func rowsOf(t *testing.T, store *SQLiteStore, resource string) []map[string]any {
	t.Helper()
	rows, err := store.ListResource(context.Background(), resource)
	if err != nil {
		t.Fatalf("ListResource(%s) error = %v", resource, err)
	}
	return rows
}

func stringsOf(t *testing.T, value any) []string {
	t.Helper()
	list, err := decodeStringArray(value)
	if err != nil {
		t.Fatalf("decodeStringArray(%#v) error = %v", value, err)
	}
	return list
}

// One upstream, several keys and several models, is one configuration: the
// gateway has to end up with a route per model and a line per key, without the
// operator ever creating a route or a channel by hand.
func TestUpstreamCreatesARoutePerModelAndALinePerKey(t *testing.T) {
	store := prepareUpstreamStore(t)
	row := createUpstream(t, store, map[string]any{
		"name":           "Example",
		"url":            "https://api.example.com",
		"global_weight":  2.0,
		"custom_headers": `{"X-Org": "acme"}`,
		"keys":           []any{"primary-key", "second-key"},
		"models":         []any{"gpt-4.1", "claude-3"},
		"model_mapping":  map[string]any{"claude-3": "claude-3-5-sonnet"},
		"key_mode":       "available_first",
	})

	siteID, ok := row["id"].(int64)
	if !ok {
		t.Fatalf("created upstream has no id: %v", row)
	}
	if siteID <= 0 {
		t.Fatalf("created upstream id = %d, want a positive id", siteID)
	}
	if row["key_mode"] != "available_first" {
		t.Errorf("key_mode = %v, want available_first", row["key_mode"])
	}
	models := stringsOf(t, row["models"])
	if len(models) != 2 || models[0] != "claude-3" || models[1] != "gpt-4.1" {
		t.Errorf("models = %v, want [claude-3 gpt-4.1]", models)
	}
	mapping, err := decodeStringMap(row["model_mapping"])
	if err != nil {
		t.Fatalf("decode mapping: %v", err)
	}
	if mapping["claude-3"] != "claude-3-5-sonnet" {
		t.Errorf("model_mapping = %v, want claude-3 mapped to claude-3-5-sonnet", mapping)
	}
	keys := stringsOf(t, row["keys"])
	if len(keys) != 2 || !IsMaskedSecret(keys[0]) || !IsMaskedSecret(keys[1]) {
		t.Errorf("keys = %v, want two masked values", keys)
	}
	// The listing only ever shows a mask, so the stored values are read back to
	// prove the batch was stored one key per line rather than collapsed.
	var accessToken, tokenValue string
	if err := store.db.QueryRow(`SELECT access_token FROM accounts WHERE site_id = ?`, siteID).Scan(&accessToken); err != nil {
		t.Fatalf("read account key: %v", err)
	}
	if err := store.db.QueryRow(`SELECT token FROM account_tokens WHERE account_id = (SELECT id FROM accounts WHERE site_id = ?)`, siteID).Scan(&tokenValue); err != nil {
		t.Fatalf("read token key: %v", err)
	}
	if accessToken != "primary-key" || tokenValue != "second-key" {
		t.Errorf("stored keys = %q and %q, want primary-key and second-key", accessToken, tokenValue)
	}

	// A route per exposed model, created by the write.
	routes := rowsOf(t, store, "routes")
	patterns := map[string]int64{}
	for _, route := range routes {
		patterns[route["model_pattern"].(string)] = route["id"].(int64)
		if route["routing_strategy"] != "stable_first" {
			t.Errorf("route %v strategy = %v, want stable_first for the available-first key mode", route["model_pattern"], route["routing_strategy"])
		}
	}
	for _, model := range []string{"gpt-4.1", "claude-3"} {
		if _, ok := patterns[model]; !ok {
			t.Fatalf("no route was created for %s: %v", model, patterns)
		}
	}

	// A line per key, on every route, carrying the name the upstream knows.
	channels := rowsOf(t, store, "channels")
	byRoute := map[int64][]map[string]any{}
	for _, channel := range channels {
		byRoute[channel["route_id"].(int64)] = append(byRoute[channel["route_id"].(int64)], channel)
	}
	for model, routeID := range patterns {
		lines := byRoute[routeID]
		if len(lines) != 2 {
			t.Fatalf("route %s has %d lines, want 2", model, len(lines))
		}
		target := model
		if model == "claude-3" {
			target = "claude-3-5-sonnet"
		}
		priorities := map[int64]bool{}
		for _, line := range lines {
			if line["source_model"] != target {
				t.Errorf("route %s line source_model = %v, want %v", model, line["source_model"], target)
			}
			priorities[line["priority"].(int64)] = true
		}
		// Available-first gives the keys distinct priorities, so the selector
		// drains one before touching the next.
		if len(priorities) != 2 {
			t.Errorf("route %s priorities = %v, want two distinct values", model, priorities)
		}
	}

	// A round-robin upstream gives every key the same priority instead.
	second := createUpstream(t, store, map[string]any{
		"name": "Second", "url": "https://api.second.example",
		"keys": []any{"other-key"}, "models": []any{"gpt-4.1"}, "key_mode": "round_robin",
	})
	if second["key_mode"] != "round_robin" {
		t.Errorf("key_mode = %v, want round_robin", second["key_mode"])
	}
	// The same model on another upstream reuses the route rather than creating a
	// second one, which is what lets a request fail over between the two.
	reloaded := rowsOf(t, store, "routes")
	if len(reloaded) != len(routes) {
		t.Errorf("routes = %d, want %d: a shared model must not create a second route", len(reloaded), len(routes))
	}
}

// An upstream added from the console serves traffic as soon as it is saved: the
// status it is created with is the enabled one, so a new upstream is not sitting
// in the form looking configured while its lines are switched off.
func TestUpstreamIsCreatedEnabled(t *testing.T) {
	store := prepareUpstreamStore(t)
	row := createUpstream(t, store, map[string]any{
		"name": "Example", "url": "https://api.example.com",
		"keys": []any{"primary-key"}, "models": []any{"gpt-4.1"},
	})
	if row["status"] != "active" {
		t.Fatalf("status = %v, want active", row["status"])
	}
	listed := listUpstreams(t, store)
	if len(listed) != 1 || listed[0]["status"] != "active" {
		t.Fatalf("listing status = %v, want the created upstream enabled", listed)
	}

	// The status is what the loader reads to enable the lines, so the channel it
	// created has to come back enabled rather than merely stored.
	configuration, err := store.LoadConfiguration(context.Background())
	if err != nil {
		t.Fatalf("LoadConfiguration() error = %v", err)
	}
	if len(configuration.Channels) != 1 {
		t.Fatalf("channels = %d, want the one the upstream created", len(configuration.Channels))
	}
	if !configuration.Channels[0].Enabled {
		t.Error("the line of a newly created upstream is disabled")
	}
}

// A site the console created carries the platform the schema requires, and the
// account the keys live on.
func TestUpstreamStoresPlatformAndKeysOnOneAccount(t *testing.T) {
	store := prepareUpstreamStore(t)
	row := createUpstream(t, store, map[string]any{
		"name": "Example", "url": "https://api.example.com",
		"keys": []any{"first-key", "second-key", "third-key"}, "models": []any{"gpt-4.1"},
	})
	siteID := row["id"].(int64)

	var platform string
	if err := store.db.QueryRow(`SELECT platform FROM sites WHERE id = ?`, siteID).Scan(&platform); err != nil {
		t.Fatalf("read platform: %v", err)
	}
	if platform != defaultPlatform {
		t.Errorf("platform = %q, want %q", platform, defaultPlatform)
	}

	accounts := rowsOf(t, store, "accounts")
	if len(accounts) != 1 {
		t.Fatalf("accounts = %d, want 1", len(accounts))
	}
	if accounts[0]["access_token"] != "first-key" {
		t.Errorf("access_token = %v, want the first key", accounts[0]["access_token"])
	}
	tokens := rowsOf(t, store, "tokens")
	if len(tokens) != 2 {
		t.Fatalf("tokens = %d, want 2: the first key is the account's own", len(tokens))
	}
}

// Editing an upstream is the same call as creating one: the key list is
// positional, and the models a caller leaves out are retired.
func TestUpstreamUpdateRewritesKeysAndRetiresDeselectedModels(t *testing.T) {
	store := prepareUpstreamStore(t)
	row := createUpstream(t, store, map[string]any{
		"name": "Example", "url": "https://api.example.com",
		"keys": []any{"primary-key", "second-key"}, "models": []any{"gpt-4.1", "claude-3"},
	})
	siteID := row["id"].(int64)

	keys := stringsOf(t, row["keys"])
	// The console re-submits what it read: the first key is replaced, the second
	// is sent back as the mask the listing showed.
	updated, err := store.UpdateResource(context.Background(), "upstreams", siteID, map[string]any{
		"keys":   []any{"replacement-key", keys[1]},
		"models": []any{"gpt-4.1"},
		"name":   "Renamed",
	})
	if err != nil {
		t.Fatalf("UpdateResource(upstreams) error = %v", err)
	}
	if updated["name"] != "Renamed" {
		t.Errorf("name = %v, want Renamed", updated["name"])
	}
	if models := stringsOf(t, updated["models"]); len(models) != 1 || models[0] != "gpt-4.1" {
		t.Errorf("models = %v, want [gpt-4.1]", models)
	}

	accounts := rowsOf(t, store, "accounts")
	if accounts[0]["access_token"] != "replacement-key" {
		t.Errorf("access_token = %v, want replacement-key", accounts[0]["access_token"])
	}
	tokens := rowsOf(t, store, "tokens")
	if len(tokens) != 1 || tokens[0]["token"] == "" || IsMaskedSecret(tokens[0]["token"].(string)) {
		t.Fatalf("tokens = %v, want the stored second key kept intact", tokens)
	}

	// The deselected model's route is gone with its lines; the kept one stays.
	routes := rowsOf(t, store, "routes")
	if len(routes) != 1 || routes[0]["model_pattern"] != "gpt-4.1" {
		t.Fatalf("routes = %v, want only gpt-4.1", routes)
	}
}

// A route the operator wrote by hand is never pruned, even when a model is
// deselected and it is left with no lines.
func TestUpstreamKeepsHandWrittenRoutes(t *testing.T) {
	store := prepareUpstreamStore(t)
	handWritten := createManagedRow(t, store, "routes", map[string]any{
		"model_pattern": "gpt-4.1", "routing_strategy": "weighted",
	})
	row := createUpstream(t, store, map[string]any{
		"name": "Example", "url": "https://api.example.com",
		"keys": []any{"primary-key"}, "models": []any{"gpt-4.1"},
	})
	siteID := row["id"].(int64)

	if _, err := store.UpdateResource(context.Background(), "upstreams", siteID, map[string]any{"models": []any{}}); err != nil {
		t.Fatalf("UpdateResource(upstreams) error = %v", err)
	}
	routes := rowsOf(t, store, "routes")
	found := false
	for _, route := range routes {
		if route["id"] == handWritten {
			found = true
		}
	}
	if !found {
		t.Error("the hand-written route was deleted by an upstream edit")
	}
}

func TestUpstreamRequiresAKey(t *testing.T) {
	store := prepareUpstreamStore(t)
	_, err := store.CreateResource(context.Background(), "upstreams", map[string]any{
		"name": "Example", "url": "https://api.example.com", "keys": []any{}, "models": []any{"gpt-4.1"},
	})
	if err == nil {
		t.Fatal("CreateResource(upstreams) accepted an upstream without a key")
	}
}

func TestUpstreamRejectsAMappingForAnUnselectedModel(t *testing.T) {
	store := prepareUpstreamStore(t)
	_, err := store.CreateResource(context.Background(), "upstreams", map[string]any{
		"name": "Example", "url": "https://api.example.com",
		"keys":          []any{"primary-key"},
		"models":        []any{"gpt-4.1"},
		"model_mapping": map[string]any{"claude-3": "claude-3-5-sonnet"},
	})
	if err == nil {
		t.Fatal("CreateResource(upstreams) accepted a mapping for a model that is not selected")
	}
}

// Deleting an upstream takes its credentials, its lines and the routes that
// only existed for it.
func TestUpstreamDeleteRemovesEverythingItOwned(t *testing.T) {
	store := prepareUpstreamStore(t)
	row := createUpstream(t, store, map[string]any{
		"name": "Example", "url": "https://api.example.com",
		"keys": []any{"primary-key", "second-key"}, "models": []any{"gpt-4.1", "claude-3"},
	})
	siteID := row["id"].(int64)

	cascaded, err := store.DeleteResource(context.Background(), "upstreams", siteID)
	if err != nil {
		t.Fatalf("DeleteResource(upstreams) error = %v", err)
	}
	if cascaded["channels"] != 4 {
		t.Errorf("cascaded channels = %d, want 4", cascaded["channels"])
	}
	if cascaded["routes"] != 2 {
		t.Errorf("cascaded routes = %d, want 2", cascaded["routes"])
	}
	for resource, want := range map[string]int{"sites": 0, "accounts": 0, "tokens": 0, "routes": 0, "channels": 0} {
		if got := len(rowsOf(t, store, resource)); got != want {
			t.Errorf("%s rows = %d, want %d", resource, got, want)
		}
	}
}

// A model two upstreams serve is one route with a line from each, which is what
// makes the gateway fail over between them.
func TestUpstreamsShareOneRouteForASharedModel(t *testing.T) {
	store := prepareUpstreamStore(t)
	createUpstream(t, store, map[string]any{
		"name": "A", "url": "https://api.a.example",
		"keys": []any{"a-key"}, "models": []any{"gpt-4.1"},
	})
	createUpstream(t, store, map[string]any{
		"name": "B", "url": "https://api.b.example",
		"keys": []any{"b-key"}, "models": []any{"gpt-4.1"},
	})

	routes := rowsOf(t, store, "routes")
	if len(routes) != 1 {
		t.Fatalf("routes = %d, want 1 shared route", len(routes))
	}
	channels := rowsOf(t, store, "channels")
	if len(channels) != 2 {
		t.Fatalf("channels = %d, want one line per upstream", len(channels))
	}

	// Deleting one upstream leaves the other's line and the shared route.
	list, err := store.ListResource(context.Background(), "upstreams")
	if err != nil {
		t.Fatalf("ListResource(upstreams) error = %v", err)
	}
	first := list[0]["id"].(int64)
	cascaded, err := store.DeleteResource(context.Background(), "upstreams", first)
	if err != nil {
		t.Fatalf("DeleteResource(upstreams) error = %v", err)
	}
	if cascaded["routes"] != 0 {
		t.Errorf("cascaded routes = %d, want none: the route still serves the other upstream", cascaded["routes"])
	}
	if got := len(rowsOf(t, store, "channels")); got != 1 {
		t.Errorf("channels = %d, want the remaining upstream's line", got)
	}
	if got := len(rowsOf(t, store, "routes")); got != 1 {
		t.Errorf("routes = %d, want the shared route kept", got)
	}
}

// UpstreamKey is what the model probe uses: it has to hand back the real value,
// because the console is only ever shown a mask.
func TestUpstreamKeyReturnsTheStoredValue(t *testing.T) {
	store := prepareUpstreamStore(t)
	row := createUpstream(t, store, map[string]any{
		"name": "Example", "url": "https://api.example.com",
		"keys": []any{"primary-key", "second-key"}, "models": []any{"gpt-4.1"},
	})
	key, err := store.UpstreamKey(context.Background(), row["id"].(int64))
	if err != nil {
		t.Fatalf("UpstreamKey() error = %v", err)
	}
	if key != "primary-key" {
		t.Errorf("UpstreamKey() = %q, want primary-key", key)
	}
	if _, err := store.UpstreamKey(context.Background(), 9999); err == nil {
		t.Error("UpstreamKey() accepted an upstream that does not exist")
	}
}

// Replacing a line's channel is a new channel id, so the circuit recorded for
// the old one has to go with it: a stale cooldown would block a line that has
// never failed, because SQLite reuses the row id.
func TestUpstreamEditClearsTheCircuitsOfReplacedChannels(t *testing.T) {
	store := prepareUpstreamStore(t)
	if err := store.EnsureBreakerSchema(context.Background()); err != nil {
		t.Fatalf("EnsureBreakerSchema() error = %v", err)
	}
	row := createUpstream(t, store, map[string]any{
		"name": "Example", "url": "https://api.example.com",
		"keys": []any{"primary-key"}, "models": []any{"gpt-4.1"},
	})
	siteID := row["id"].(int64)

	channels := rowsOf(t, store, "channels")
	if len(channels) != 1 {
		t.Fatalf("channels = %d, want 1", len(channels))
	}
	channelID := fmt.Sprintf("%d", channels[0]["id"].(int64))
	if _, err := store.UpdateBreakerState(context.Background(), breaker.Scope{ChannelID: channelID}, func(state breaker.State) breaker.State {
		state.CooldownLevel = 2
		state.BlockedUntil = time.Now().Add(time.Hour)
		return state
	}); err != nil {
		t.Fatalf("UpdateBreakerState() error = %v", err)
	}

	if _, err := store.UpdateResource(context.Background(), "upstreams", siteID, map[string]any{"name": "Renamed"}); err != nil {
		t.Fatalf("UpdateResource(upstreams) error = %v", err)
	}
	// The name alone does not touch the lines, so the circuit stays.
	var count int64
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM gateway_breaker_states`).Scan(&count); err != nil {
		t.Fatalf("count breaker states: %v", err)
	}
	if count != 1 {
		t.Fatalf("breaker states after a rename = %d, want the recorded circuit kept", count)
	}

	if _, err := store.UpdateResource(context.Background(), "upstreams", siteID, map[string]any{"keys": []any{"primary-key", "second-key"}}); err != nil {
		t.Fatalf("UpdateResource(upstreams) error = %v", err)
	}
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM gateway_breaker_states`).Scan(&count); err != nil {
		t.Fatalf("count breaker states: %v", err)
	}
	if count != 0 {
		t.Errorf("breaker states after the lines were replaced = %d, want none", count)
	}
}

// Priority is what selection compares first, and it travels with the upstream
// rather than with any one line, so a key mode that orders an upstream's keys
// cannot lift them past a preferred upstream.
func TestUpstreamPriorityIsStoredAndLoadedOntoItsLines(t *testing.T) {
	store := prepareUpstreamStore(t)
	row := createUpstream(t, store, map[string]any{
		"name":     "Preferred",
		"url":      "https://api.example.com",
		"priority": 7,
		"keys":     []any{"key"},
		"models":   []any{"gpt-4.1"},
	})
	if row["priority"] != int64(7) {
		t.Fatalf("priority = %#v, want 7", row["priority"])
	}
	siteID := row["id"].(int64)

	loaded, err := store.LoadConfiguration(context.Background())
	if err != nil {
		t.Fatalf("LoadConfiguration() error = %v", err)
	}
	if len(loaded.Channels) != 1 {
		t.Fatalf("loaded %d channels, want 1", len(loaded.Channels))
	}
	if got := loaded.Channels[0].SitePriority; got != 7 {
		t.Fatalf("loaded channel SitePriority = %d, want 7", got)
	}

	// An upstream nobody gave a priority to sits at the default, which is what
	// every upstream had before the console could set one.
	plain := createUpstream(t, store, map[string]any{
		"name": "Plain", "url": "https://api.other.com", "keys": []any{"key"}, "models": []any{"gpt-4.1"},
	})
	if plain["priority"] != int64(0) {
		t.Fatalf("priority = %#v, want 0 by default", plain["priority"])
	}

	// Clearing it puts the upstream back at the default rather than storing 0.
	updated, err := store.UpdateResource(context.Background(), "upstreams", siteID, map[string]any{"priority": 0})
	if err != nil {
		t.Fatalf("UpdateResource(priority) error = %v", err)
	}
	if updated["priority"] != int64(0) {
		t.Fatalf("priority = %#v, want 0 after clearing", updated["priority"])
	}
	stored, err := store.LoadSettings(context.Background())
	if err != nil {
		t.Fatalf("LoadSettings() error = %v", err)
	}
	if value := stored[upstreamPrioritySetting]; value != "" && value != "{}" {
		t.Fatalf("stored priorities = %q, want the cleared upstream left out", value)
	}

	// Deleting the upstream forgets its priority, so a reused site id cannot
	// inherit it.
	if _, err := store.UpdateResource(context.Background(), "upstreams", siteID, map[string]any{"priority": 3}); err != nil {
		t.Fatalf("UpdateResource(priority) error = %v", err)
	}
	if _, err := store.DeleteResource(context.Background(), "upstreams", siteID); err != nil {
		t.Fatalf("DeleteResource(upstreams) error = %v", err)
	}
	priorities, err := upstreamPriorities(context.Background(), store.db)
	if err != nil {
		t.Fatalf("upstreamPriorities() error = %v", err)
	}
	if _, present := priorities[siteID]; present {
		t.Fatalf("priorities = %v, want the deleted upstream forgotten", priorities)
	}
}

// A model is exposed under its canonical name while the upstream keeps the
// spelling it listed, so two upstreams that call one model different things meet
// on one route.
func TestUpstreamExposesTheCanonicalModelName(t *testing.T) {
	store := prepareUpstreamStore(t)
	first := createUpstream(t, store, map[string]any{
		"name": "OpenRouter-like", "url": "https://api.one.example.com",
		"keys": []any{"key"}, "models": []any{"cline-free/deepseek-v4.1-flash:free"},
	})
	second := createUpstream(t, store, map[string]any{
		"name": "Direct", "url": "https://api.two.example.com",
		"keys": []any{"key"}, "models": []any{"DeepSeek-V4.1-Flash"},
	})

	models := stringsOf(t, first["models"])
	if len(models) != 1 || models[0] != "deepseek-v4.1-flash" {
		t.Fatalf("models = %v, want the canonical name", models)
	}
	if got := stringsOf(t, second["models"]); len(got) != 1 || got[0] != "deepseek-v4.1-flash" {
		t.Fatalf("models = %v, want both upstreams on the same exposed name", got)
	}

	// One route for the model, carrying a line per upstream.
	routes := rowsOf(t, store, "routes")
	if len(routes) != 1 {
		t.Fatalf("routes = %v, want one route for one model", routes)
	}
	routeID := routes[0]["id"].(int64)
	channels := 0
	sources := map[string]bool{}
	for _, channel := range rowsOf(t, store, "channels") {
		if channel["route_id"] != routeID {
			continue
		}
		channels++
		sources[channel["source_model"].(string)] = true
	}
	if channels != 2 {
		t.Fatalf("route has %d lines, want one per upstream", channels)
	}
	// Each line keeps the name its own upstream listed, which is what the
	// gateway forwards to it.
	if !sources["cline-free/deepseek-v4.1-flash:free"] || !sources["DeepSeek-V4.1-Flash"] {
		t.Fatalf("line source models = %v, want each upstream's own spelling", sources)
	}
}
