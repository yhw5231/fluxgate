package store

import (
	"context"
	"errors"
	"strconv"
	"testing"
)

// prepareManagedStore returns a store carrying the upstream schema the gateway
// creates on a fresh database, which is also the schema these writes target.
func prepareManagedStore(t *testing.T) *SQLiteStore {
	t.Helper()
	store := openTestStore(t)
	if err := store.EnsureUpstreamSchema(context.Background()); err != nil {
		t.Fatalf("EnsureUpstreamSchema() error = %v", err)
	}
	return store
}

// createManagedRow creates a row and returns its id, failing the test on error.
func createManagedRow(t *testing.T, store *SQLiteStore, resource string, values map[string]any) int64 {
	t.Helper()
	row, err := store.CreateResource(context.Background(), resource, values)
	if err != nil {
		t.Fatalf("CreateResource(%s) error = %v", resource, err)
	}
	id, ok := row["id"].(int64)
	if !ok {
		t.Fatalf("CreateResource(%s) returned no id: %v", resource, row)
	}
	return id
}

// upstreamFixture builds the smallest complete configuration: one site, one
// account, one route, and one channel connecting them.
type upstreamFixture struct {
	siteID    int64
	accountID int64
	routeID   int64
	channelID int64
}

func buildUpstreamFixture(t *testing.T, store *SQLiteStore) upstreamFixture {
	t.Helper()
	fixture := upstreamFixture{}
	fixture.siteID = createManagedRow(t, store, "sites", map[string]any{
		"name": "Example", "url": "https://api.example.com", "platform": "openai",
	})
	fixture.accountID = createManagedRow(t, store, "accounts", map[string]any{
		"site_id": fixture.siteID, "access_token": "upstream-secret",
	})
	fixture.routeID = createManagedRow(t, store, "routes", map[string]any{
		"model_pattern": "gpt-4.1", "routing_strategy": "weighted",
	})
	fixture.channelID = createManagedRow(t, store, "channels", map[string]any{
		"route_id": fixture.routeID, "account_id": fixture.accountID,
		"priority": 10, "weight": 20,
	})
	return fixture
}

func TestCreateResourceStoresEveryWritableField(t *testing.T) {
	store := prepareManagedStore(t)
	fixture := buildUpstreamFixture(t, store)

	tokenID := createManagedRow(t, store, "tokens", map[string]any{
		"account_id": fixture.accountID, "token": "token-secret", "enabled": true,
	})
	channelID := createManagedRow(t, store, "channels", map[string]any{
		"route_id":     fixture.routeID,
		"account_id":   fixture.accountID,
		"token_id":     tokenID,
		"source_model": "gpt-4.1-2025",
		"priority":     5,
		"weight":       3,
		"enabled":      false,
	})
	keyID := createManagedRow(t, store, "keys", map[string]any{
		"name":              "client-a",
		"key":               "sk-client-a",
		"expires_at":        "2027-01-31T09:00:00Z",
		"max_requests":      100,
		"supported_models":  `["gpt-4.1-mini"]`,
		"excluded_site_ids": "[9]",
	})
	proxyID := createManagedRow(t, store, "proxies", map[string]any{
		"name": "default-egress", "protocol": "socks5", "url": "socks5://127.0.0.1:1080", "is_default": true,
	})

	channels, err := store.ListResource(context.Background(), "channels")
	if err != nil {
		t.Fatalf("ListResource(channels) error = %v", err)
	}
	var found bool
	for _, channel := range channels {
		if channel["id"] != channelID {
			continue
		}
		found = true
		if channel["source_model"] != "gpt-4.1-2025" {
			t.Errorf("source_model = %v, want gpt-4.1-2025", channel["source_model"])
		}
		if channel["weight"] != int64(3) {
			t.Errorf("weight = %#v, want 3", channel["weight"])
		}
		if channel["enabled"] != false {
			t.Errorf("enabled = %#v, want false", channel["enabled"])
		}
		if channel["token_id"] != tokenID {
			t.Errorf("token_id = %#v, want %d", channel["token_id"], tokenID)
		}
	}
	if !found {
		t.Fatalf("channel %d is missing from the listing", channelID)
	}

	keys, err := store.ListResource(context.Background(), "keys")
	if err != nil {
		t.Fatalf("ListResource(keys) error = %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("key count = %d, want 1", len(keys))
	}
	if keys[0]["key"] != "sk-client-a" {
		t.Errorf("stored key = %v, want sk-client-a", keys[0]["key"])
	}
	if keys[0]["expires_at"] != "2027-01-31T09:00:00Z" {
		t.Errorf("expires_at = %v, want the RFC 3339 form", keys[0]["expires_at"])
	}
	if keys[0]["supported_models"] != `["gpt-4.1-mini"]` {
		t.Errorf("supported_models = %v", keys[0]["supported_models"])
	}
	if keys[0]["id"] != keyID {
		t.Errorf("key id = %v, want %d", keys[0]["id"], keyID)
	}

	proxies, err := store.ListResource(context.Background(), "proxies")
	if err != nil {
		t.Fatalf("ListResource(proxies) error = %v", err)
	}
	if len(proxies) != 1 || proxies[0]["is_default"] != true || proxies[0]["id"] != proxyID {
		t.Fatalf("proxy row = %#v", proxies)
	}

	configuration, err := store.LoadConfiguration(context.Background())
	if err != nil {
		t.Fatalf("LoadConfiguration() error = %v", err)
	}
	// The whole point of a write is that the gateway's own loader sees it.
	if len(configuration.Channels) != 2 {
		t.Fatalf("loaded channel count = %d, want 2", len(configuration.Channels))
	}
	if len(configuration.Routes) != 1 || len(configuration.Routes[0].Channels) != 2 {
		t.Fatalf("loaded routes = %#v", configuration.Routes)
	}
}

func TestCreateResourceRejectsIncompleteOrUnknownFields(t *testing.T) {
	store := prepareManagedStore(t)
	ctx := context.Background()

	_, err := store.CreateResource(ctx, "sites", map[string]any{"url": "https://api.example.com"})
	var invalid ValidationError
	if !errors.As(err, &invalid) {
		t.Fatalf("error = %v, want a ValidationError", err)
	}
	if invalid.Field != "name" || invalid.Reason != ReasonRequired {
		t.Fatalf("ValidationError = %#v, want the required reason on name", invalid)
	}

	_, err = store.CreateResource(ctx, "sites", map[string]any{
		"name": "Example", "url": "https://api.example.com", "platform": "openai", "nonsense": 1,
	})
	if !errors.As(err, &invalid) || invalid.Reason != ReasonUnknownField {
		t.Fatalf("error = %v, want an unknown-field ValidationError", err)
	}

	_, err = store.CreateResource(ctx, "sites", map[string]any{
		"name": "Example", "url": "ftp://api.example.com", "platform": "openai",
	})
	if !errors.As(err, &invalid) || invalid.Field != "url" {
		t.Fatalf("error = %v, want a url ValidationError", err)
	}

	_, err = store.CreateResource(ctx, "not-a-resource", map[string]any{})
	if !errors.Is(err, ErrUnknownResource) {
		t.Fatalf("error = %v, want ErrUnknownResource", err)
	}
}

func TestUpdateResourceAppliesOnlyNamedFieldsAndProtectsSecrets(t *testing.T) {
	store := prepareManagedStore(t)
	ctx := context.Background()
	fixture := buildUpstreamFixture(t, store)

	// An empty secret is what an untouched console input submits, so the stored
	// credential has to survive it.
	updated, err := store.UpdateResource(ctx, "accounts", fixture.accountID, map[string]any{
		"access_token": "", "status": "disabled",
	})
	if err != nil {
		t.Fatalf("UpdateResource(accounts) error = %v", err)
	}
	if updated["access_token"] != "upstream-secret" {
		t.Fatalf("access_token = %v, want the stored value kept", updated["access_token"])
	}
	if updated["status"] != "disabled" {
		t.Fatalf("status = %v, want disabled", updated["status"])
	}

	// A field the request omits keeps its value rather than being cleared.
	updated, err = store.UpdateResource(ctx, "sites", fixture.siteID, map[string]any{"global_weight": 2.5})
	if err != nil {
		t.Fatalf("UpdateResource(sites) error = %v", err)
	}
	if updated["name"] != "Example" {
		t.Fatalf("name = %v, want the stored value kept", updated["name"])
	}
	if updated["global_weight"] != 2.5 {
		t.Fatalf("global_weight = %#v, want 2.5", updated["global_weight"])
	}

	// Clearing a required field is refused rather than silently stored.
	_, err = store.UpdateResource(ctx, "sites", fixture.siteID, map[string]any{"name": ""})
	var invalid ValidationError
	if !errors.As(err, &invalid) || invalid.Reason != ReasonRequired {
		t.Fatalf("error = %v, want a required-field ValidationError", err)
	}

	// A clearable secret can be erased deliberately by sending null.
	tokenID := createManagedRow(t, store, "accounts", map[string]any{
		"site_id": fixture.siteID, "access_token": "first", "api_token": "second",
	})
	updated, err = store.UpdateResource(ctx, "accounts", tokenID, map[string]any{"api_token": nil})
	if err != nil {
		t.Fatalf("UpdateResource(accounts) error = %v", err)
	}
	if updated["api_token"] != nil {
		t.Fatalf("api_token = %v, want it cleared", updated["api_token"])
	}

	if _, err := store.UpdateResource(ctx, "sites", 9999, map[string]any{"name": "Gone"}); !errors.Is(err, ErrResourceNotFound) {
		t.Fatalf("error = %v, want ErrResourceNotFound", err)
	}
}

func TestDeleteResourceRefusesWhileReferenced(t *testing.T) {
	store := prepareManagedStore(t)
	ctx := context.Background()
	fixture := buildUpstreamFixture(t, store)

	_, err := store.DeleteResource(ctx, "sites", fixture.siteID)
	if !errors.Is(err, ErrResourceReferenced) {
		t.Fatalf("error = %v, want ErrResourceReferenced", err)
	}
	if _, err := store.DeleteResource(ctx, "accounts", fixture.accountID); !errors.Is(err, ErrResourceReferenced) {
		t.Fatalf("error = %v, want ErrResourceReferenced for the channel that uses the account", err)
	}
	if _, err := store.DeleteResource(ctx, "routes", fixture.routeID); err != nil {
		t.Fatalf("DeleteResource(routes) error = %v", err)
	}
	// Deleting the route took its channels with it, which is what unwires the
	// account and the site.
	if _, err := store.DeleteResource(ctx, "accounts", fixture.accountID); err != nil {
		t.Fatalf("DeleteResource(accounts) error = %v", err)
	}
	if _, err := store.DeleteResource(ctx, "sites", fixture.siteID); err != nil {
		t.Fatalf("DeleteResource(sites) error = %v", err)
	}

	channels, err := store.ListResource(ctx, "channels")
	if err != nil {
		t.Fatalf("ListResource(channels) error = %v", err)
	}
	if len(channels) != 0 {
		t.Fatalf("channels = %#v, want the route's channels removed with it", channels)
	}
}

func TestDeleteResourceReportsCascades(t *testing.T) {
	store := prepareManagedStore(t)
	ctx := context.Background()
	fixture := buildUpstreamFixture(t, store)

	createManagedRow(t, store, "tokens", map[string]any{"account_id": fixture.accountID, "token": "t1"})
	createManagedRow(t, store, "tokens", map[string]any{"account_id": fixture.accountID, "token": "t2"})

	if _, err := store.DeleteResource(ctx, "routes", fixture.routeID); err != nil {
		t.Fatalf("DeleteResource(routes) error = %v", err)
	}
	cascaded, err := store.DeleteResource(ctx, "accounts", fixture.accountID)
	if err != nil {
		t.Fatalf("DeleteResource(accounts) error = %v", err)
	}
	if cascaded["tokens"] != 2 {
		t.Fatalf("cascaded = %#v, want 2 tokens", cascaded)
	}
	tokens, err := store.ListResource(ctx, "tokens")
	if err != nil {
		t.Fatalf("ListResource(tokens) error = %v", err)
	}
	if len(tokens) != 0 {
		t.Fatalf("tokens = %#v, want none", tokens)
	}
}

func TestChannelRejectsTokenFromAnotherAccount(t *testing.T) {
	store := prepareManagedStore(t)
	ctx := context.Background()
	fixture := buildUpstreamFixture(t, store)

	otherAccount := createManagedRow(t, store, "accounts", map[string]any{
		"site_id": fixture.siteID, "access_token": "other",
	})
	otherToken := createManagedRow(t, store, "tokens", map[string]any{
		"account_id": otherAccount, "token": "other-token",
	})

	_, err := store.CreateResource(ctx, "channels", map[string]any{
		"route_id": fixture.routeID, "account_id": fixture.accountID, "token_id": otherToken,
	})
	var invalid ValidationError
	if !errors.As(err, &invalid) || invalid.Reason != ReasonReferenceMismatch {
		t.Fatalf("error = %v, want a reference-mismatch ValidationError", err)
	}
	if invalid.Field != "token_id" {
		t.Fatalf("field = %q, want token_id", invalid.Field)
	}

	_, err = store.CreateResource(ctx, "channels", map[string]any{
		"route_id": 9999, "account_id": fixture.accountID,
	})
	if !errors.As(err, &invalid) || invalid.Reason != ReasonMissingReference || invalid.Field != "route_id" {
		t.Fatalf("error = %v, want a missing-reference ValidationError on route_id", err)
	}
}

func TestRouteModelMappingAndGroupSources(t *testing.T) {
	store := prepareManagedStore(t)
	ctx := context.Background()
	fixture := buildUpstreamFixture(t, store)

	stored, err := store.UpdateResource(ctx, "routes", fixture.routeID, map[string]any{
		"model_mapping": `{"z-model": "target-z", "a-model": "target-a"}`,
	})
	if err != nil {
		t.Fatalf("UpdateResource(routes) error = %v", err)
	}
	// The declaration order decides which pattern wins, so it has to survive.
	if stored["model_mapping"] != `{"z-model":"target-z","a-model":"target-a"}` {
		t.Fatalf("model_mapping = %v, want the written key order preserved", stored["model_mapping"])
	}

	groupID := createManagedRow(t, store, "routes", map[string]any{
		"model_pattern": "group", "route_mode": "explicit_group", "display_name": "bundle",
		"source_route_ids": "[" + strconv.FormatInt(fixture.routeID, 10) + "]",
	})
	configuration, err := store.LoadConfiguration(ctx)
	if err != nil {
		t.Fatalf("LoadConfiguration() error = %v", err)
	}
	var groupFound bool
	for _, route := range configuration.Routes {
		if route.ID != groupID {
			continue
		}
		groupFound = true
		if len(route.SourceRouteIDs) != 1 || route.SourceRouteIDs[0] != fixture.routeID {
			t.Fatalf("group source routes = %v, want the source route that was set", route.SourceRouteIDs)
		}
	}
	if !groupFound {
		t.Fatal("the group route is missing from the loaded configuration")
	}

	// The source route is still referenced by the group, so removing it is refused.
	if _, err := store.DeleteResource(ctx, "routes", fixture.routeID); !errors.Is(err, ErrResourceReferenced) {
		t.Fatalf("error = %v, want ErrResourceReferenced", err)
	}

	// Clearing the field is a deliberate unwiring and then the delete succeeds.
	if _, err := store.UpdateResource(ctx, "routes", groupID, map[string]any{"source_route_ids": nil}); err != nil {
		t.Fatalf("UpdateResource(routes) error = %v", err)
	}
	if _, err := store.DeleteResource(ctx, "routes", fixture.routeID); err != nil {
		t.Fatalf("DeleteResource(routes) error = %v", err)
	}
}

func TestDownstreamKeyValueHasToStayUnique(t *testing.T) {
	store := prepareManagedStore(t)
	ctx := context.Background()

	first := createManagedRow(t, store, "keys", map[string]any{"name": "a", "key": "sk-shared"})
	_, err := store.CreateResource(ctx, "keys", map[string]any{"name": "b", "key": "sk-shared"})
	if !errors.Is(err, ErrDuplicateValue) {
		t.Fatalf("error = %v, want ErrDuplicateValue", err)
	}

	// Renaming one row to its own value is not a conflict with itself.
	if _, err := store.UpdateResource(ctx, "keys", first, map[string]any{"name": "renamed", "key": "sk-shared"}); err != nil {
		t.Fatalf("UpdateResource(keys) error = %v", err)
	}

	second := createManagedRow(t, store, "keys", map[string]any{"name": "c", "key": "sk-other"})
	_, err = store.UpdateResource(ctx, "keys", second, map[string]any{"key": "sk-shared"})
	if !errors.Is(err, ErrDuplicateValue) {
		t.Fatalf("error = %v, want ErrDuplicateValue", err)
	}
}

func TestListResourceReturnsEveryRowInConsoleOrder(t *testing.T) {
	store := prepareManagedStore(t)
	ctx := context.Background()
	fixture := buildUpstreamFixture(t, store)

	createManagedRow(t, store, "channels", map[string]any{
		"route_id": fixture.routeID, "account_id": fixture.accountID, "priority": 90,
	})
	createManagedRow(t, store, "channels", map[string]any{
		"route_id": fixture.routeID, "account_id": fixture.accountID, "priority": 50,
	})

	channels, err := store.ListResource(ctx, "channels")
	if err != nil {
		t.Fatalf("ListResource(channels) error = %v", err)
	}
	if len(channels) != 3 {
		t.Fatalf("channel count = %d, want 3", len(channels))
	}
	// Highest priority first, which is the order the selector prefers.
	if channels[0]["priority"] != int64(90) || channels[2]["priority"] != int64(10) {
		t.Fatalf("channel order = %v, %v, %v", channels[0]["priority"], channels[1]["priority"], channels[2]["priority"])
	}
	if _, err := store.ListResource(ctx, "nope"); !errors.Is(err, ErrUnknownResource) {
		t.Fatalf("error = %v, want ErrUnknownResource", err)
	}
}

// The loader switches on these strings exactly, so an accepted variant has to be
// stored as the declared spelling rather than as whatever case arrived.
func TestChoiceValuesAreStoredInTheirDeclaredSpelling(t *testing.T) {
	store := prepareManagedStore(t)
	ctx := context.Background()
	fixture := buildUpstreamFixture(t, store)

	stored, err := store.UpdateResource(ctx, "routes", fixture.routeID, map[string]any{"routing_strategy": "Round_Robin"})
	if err != nil {
		t.Fatalf("UpdateResource(routes) error = %v", err)
	}
	if stored["routing_strategy"] != "round_robin" {
		t.Fatalf("routing_strategy = %v, want the declared spelling", stored["routing_strategy"])
	}

	_, err = store.UpdateResource(ctx, "routes", fixture.routeID, map[string]any{"routing_strategy": "fastest"})
	var invalid ValidationError
	if !errors.As(err, &invalid) || invalid.Reason != ReasonNotAllowed {
		t.Fatalf("error = %v, want a not-allowed ValidationError", err)
	}
}

// Source routes are only meaningful on an explicit group route, so storing them
// on a pattern route would leave a field that looks configured and does nothing.
func TestGroupSourcesAreRefusedOnANonGroupRoute(t *testing.T) {
	store := prepareManagedStore(t)
	ctx := context.Background()
	fixture := buildUpstreamFixture(t, store)

	_, err := store.UpdateResource(ctx, "routes", fixture.routeID, map[string]any{"source_route_ids": "[1]"})
	var invalid ValidationError
	if !errors.As(err, &invalid) || invalid.Field != "source_route_ids" {
		t.Fatalf("error = %v, want a source_route_ids ValidationError", err)
	}

	// The same request is accepted once the route is a group.
	groupID := createManagedRow(t, store, "routes", map[string]any{"model_pattern": "bundle", "route_mode": "explicit_group"})
	if _, err := store.UpdateResource(ctx, "routes", groupID, map[string]any{"source_route_ids": "[" + strconv.FormatInt(fixture.routeID, 10) + "]"}); err != nil {
		t.Fatalf("UpdateResource(routes) error = %v", err)
	}
}

// The console shows an untouched checkbox as checked or not according to the
// column's declared default, so that declaration has to be what the database
// actually stores when a create omits the flag. A site created from the console
// once switched on the system proxy this way.
func TestFlagDefaultsMatchWhatAnOmittedCreateStores(t *testing.T) {
	store := prepareManagedStore(t)
	ctx := context.Background()

	defaults := map[string]bool{}
	for _, resource := range Resources() {
		for _, column := range resource.Columns {
			if column.Kind == KindBool {
				defaults[resource.Name+"."+column.Name] = column.Default
			}
		}
	}
	// The schema declares these two off, and every enabled flag on.
	if defaults["sites.use_system_proxy"] {
		t.Error("sites.use_system_proxy declares a default of true, but the schema stores 0")
	}
	if defaults["tokens.use_system_proxy"] || defaults["proxies.is_default"] {
		t.Error("a flag the schema stores as 0 declares a default of true")
	}
	for _, name := range []string{"tokens.enabled", "routes.enabled", "channels.enabled", "keys.enabled", "proxies.enabled"} {
		if !defaults[name] {
			t.Errorf("%s declares a default of false, but the schema stores 1", name)
		}
	}

	fixture := buildUpstreamFixture(t, store)
	site, err := store.CreateResource(ctx, "sites", map[string]any{
		"name": "Defaults", "url": "https://defaults.example.com", "platform": "openai",
	})
	if err != nil {
		t.Fatalf("CreateResource(sites) error = %v", err)
	}
	if site["use_system_proxy"] != false {
		t.Fatalf("use_system_proxy = %v, want the schema default of false", site["use_system_proxy"])
	}
	token, err := store.CreateResource(ctx, "tokens", map[string]any{"account_id": fixture.accountID, "token": "t"})
	if err != nil {
		t.Fatalf("CreateResource(tokens) error = %v", err)
	}
	if token["enabled"] != true {
		t.Fatalf("enabled = %v, want the schema default of true", token["enabled"])
	}
}

func TestEveryResourceHasAnAddressableUniqueName(t *testing.T) {
	seen := map[string]bool{}
	for _, resource := range Resources() {
		if resource.Name == "" || resource.Table == "" {
			t.Fatalf("resource %#v is missing a name or table", resource)
		}
		if seen[resource.Name] {
			t.Fatalf("resource name %q is used twice", resource.Name)
		}
		seen[resource.Name] = true
		if _, ok := FindResource(resource.Name); !ok {
			t.Fatalf("FindResource(%q) did not resolve", resource.Name)
		}
		for _, column := range resource.Columns {
			if column.Name == "id" {
				t.Fatalf("resource %q declares id as writable, but it is managed by the store", resource.Name)
			}
		}
	}
}
