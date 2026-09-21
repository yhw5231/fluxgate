package router

import (
	"testing"

	"github.com/yhw5231/fluxgate/internal/domain"
)

// route builds an enabled pattern route. Channels are used exactly as given.
func route(id int64, modelPattern string, channels ...domain.Channel) domain.Route {
	return domain.Route{
		ID:           id,
		ModelPattern: modelPattern,
		Mode:         domain.RouteModePattern,
		Enabled:      true,
		Channels:     channels,
	}
}

func channel(id string, priority int, weight int) domain.Channel {
	return domain.Channel{ID: id, Enabled: true, Priority: priority, Weight: weight}
}

func selectChannel(t *testing.T, selector *MemorySelector, model string, policy domain.RoutingPolicy) domain.Selection {
	t.Helper()
	selection, err := selector.Select(domain.SelectionRequest{Model: model, Policy: policy})
	if err != nil {
		t.Fatalf("Select(%q) error = %v", model, err)
	}
	return selection
}

func TestMemorySelectorPrefersHighestPriorityAndMapsModel(t *testing.T) {
	selector := NewMemorySelector([]domain.Route{{
		ID:           1,
		ModelPattern: "gpt-*",
		Mode:         domain.RouteModePattern,
		Enabled:      true,
		ModelMapping: domain.ModelMapping{{Pattern: "gpt-*", Target: "mapped-model"}},
		Channels:     []domain.Channel{channel("lower", 10, 1), channel("higher", 20, 1)},
	}})

	selection := selectChannel(t, selector, "gpt-4.1", domain.RoutingPolicy{})
	if selection.Channel.ID != "higher" {
		t.Fatalf("selected channel = %q, want higher", selection.Channel.ID)
	}
	if selection.Model != "mapped-model" {
		t.Fatalf("selected model = %q, want mapped-model", selection.Model)
	}
}

func TestMemorySelectorUsesSmoothWeightedRoundRobinWithinPriority(t *testing.T) {
	selector := NewMemorySelector([]domain.Route{route(1, "*",
		channel("heavy", 10, 3),
		channel("light", 10, 1),
	)})

	counts := map[string]int{}
	for index := 0; index < 8; index++ {
		counts[selectChannel(t, selector, "model", domain.RoutingPolicy{}).Channel.ID]++
	}

	if counts["heavy"] != 6 || counts["light"] != 2 {
		t.Fatalf("weighted counts = %#v, want heavy:6 light:2", counts)
	}
}

func TestMemorySelectorHonorsExclusionsAndDisabledChannels(t *testing.T) {
	selector := NewMemorySelector([]domain.Route{route(1, "*",
		domain.Channel{ID: "disabled", Enabled: false, Priority: 30, Weight: 1},
		channel("primary", 20, 1),
		channel("fallback", 10, 1),
	)})

	selection, err := selector.Select(domain.SelectionRequest{
		Model:    "model",
		Excluded: map[string]struct{}{"primary": {}},
	})
	if err != nil {
		t.Fatalf("Select() error = %v", err)
	}
	if selection.Channel.ID != "fallback" {
		t.Fatalf("selected channel = %q, want fallback", selection.Channel.ID)
	}
}

func TestMemorySelectorReturnsErrorWhenNoChannelIsEligible(t *testing.T) {
	selector := NewMemorySelector([]domain.Route{route(1, "*",
		domain.Channel{ID: "disabled", Enabled: false, Priority: 10, Weight: 1},
	)})

	_, err := selector.Select(domain.SelectionRequest{Model: "model"})
	if err != ErrNoChannel {
		t.Fatalf("Select() error = %v, want ErrNoChannel", err)
	}
}

// A channel may only serve the models its own route declares. Without this the
// gateway forwards requests to an upstream that never offered the model.
func TestMemorySelectorOnlyUsesChannelsOfTheMatchingRoute(t *testing.T) {
	selector := NewMemorySelector([]domain.Route{
		route(1, "alpha-only", channel("alpha-channel", 0, 1)),
		route(2, "beta-only", channel("beta-channel", 99, 1)),
	})

	if got := selectChannel(t, selector, "alpha-only", domain.RoutingPolicy{}).Channel.ID; got != "alpha-channel" {
		t.Fatalf("selected channel = %q, want alpha-channel", got)
	}
	if got := selectChannel(t, selector, "beta-only", domain.RoutingPolicy{}).Channel.ID; got != "beta-channel" {
		t.Fatalf("selected channel = %q, want beta-channel", got)
	}
}

func TestMemorySelectorRejectsModelsThatMatchNoRoute(t *testing.T) {
	selector := NewMemorySelector([]domain.Route{route(1, "alpha-only", channel("alpha-channel", 0, 1))})

	_, err := selector.Select(domain.SelectionRequest{Model: "unknown-model"})
	if err != ErrModelNotRoutable {
		t.Fatalf("Select() error = %v, want ErrModelNotRoutable", err)
	}
	if selector.HasCandidate("unknown-model", domain.RoutingPolicy{}) {
		t.Fatal("HasCandidate() = true for an unroutable model")
	}
}

func TestMemorySelectorPrefersExactPatternOverGlob(t *testing.T) {
	selector := NewMemorySelector([]domain.Route{
		route(1, "gpt-*", channel("glob-channel", 50, 1)),
		route(2, "gpt-4.1", channel("exact-channel", 0, 1)),
	})

	if got := selectChannel(t, selector, "gpt-4.1", domain.RoutingPolicy{}).Channel.ID; got != "exact-channel" {
		t.Fatalf("selected channel = %q, want exact-channel", got)
	}
	if got := selectChannel(t, selector, "gpt-4o", domain.RoutingPolicy{}).Channel.ID; got != "glob-channel" {
		t.Fatalf("selected channel = %q, want glob-channel", got)
	}
}

func TestMemorySelectorResolvesGroupByDisplayName(t *testing.T) {
	group := domain.Route{
		ID:             10,
		DisplayName:    "claude-opus-4-6",
		Mode:           domain.RouteModeExplicitGroup,
		Enabled:        true,
		SourceRouteIDs: []int64{11},
	}
	source := route(11, "claude-opus-4-5", domain.Channel{
		ID: "group-source", Enabled: true, Weight: 1, SourceModel: "claude-opus-4-5",
	})
	selector := NewMemorySelector([]domain.Route{group, source})

	selection := selectChannel(t, selector, "claude-opus-4-6", domain.RoutingPolicy{})
	if selection.Channel.ID != "group-source" {
		t.Fatalf("selected channel = %q, want group-source", selection.Channel.ID)
	}
	// A display-name request is forwarded with the source route's model.
	if selection.Model != "claude-opus-4-5" {
		t.Fatalf("selected model = %q, want claude-opus-4-5", selection.Model)
	}
	// The group's own pattern must not be matched as a model name.
	if !selector.HasCandidate("claude-opus-4-5", domain.RoutingPolicy{}) {
		t.Fatal("source route should stay selectable through its own model")
	}
}

func TestMemorySelectorFiltersBySourceModel(t *testing.T) {
	selector := NewMemorySelector([]domain.Route{route(1, "gpt-*", domain.Channel{
		ID: "gpt-4o-only", Enabled: true, Weight: 1, SourceModel: "gpt-4o",
	})})

	if got := selectChannel(t, selector, "gpt-4o", domain.RoutingPolicy{}).Channel.ID; got != "gpt-4o-only" {
		t.Fatalf("selected channel = %q, want gpt-4o-only", got)
	}
	if _, err := selector.Select(domain.SelectionRequest{Model: "gpt-4.1"}); err != ErrNoChannel {
		t.Fatalf("Select() error = %v, want ErrNoChannel for a model the channel does not serve", err)
	}
}

func TestMemorySelectorTreatsSourceModelAsPatternAndAlias(t *testing.T) {
	t.Run("glob source model", func(t *testing.T) {
		selector := NewMemorySelector([]domain.Route{route(1, "*", domain.Channel{
			ID: "pattern", Enabled: true, Weight: 1, SourceModel: "gpt-4o-*",
		})})
		if got := selectChannel(t, selector, "gpt-4o-2024-11-20", domain.RoutingPolicy{}).Channel.ID; got != "pattern" {
			t.Fatalf("selected channel = %q", got)
		}
	})

	t.Run("vendor prefix and free suffix are equivalent", func(t *testing.T) {
		selector := NewMemorySelector([]domain.Route{route(1, "*", domain.Channel{
			ID: "alias", Enabled: true, Weight: 1, SourceModel: "deepseek-ai/deepseek-v4-free",
		})})
		if got := selectChannel(t, selector, "deepseek-v4", domain.RoutingPolicy{}).Channel.ID; got != "alias" {
			t.Fatalf("selected channel = %q", got)
		}
	})
}

func TestMemorySelectorAppliesDownstreamPolicyRestrictions(t *testing.T) {
	tokenID := int64(42)
	newSelector := func() *MemorySelector {
		return NewMemorySelector([]domain.Route{route(1, "*",
			domain.Channel{ID: "site-7", Enabled: true, Weight: 1, SiteID: 7, AccountID: 1, TokenID: &tokenID},
			domain.Channel{ID: "site-8", Enabled: true, Weight: 1, SiteID: 8, AccountID: 2},
		)})
	}

	t.Run("denied model pattern wins over routing", func(t *testing.T) {
		policy := domain.RoutingPolicy{DeniedModelPatterns: []string{"blocked-*"}}
		if got := selectChannel(t, newSelector(), "allowed-model", policy).Channel.ID; got != "site-7" {
			t.Fatalf("selected channel = %q, want site-7", got)
		}
		if _, err := newSelector().Select(domain.SelectionRequest{Model: "blocked-model", Policy: policy}); err != ErrModelNotRoutable {
			t.Fatalf("Select() error = %v, want ErrModelNotRoutable", err)
		}
		if newSelector().HasCandidate("blocked-model", policy) {
			t.Fatal("HasCandidate() = true for a denied model")
		}
	})

	t.Run("excluded site removes its channel", func(t *testing.T) {
		policy := domain.RoutingPolicy{ExcludedSiteIDs: []int64{7}}
		if got := selectChannel(t, newSelector(), "model", policy).Channel.ID; got != "site-8" {
			t.Fatalf("selected channel = %q, want site-8", got)
		}
	})

	t.Run("excluded credential requires every identifier to match", func(t *testing.T) {
		policy := domain.RoutingPolicy{ExcludedCredentials: []domain.ExcludedCredential{
			{Kind: "account_token", SiteID: 7, AccountID: 1, TokenID: 42},
		}}
		if got := selectChannel(t, newSelector(), "model", policy).Channel.ID; got != "site-8" {
			t.Fatalf("selected channel = %q, want site-8", got)
		}

		mismatched := domain.RoutingPolicy{ExcludedCredentials: []domain.ExcludedCredential{
			{Kind: "account_token", SiteID: 7, AccountID: 1, TokenID: 43},
		}}
		if got := selectChannel(t, newSelector(), "model", mismatched).Channel.ID; got != "site-7" {
			t.Fatalf("selected channel = %q, want site-7", got)
		}
	})

	t.Run("a channel without a token is never excluded by a token reference", func(t *testing.T) {
		selector := NewMemorySelector([]domain.Route{route(1, "*",
			domain.Channel{ID: "no-token", Enabled: true, Weight: 1, SiteID: 7, AccountID: 1},
		)})
		policy := domain.RoutingPolicy{ExcludedCredentials: []domain.ExcludedCredential{
			{Kind: "account_token", SiteID: 7, AccountID: 1, TokenID: 42},
		}}
		if got := selectChannel(t, selector, "model", policy).Channel.ID; got != "no-token" {
			t.Fatalf("selected channel = %q, want no-token", got)
		}
	})

	t.Run("allowed route ids restrict visible glob routes", func(t *testing.T) {
		selector := NewMemorySelector([]domain.Route{
			route(1, "model-a-*", channel("route-1", 0, 1)),
			route(2, "model-b-*", channel("route-2", 0, 1)),
		})
		policy := domain.RoutingPolicy{AllowedRouteIDs: []int64{1}}
		if got := selectChannel(t, selector, "model-a-1", policy).Channel.ID; got != "route-1" {
			t.Fatalf("selected channel = %q, want route-1", got)
		}
		if selector.HasCandidate("model-b-1", policy) {
			t.Fatal("a glob route outside allowed_route_ids was still selectable")
		}
	})

	t.Run("an exact route pattern stays visible regardless of allowed route ids", func(t *testing.T) {
		selector := NewMemorySelector([]domain.Route{
			route(1, "model-a-*", channel("route-1", 0, 1)),
			route(2, "model-b", channel("route-2", 0, 1)),
		})
		policy := domain.RoutingPolicy{AllowedRouteIDs: []int64{1}}
		if got := selectChannel(t, selector, "model-b", policy).Channel.ID; got != "route-2" {
			t.Fatalf("selected channel = %q, want route-2", got)
		}
	})
}

func TestMemorySelectorUsesSiteWeightAndMultiplier(t *testing.T) {
	selector := NewMemorySelector([]domain.Route{route(1, "*",
		domain.Channel{ID: "base", Enabled: true, Weight: 10, SiteID: 1, SiteGlobalWeight: 1},
		domain.Channel{ID: "double", Enabled: true, Weight: 10, SiteID: 2, SiteGlobalWeight: 2},
	)})

	// Site 2 carries twice the weight, so it wins the first draw.
	if got := selectChannel(t, selector, "model", domain.RoutingPolicy{}).Channel.ID; got != "double" {
		t.Fatalf("selected channel = %q, want double", got)
	}

	rebalanced := NewMemorySelector([]domain.Route{route(1, "*",
		domain.Channel{ID: "base", Enabled: true, Weight: 10, SiteID: 1, SiteGlobalWeight: 1},
		domain.Channel{ID: "double", Enabled: true, Weight: 10, SiteID: 2, SiteGlobalWeight: 2},
	)})
	policy := domain.RoutingPolicy{SiteMultipliers: map[int64]float64{2: 0.1}}
	if got := selectChannel(t, rebalanced, "model", policy).Channel.ID; got != "base" {
		t.Fatalf("selected channel = %q, want base once the multiplier is lowered", got)
	}
}

// An exact route exposes a friendly name while the upstream expects the vendor
// name, so the source model is what gets forwarded.
func TestMemorySelectorForwardsSourceModelForExactRoute(t *testing.T) {
	selector := NewMemorySelector([]domain.Route{route(1, "deepseek-v4", domain.Channel{
		ID: "channel", Enabled: true, Weight: 1, SourceModel: "deepseek-ai/deepseek-v4-free",
	})})

	if got := selectChannel(t, selector, "deepseek-v4", domain.RoutingPolicy{}).Model; got != "deepseek-ai/deepseek-v4-free" {
		t.Fatalf("selected model = %q, want the channel source model", got)
	}
}

// Without a source model, an exact route forwards its own pattern.
func TestMemorySelectorForwardsRoutePatternForExactRouteWithoutSourceModel(t *testing.T) {
	selector := NewMemorySelector([]domain.Route{route(1, "Exact-Model", domain.Channel{
		ID: "channel", Enabled: true, Weight: 1,
	})})

	selection := selectChannel(t, selector, "exact-model", domain.RoutingPolicy{})
	if selection.Model != "Exact-Model" {
		t.Fatalf("selected model = %q, want the route pattern as stored", selection.Model)
	}
}

// A glob route forwards the requested name untouched when nothing maps it.
func TestMemorySelectorForwardsRequestedModelForGlobRoute(t *testing.T) {
	selector := NewMemorySelector([]domain.Route{route(1, "gpt-*", domain.Channel{
		ID: "channel", Enabled: true, Weight: 1,
	})})

	if got := selectChannel(t, selector, "gpt-4o-mini", domain.RoutingPolicy{}).Model; got != "gpt-4o-mini" {
		t.Fatalf("selected model = %q, want the requested model", got)
	}
}

// One exposed model may reach several upstreams that each spell it differently.
// The route carries the name clients ask for and the channel carries the name
// its own upstream knows, so both channels have to be candidates and each has to
// receive its own spelling.
func TestMemorySelectorSendsEachUpstreamItsOwnModelName(t *testing.T) {
	selector := NewMemorySelector([]domain.Route{route(1, "claude-sonnet",
		domain.Channel{ID: "vendor-a", Enabled: true, Weight: 10, SiteID: 1, SourceModel: "claude-3-5-sonnet"},
		domain.Channel{ID: "vendor-b", Enabled: true, Weight: 10, SiteID: 2, SourceModel: "claude-sonnet"},
	)})

	seen := map[string]string{}
	for index := 0; index < 8; index++ {
		selection := selectChannel(t, selector, "claude-sonnet", domain.RoutingPolicy{})
		seen[selection.Channel.ID] = selection.Model
	}
	if len(seen) != 2 {
		t.Fatalf("only %v were selected, want both upstreams to stay reachable", seen)
	}
	if seen["vendor-a"] != "claude-3-5-sonnet" {
		t.Errorf("vendor-a received %q, want its own name claude-3-5-sonnet", seen["vendor-a"])
	}
	if seen["vendor-b"] != "claude-sonnet" {
		t.Errorf("vendor-b received %q, want claude-sonnet", seen["vendor-b"])
	}
}

// A channel whose own model name is not equivalent to the request is still a
// candidate on the route that exposes it, which is what makes the mapping work.
func TestMemorySelectorAdmitsAnExplicitSourceModelOnAnExactRoute(t *testing.T) {
	selector := NewMemorySelector([]domain.Route{route(1, "gpt-4.1",
		domain.Channel{ID: "renamed", Enabled: true, Weight: 10, SourceModel: "gpt-4.1-2025-04-14"},
	)})
	selection := selectChannel(t, selector, "gpt-4.1", domain.RoutingPolicy{})
	if selection.Channel.ID != "renamed" {
		t.Fatalf("selected channel = %q, want renamed", selection.Channel.ID)
	}
	if selection.Model != "gpt-4.1-2025-04-14" {
		t.Fatalf("selected model = %q, want the channel's own name", selection.Model)
	}

	// The same channel is not a candidate for a different model.
	if selector.HasCandidate("gpt-4.1-mini", domain.RoutingPolicy{}) {
		t.Error("a channel with an explicit source model answered for an unrelated model")
	}
}

func TestMemorySelectorHonoursThePin(t *testing.T) {
	selector := NewMemorySelector([]domain.Route{route(1, "gpt-4.1",
		domain.Channel{ID: "a", Enabled: true, Weight: 10, SiteID: 1},
		domain.Channel{ID: "b", Enabled: true, Weight: 10, SiteID: 2},
	)})

	selection, err := selector.Select(domain.SelectionRequest{Model: "gpt-4.1", OnlyChannel: "b"})
	if err != nil {
		t.Fatalf("Select(OnlyChannel) error = %v", err)
	}
	if selection.Channel.ID != "b" {
		t.Fatalf("selected channel = %q, want the pinned one", selection.Channel.ID)
	}

	selection, err = selector.Select(domain.SelectionRequest{Model: "gpt-4.1", OnlySiteID: 1})
	if err != nil {
		t.Fatalf("Select(OnlySiteID) error = %v", err)
	}
	if selection.Channel.ID != "a" {
		t.Fatalf("selected channel = %q, want the channel of the pinned upstream", selection.Channel.ID)
	}

	if _, err := selector.Select(domain.SelectionRequest{Model: "gpt-4.1", OnlyChannel: "gone"}); err == nil {
		t.Error("Select() answered a pin that matches no channel")
	}
	if _, err := selector.Select(domain.SelectionRequest{Model: "gpt-4.1", OnlySiteID: 99}); err == nil {
		t.Error("Select() answered a pin that matches no upstream")
	}
}
