package domain

import (
	"testing"
)

func TestModelMappingResolvePrefersExactKeyThenDeclarationOrder(t *testing.T) {
	mapping := ModelMapping{
		{Pattern: "gpt-*", Target: "first-target"},
		{Pattern: "*-mini", Target: "second-target"},
		{Pattern: "gpt-4.1", Target: "exact-target"},
	}

	// An exact key wins even though it is declared last.
	if got := mapping.Resolve("GPT-4.1"); got != "exact-target" {
		t.Fatalf("Resolve(GPT-4.1) = %q, want exact-target", got)
	}
	// Overlapping globs resolve by declaration order, not map iteration order.
	for attempt := 0; attempt < 20; attempt++ {
		if got := mapping.Resolve("gpt-4o-mini"); got != "first-target" {
			t.Fatalf("Resolve(gpt-4o-mini) = %q, want the first declared match", got)
		}
	}
	if got := mapping.Resolve("claude-3"); got != "claude-3" {
		t.Fatalf("Resolve(claude-3) = %q, want the requested model", got)
	}
}

func TestModelMappingResolveIgnoresEmptyTargets(t *testing.T) {
	mapping := ModelMapping{
		{Pattern: "gpt-*", Target: "  "},
		{Pattern: "*", Target: "fallback"},
	}
	if got := mapping.Resolve("gpt-4o"); got != "fallback" {
		t.Fatalf("Resolve() = %q, want fallback", got)
	}
}

// supported_models is an exclusion list: a matching model is denied.
func TestAllowsModelTreatsPatternsAsDenyList(t *testing.T) {
	routes := []Route{{ID: 1, ModelPattern: "gpt-*", Mode: RouteModePattern, Enabled: true}}
	policy := RoutingPolicy{DeniedModelPatterns: []string{"gpt-4.1", "claude-*"}}

	if AllowsModel(routes, policy, "claude-sonnet") {
		t.Fatal("a model matching a deny pattern was allowed")
	}
	if AllowsModel(routes, policy, "gpt-4.1") {
		t.Fatal("an exactly denied model was allowed")
	}
	if !AllowsModel(routes, policy, "gpt-4o") {
		t.Fatal("a model outside the deny list was rejected")
	}
}

// allowed_route_ids exposes only the selected routes, addressed by their public
// name (display name when set, otherwise the pattern). An exact route pattern
// is exempt so a directly named model stays reachable.
func TestAllowsModelAppliesRouteScopeWithExactPatternExemption(t *testing.T) {
	routes := []Route{
		{ID: 1, ModelPattern: "gpt-*", DisplayName: "team-gpt", Mode: RouteModePattern, Enabled: true},
		{ID: 2, ModelPattern: "claude-*", DisplayName: "team-claude", Mode: RouteModePattern, Enabled: true},
		{ID: 3, ModelPattern: "exact-model", Mode: RouteModePattern, Enabled: true},
	}
	policy := RoutingPolicy{AllowedRouteIDs: []int64{1}}

	if !AllowsModel(routes, policy, "team-gpt") {
		t.Fatal("the allowed route's public name was rejected")
	}
	if AllowsModel(routes, policy, "team-claude") {
		t.Fatal("a route outside allowed_route_ids was reachable by its public name")
	}
	if AllowsModel(routes, policy, "gpt-4o") {
		t.Fatal("a private model name behind a glob route was exposed")
	}
	// An exact route pattern stays visible regardless of the allow list.
	if !AllowsModel(routes, policy, "exact-model") {
		t.Fatal("an exact route pattern was hidden by allowed_route_ids")
	}
}

// An unrestricted policy permits any name at the policy layer. Whether a route
// and channel actually exist is decided by selection, so a disabled route is
// rejected there rather than here.
func TestAllowsModelDoesNotEnforceRouteAvailability(t *testing.T) {
	disabled := []Route{{ID: 1, ModelPattern: "globa-*", Mode: RouteModePattern, Enabled: false}}
	if !AllowsModel(disabled, RoutingPolicy{}, "globa-1") {
		t.Fatal("the policy layer rejected a model before route resolution")
	}
}

// allowed_site_ids is an allow list that reads like an absent one when it is
// empty, so a key that never selected an upstream keeps reaching all of them
// while a key that did select reaches only those.
func TestAllowsSiteTreatsAnEmptyListAsUnrestricted(t *testing.T) {
	unrestricted := RoutingPolicy{}
	if !unrestricted.AllowsSite(1) || !unrestricted.AllowsSite(99) {
		t.Fatal("an empty allow list restricted the key")
	}

	restricted := RoutingPolicy{AllowedSiteIDs: []int64{3, 5}}
	if !restricted.AllowsSite(3) || !restricted.AllowsSite(5) {
		t.Fatal("a site on the allow list was rejected")
	}
	if restricted.AllowsSite(4) {
		t.Fatal("a site outside the allow list was permitted")
	}

	// A site that is both allowed and excluded stays out of reach: the allow list
	// answers for itself and the exclusion is applied on top of it.
	both := RoutingPolicy{AllowedSiteIDs: []int64{3}, ExcludedSiteIDs: []int64{3}}
	if !both.AllowsSite(3) || !both.ExcludesSite(3) {
		t.Fatal("the allow list and the exclusion did not both answer for the site")
	}
}

func TestExposedModelsHidesRoutesCoveredByAGroup(t *testing.T) {
	routes := []Route{
		{ID: 1, ModelPattern: "gpt-*", Mode: RouteModePattern, Enabled: true},
		{ID: 2, ModelPattern: "claude-opus-4-5", Mode: RouteModePattern, Enabled: true},
		{
			ID:             3,
			DisplayName:    "claude-opus-4-6",
			Mode:           RouteModeExplicitGroup,
			Enabled:        true,
			SourceRouteIDs: []int64{2},
		},
	}

	models := ExposedModels(routes)
	if len(models) != 2 || models[0] != "claude-opus-4-6" || models[1] != "gpt-*" {
		t.Fatalf("ExposedModels() = %#v, want [claude-opus-4-6 gpt-*]", models)
	}
}

func TestExposedModelsSkipsDisabledRoutesAndGroupsWithoutAlias(t *testing.T) {
	routes := []Route{
		{ID: 1, ModelPattern: "gpt-*", Mode: RouteModePattern, Enabled: false},
		{ID: 2, ModelPattern: "claude-*", Mode: RouteModePattern, Enabled: true},
		{ID: 3, DisplayName: "unnamed-group", Mode: RouteModeExplicitGroup, Enabled: false},
	}

	models := ExposedModels(routes)
	if len(models) != 1 || models[0] != "claude-*" {
		t.Fatalf("ExposedModels() = %#v, want [claude-*]", models)
	}
}

func TestAllowsModelIgnoresEmptyPolicy(t *testing.T) {
	routes := []Route{{ID: 1, ModelPattern: "gpt-*", Mode: RouteModePattern, Enabled: true}}
	if !AllowsModel(routes, RoutingPolicy{}, "anything") {
		t.Fatal("an unrestricted policy rejected a model")
	}
}

// A mapping written for one spelling of a model answers for the others, so an
// operator who wrote the mapping once does not have to write it per channel.
func TestModelMappingResolveMatchesEverySpellingOfTheModel(t *testing.T) {
	mapping := ModelMapping{{Pattern: "deepseek-v4.1-flash", Target: "cline-free/deepseek-v4.1-flash:free"}}

	for _, requested := range []string{
		"deepseek-v4.1-flash",
		"DeepSeek-V4.1-Flash",
		"cline-free/deepseek-v4.1-flash:free",
	} {
		if got := mapping.Resolve(requested); got != "cline-free/deepseek-v4.1-flash:free" {
			t.Errorf("Resolve(%q) = %q, want the mapped target", requested, got)
		}
	}
	if got := mapping.Resolve("deepseek-v4.1-pro"); got != "deepseek-v4.1-pro" {
		t.Errorf("Resolve(%q) = %q, want the requested name when nothing maps", "deepseek-v4.1-pro", got)
	}
}

// A restriction written for the plain model name covers the decorated spellings
// of it, or a key could reach a denied model by asking for a channel's own name
// for it.
func TestDeniedModelPatternsCoverEverySpelling(t *testing.T) {
	policy := RoutingPolicy{DeniedModelPatterns: []string{"deepseek-v4.1-flash"}}

	for _, model := range []string{
		"deepseek-v4.1-flash",
		"DeepSeek-V4.1-Flash",
		"cline-free/deepseek-v4.1-flash:free",
	} {
		if !policy.DeniesModel(model) {
			t.Errorf("DeniesModel(%q) = false, want the denied model refused", model)
		}
	}
	if policy.DeniesModel("deepseek-v4.1-pro") {
		t.Error("DeniesModel() refused a model the policy does not deny")
	}
}

// A key scoped to a route reaches that route under every spelling of the model
// the route exposes.
func TestAllowsModelAcceptsEverySpellingOfAnAllowedRoute(t *testing.T) {
	routes := []Route{{
		ID:           9,
		ModelPattern: "deepseek-v4.1-flash",
		Mode:         RouteModePattern,
		Enabled:      true,
	}}
	policy := RoutingPolicy{AllowedRouteIDs: []int64{9}}

	for _, model := range []string{"deepseek-v4.1-flash", "DeepSeek-V4.1-Flash", "cline-free/deepseek-v4.1-flash:free"} {
		if !AllowsModel(routes, policy, model) {
			t.Errorf("AllowsModel(%q) = false, want the allowed route's model permitted", model)
		}
	}
}
