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

// The upstream priority decides which upstream answers, and the model mapping of
// the chosen line is what the upstream is asked for.
func TestMemorySelectorPrefersTheHigherUpstreamAndMapsModel(t *testing.T) {
	selector := NewMemorySelector([]domain.Route{{
		ID:           1,
		ModelPattern: "gpt-*",
		Mode:         domain.RouteModePattern,
		Enabled:      true,
		ModelMapping: domain.ModelMapping{{Pattern: "gpt-*", Target: "mapped-model"}},
		Channels: []domain.Channel{
			{ID: "lower", Enabled: true, Weight: 1, SiteID: 1},
			{ID: "higher", Enabled: true, Weight: 1, SiteID: 2, SitePriority: 5},
		},
	}})

	selection := selectChannel(t, selector, "gpt-4.1", domain.RoutingPolicy{})
	if selection.Channel.ID != "higher" {
		t.Fatalf("selected channel = %q, want higher", selection.Channel.ID)
	}
	if selection.Model != "mapped-model" {
		t.Fatalf("selected model = %q, want mapped-model", selection.Model)
	}
}

// drawCounts selects the model a number of times and counts what came back, so
// a weighted choice is checked as a share rather than as a sequence.
func drawCounts(t *testing.T, selector *MemorySelector, model string, policy domain.RoutingPolicy, draws int) map[string]int {
	t.Helper()
	counts := map[string]int{}
	for index := 0; index < draws; index++ {
		counts[selectChannel(t, selector, model, policy).Channel.ID]++
	}
	return counts
}

// withinShare reports whether a count is within a tolerance of the share of the
// draws it should have taken.
func withinShare(count, draws int, share, tolerance float64) bool {
	ratio := float64(count) / float64(draws)
	return ratio >= share-tolerance && ratio <= share+tolerance
}

// An upstream that rotates spreads its requests across its own keys, each key in
// proportion to its weight: a 3:1 pair takes about three quarters of the requests.
func TestMemorySelectorDrawsKeysOfOneUpstreamByKeyWeight(t *testing.T) {
	selector := NewMemorySelector([]domain.Route{route(1, "*",
		domain.Channel{ID: "heavy", Enabled: true, Weight: 3, SiteID: 1, RoutingStrategy: keyModeRotate},
		domain.Channel{ID: "light", Enabled: true, Weight: 1, SiteID: 1, RoutingStrategy: keyModeRotate},
	)})

	const draws = 4000
	counts := drawCounts(t, selector, "model", domain.RoutingPolicy{}, draws)
	if !withinShare(counts["heavy"], draws, 0.75, 0.05) {
		t.Fatalf("heavy was drawn %d of %d times, want about three quarters", counts["heavy"], draws)
	}
	if counts["heavy"]+counts["light"] != draws {
		t.Fatalf("counts = %#v, want every draw answered", counts)
	}
}

// Two upstreams of one priority are drawn by upstream weight, and both keep
// answering: a tie is a split, not a fallback.
func TestMemorySelectorKeepsEveryUpstreamOfAPriorityReachable(t *testing.T) {
	selector := NewMemorySelector([]domain.Route{route(1, "*",
		domain.Channel{ID: "a", Enabled: true, Weight: 10, SiteID: 1, SiteGlobalWeight: 1},
		domain.Channel{ID: "b", Enabled: true, Weight: 10, SiteID: 2, SiteGlobalWeight: 1},
	)})

	counts := drawCounts(t, selector, "model", domain.RoutingPolicy{}, 200)
	if counts["a"] == 0 || counts["b"] == 0 {
		t.Fatalf("counts = %#v, want both upstreams to stay reachable", counts)
	}
}

func TestMemorySelectorHonorsExclusionsAndDisabledChannels(t *testing.T) {
	selector := NewMemorySelector([]domain.Route{route(1, "*",
		domain.Channel{ID: "disabled", Enabled: false, Weight: 1, SiteID: 3, SitePriority: 30},
		domain.Channel{ID: "primary", Enabled: true, Weight: 1, SiteID: 2, SitePriority: 20},
		domain.Channel{ID: "fallback", Enabled: true, Weight: 1, SiteID: 1, SitePriority: 10},
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
	// Site 7 carries the higher priority, so which line answers is decided by
	// the policy under test rather than by the draw between equals.
	newSelector := func() *MemorySelector {
		return NewMemorySelector([]domain.Route{route(1, "*",
			domain.Channel{ID: "site-7", Enabled: true, Weight: 1, SiteID: 7, AccountID: 1, TokenID: &tokenID, SitePriority: 10},
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

	t.Run("allowed site ids keep only the listed upstreams", func(t *testing.T) {
		policy := domain.RoutingPolicy{AllowedSiteIDs: []int64{8}}
		if got := selectChannel(t, newSelector(), "model", policy).Channel.ID; got != "site-8" {
			t.Fatalf("selected channel = %q, want site-8", got)
		}
		if !newSelector().HasCandidate("model", policy) {
			t.Fatal("HasCandidate() = false for a model one allowed upstream serves")
		}

		// A key restricted to an upstream that serves nothing has no candidate, so
		// the request fails instead of falling through to an upstream it may not use.
		absent := domain.RoutingPolicy{AllowedSiteIDs: []int64{9}}
		if newSelector().HasCandidate("model", absent) {
			t.Fatal("HasCandidate() = true for a site the allow list does not name")
		}
		if _, err := newSelector().Select(domain.SelectionRequest{Model: "model", Policy: absent}); err != ErrNoChannel {
			t.Fatalf("Select() error = %v, want ErrNoChannel", err)
		}
	})

	t.Run("an exclusion applies on top of the allow list", func(t *testing.T) {
		policy := domain.RoutingPolicy{AllowedSiteIDs: []int64{7, 8}, ExcludedSiteIDs: []int64{7}}
		if got := selectChannel(t, newSelector(), "model", policy).Channel.ID; got != "site-8" {
			t.Fatalf("selected channel = %q, want site-8", got)
		}
		excludedBoth := domain.RoutingPolicy{AllowedSiteIDs: []int64{7}, ExcludedSiteIDs: []int64{7}}
		if newSelector().HasCandidate("model", excludedBoth) {
			t.Fatal("a site that is both allowed and excluded stayed selectable")
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
	newSelector := func() *MemorySelector {
		return NewMemorySelector([]domain.Route{route(1, "*",
			domain.Channel{ID: "base", Enabled: true, Weight: 10, SiteID: 1, SiteGlobalWeight: 1},
			domain.Channel{ID: "double", Enabled: true, Weight: 10, SiteID: 2, SiteGlobalWeight: 2},
		)})
	}

	// Site 2 carries twice the weight, so it answers about two draws in three.
	const draws = 4000
	counts := drawCounts(t, newSelector(), "model", domain.RoutingPolicy{}, draws)
	if !withinShare(counts["double"], draws, 2.0/3.0, 0.05) {
		t.Fatalf("double answered %d of %d draws, want about two thirds", counts["double"], draws)
	}

	// A downstream multiplier scales one site's share of the same draws: site 2
	// falls to 2 × 0.1 against site 1's 1.
	policy := domain.RoutingPolicy{SiteMultipliers: map[int64]float64{2: 0.1}}
	counts = drawCounts(t, newSelector(), "model", policy, draws)
	if !withinShare(counts["base"], draws, 10.0/12.0, 0.05) {
		t.Fatalf("base answered %d of %d draws, want about five sixths once the multiplier is lowered", counts["base"], draws)
	}
}

// Priority is what an operator uses to say "use this upstream first", so every
// line of a preferred upstream is tried before any line of a lower-priority one,
// whatever their weights are.
func TestMemorySelectorPrefersTheHigherUpstreamPriority(t *testing.T) {
	selector := NewMemorySelector([]domain.Route{route(1, "*",
		domain.Channel{ID: "preferred", Enabled: true, Weight: 1, SiteID: 1, SitePriority: 5},
		domain.Channel{ID: "fallback", Enabled: true, Weight: 1000, SiteID: 2},
	)})

	counts := drawCounts(t, selector, "model", domain.RoutingPolicy{}, 200)
	if counts["preferred"] != 200 {
		t.Fatalf("counts = %#v, want every request on the preferred upstream", counts)
	}

	// The preferred upstream leaves the pool when it is excluded, which is what
	// makes the lower priority the fallback rather than a second half of a split.
	selection, err := selector.Select(domain.SelectionRequest{
		Model: "model",
		Excluded: map[string]struct{}{
			"preferred": {},
		},
	})
	if err != nil {
		t.Fatalf("Select() error = %v", err)
	}
	if selection.Channel.ID != "fallback" {
		t.Fatalf("selected channel = %q, want the fallback once the preferred one is out", selection.Channel.ID)
	}
}

// A key's own priority must never lift its upstream past a preferred one: the
// upstream is chosen first, and only then is a key of that upstream picked.
func TestMemorySelectorNeverLiftsAKeyPastAPreferredUpstream(t *testing.T) {
	selector := NewMemorySelector([]domain.Route{route(1, "*",
		domain.Channel{ID: "preferred-key", Enabled: true, Weight: 1, SiteID: 1, SitePriority: 1},
		domain.Channel{ID: "other-first-key", Enabled: true, Weight: 1, SiteID: 2, Priority: 9},
		domain.Channel{ID: "other-second-key", Enabled: true, Weight: 1, SiteID: 2, Priority: 8},
	)})

	counts := drawCounts(t, selector, "model", domain.RoutingPolicy{}, 100)
	if counts["preferred-key"] != 100 {
		t.Fatalf("counts = %#v, want the preferred upstream's key throughout", counts)
	}
}

// An upstream that prefers its first key takes the lowest key of its own list,
// whatever the keys weigh, and moves to the next one only when that key is out.
func TestMemorySelectorPrefersTheFirstKeyOfAnUpstream(t *testing.T) {
	newSelector := func() *MemorySelector {
		return NewMemorySelector([]domain.Route{route(1, "*",
			domain.Channel{ID: "1", Enabled: true, Weight: 1, SiteID: 1, RoutingStrategy: keyModeFirstAvailable},
			domain.Channel{ID: "2", Enabled: true, Weight: 1000, SiteID: 1, RoutingStrategy: keyModeFirstAvailable},
			domain.Channel{ID: "3", Enabled: true, Weight: 1000, SiteID: 1, RoutingStrategy: keyModeFirstAvailable},
		)})
	}

	counts := drawCounts(t, newSelector(), "model", domain.RoutingPolicy{}, 100)
	if counts["1"] != 100 {
		t.Fatalf("counts = %#v, want the upstream's first key throughout", counts)
	}

	// The second key takes over once the first one is out of the pool, and the
	// list order decides that rather than the weights.
	selection, err := newSelector().Select(domain.SelectionRequest{
		Model:    "model",
		Excluded: map[string]struct{}{"1": {}},
	})
	if err != nil {
		t.Fatalf("Select() error = %v", err)
	}
	if selection.Channel.ID != "2" {
		t.Fatalf("selected channel = %q, want the next key of the same upstream", selection.Channel.ID)
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
	for index := 0; index < 40; index++ {
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

// A route exposes one model identity, so a client reaches it under any spelling
// the upstreams use: the case it likes, a channel path, or a variant suffix.
func TestMemorySelectorReachesARouteUnderEverySpellingOfTheModel(t *testing.T) {
	selector := NewMemorySelector([]domain.Route{route(1, "deepseek-v4.1-flash", domain.Channel{
		ID: "channel", Enabled: true, Weight: 1, SourceModel: "cline-free/deepseek-v4.1-flash:free",
	})})

	for _, requested := range []string{
		"deepseek-v4.1-flash",
		"DeepSeek-V4.1-Flash",
		"cline-free/deepseek-v4.1-flash",
		"cline-free/deepseek-v4.1-flash:free",
	} {
		selection := selectChannel(t, selector, requested, domain.RoutingPolicy{})
		if selection.Channel.ID != "channel" {
			t.Errorf("Select(%q) chose %q, want the route that exposes the model", requested, selection.Channel.ID)
			continue
		}
		// Whatever the client called it, the upstream receives the spelling it
		// listed.
		if selection.Model != "cline-free/deepseek-v4.1-flash:free" {
			t.Errorf("Select(%q) forwarded %q, want the upstream's own spelling", requested, selection.Model)
		}
	}

	if selector.HasCandidate("deepseek-v4.1-pro", domain.RoutingPolicy{}) {
		t.Error("a decorated spelling of another model reached the route")
	}
}

// A glob route written for the plain model name covers the decorated spellings
// of it too, which is how one route can serve a whole family of listings.
func TestMemorySelectorMatchesAGlobRouteAgainstTheCanonicalName(t *testing.T) {
	selector := NewMemorySelector([]domain.Route{route(1, "deepseek-*", channel("channel", 0, 1))})

	if got := selectChannel(t, selector, "cline-free/deepseek-v4.1-flash:free", domain.RoutingPolicy{}).Channel.ID; got != "channel" {
		t.Fatalf("selected channel = %q, want the glob route", got)
	}
}

// An explicit group is named by its display name, and the same spelling rules
// apply to it.
func TestMemorySelectorMatchesAGroupDisplayNameByModelIdentity(t *testing.T) {
	group := domain.Route{
		ID:             10,
		DisplayName:    "claude-sonnet",
		Mode:           domain.RouteModeExplicitGroup,
		Enabled:        true,
		SourceRouteIDs: []int64{11},
	}
	source := route(11, "claude-sonnet", domain.Channel{
		ID: "group-source", Enabled: true, Weight: 1, SourceModel: "anthropic/claude-sonnet:beta",
	})
	selector := NewMemorySelector([]domain.Route{group, source})

	selection := selectChannel(t, selector, "anthropic/claude-sonnet:beta", domain.RoutingPolicy{})
	if selection.Channel.ID != "group-source" {
		t.Fatalf("selected channel = %q, want the group's source", selection.Channel.ID)
	}
}

// A deny pattern written for the plain model name also covers the decorated
// spellings, so a restricted model cannot be reached by asking for a channel's
// own name for it.
func TestMemorySelectorDeniesEverySpellingOfADeniedModel(t *testing.T) {
	selector := NewMemorySelector([]domain.Route{route(1, "*", channel("channel", 0, 1))})
	policy := domain.RoutingPolicy{DeniedModelPatterns: []string{"deepseek-v4.1-flash"}}

	for _, requested := range []string{
		"deepseek-v4.1-flash",
		"DeepSeek-V4.1-Flash",
		"cline-free/deepseek-v4.1-flash:free",
	} {
		if _, err := selector.Select(domain.SelectionRequest{Model: requested, Policy: policy}); err != ErrModelNotRoutable {
			t.Errorf("Select(%q) error = %v, want ErrModelNotRoutable", requested, err)
		}
	}
}
