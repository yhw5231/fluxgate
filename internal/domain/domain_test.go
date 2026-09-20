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
