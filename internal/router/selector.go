package router

import (
	"errors"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/yhw5231/fluxgate/internal/domain"
	"github.com/yhw5231/fluxgate/internal/pattern"
)

const defaultChannelWeight = 10

var (
	// ErrNoChannel reports that a route matched but no channel could serve the
	// request, for example because every candidate is blocked or excluded.
	ErrNoChannel = errors.New("no eligible upstream channel")
	// ErrModelNotRoutable reports that no enabled route serves the model.
	ErrModelNotRoutable = errors.New("no enabled route matches the requested model")
)

// ChannelFilter can remove temporarily or permanently blocked channels at selection time.
type ChannelFilter interface {
	IsBlocked(channel domain.Channel, model string) bool
}

// MemorySelector resolves the route that owns the requested model and picks one
// of its channels.
//
// Selection is by priority first and by weight second, the way an operator
// reasons about a pool of upstreams:
//
//  1. The highest upstream priority with an eligible channel wins. The channel's
//     own priority is compared inside that: a key mode that orders one
//     upstream's keys orders them within their upstream, never across upstreams.
//  2. Among the channels that tie on both, one is drawn at random, each
//     channel's chance proportional to its effective weight.
type MemorySelector struct {
	mu     sync.Mutex
	routes []domain.Route
	filter ChannelFilter
	random *rand.Rand
}

func NewMemorySelector(routes []domain.Route) *MemorySelector {
	return NewMemorySelectorWithFilter(routes, nil)
}

// NewMemorySelectorWithFilter preserves the existing selector behavior while
// allowing runtime circuit-breaker filtering.
func NewMemorySelectorWithFilter(routes []domain.Route, filter ChannelFilter) *MemorySelector {
	copied := append([]domain.Route(nil), routes...)
	sort.SliceStable(copied, func(i, j int) bool { return copied[i].ID < copied[j].ID })
	return &MemorySelector{
		routes: copied,
		filter: filter,
		random: rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

// SetRoutes replaces the routing table in place. The selector object itself is
// never swapped, so a configuration change made in the management console takes
// effect for the next request while requests already in flight finish against
// the table they started with.
func (s *MemorySelector) SetRoutes(routes []domain.Route) {
	copied := append([]domain.Route(nil), routes...)
	sort.SliceStable(copied, func(i, j int) bool { return copied[i].ID < copied[j].ID })
	s.mu.Lock()
	defer s.mu.Unlock()
	s.routes = copied
}

// Select resolves a channel for the requested model, applying the downstream
// key policy and skipping channels already excluded by the retry loop.
func (s *MemorySelector) Select(request domain.SelectionRequest) (domain.Selection, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	route, ok := s.findRoute(request.Model, request.Policy)
	if !ok {
		return domain.Selection{}, ErrModelNotRoutable
	}

	eligible := s.eligible(route, request, displayNameMatches(request.Model, route.DisplayName))
	if len(eligible) == 0 {
		return domain.Selection{}, ErrNoChannel
	}

	selected := s.pickWeighted(highestPriorityTier(eligible), request.Policy)
	return domain.Selection{
		Channel: selected,
		Model:   ActualModel(request.Model, route, selected),
	}, nil
}

// highestPriorityTier keeps the channels a request should be tried on first: the
// highest upstream priority present, and within it the highest channel priority.
// A channel of a lower tier is only reached when every channel of the higher one
// is unusable, which is what makes priority mean "try these first" rather than
// "give these more traffic".
func highestPriorityTier(eligible []domain.Channel) []domain.Channel {
	bestSite, bestLine := eligible[0].SitePriority, eligible[0].Priority
	for _, channel := range eligible {
		if channel.SitePriority > bestSite {
			bestSite, bestLine = channel.SitePriority, channel.Priority
			continue
		}
		if channel.SitePriority == bestSite && channel.Priority > bestLine {
			bestLine = channel.Priority
		}
	}
	pool := make([]domain.Channel, 0, len(eligible))
	for _, channel := range eligible {
		if channel.SitePriority == bestSite && channel.Priority == bestLine {
			pool = append(pool, channel)
		}
	}
	return pool
}

// HasCandidate reports whether any channel could serve the model, without
// drawing a channel.
func (s *MemorySelector) HasCandidate(model string, policy domain.RoutingPolicy) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	route, ok := s.findRoute(model, policy)
	if !ok {
		return false
	}
	request := domain.SelectionRequest{Model: model, Policy: policy}
	return len(s.eligible(route, request, displayNameMatches(model, route.DisplayName))) > 0
}

// findRoute applies the upstream route precedence: an explicit group matched by
// display name, then an exact model pattern, then a non-group display name,
// then a glob or regex pattern.
func (s *MemorySelector) findRoute(model string, policy domain.RoutingPolicy) (domain.Route, bool) {
	if policy.DeniesModel(model) {
		return domain.Route{}, false
	}

	candidates := make([]domain.Route, 0, len(s.routes))
	for _, route := range s.routes {
		if !route.Enabled {
			continue
		}
		if !routeAllowedByPolicy(route, policy) {
			continue
		}
		candidates = append(candidates, route)
	}

	for _, route := range candidates {
		if route.IsGroup() && displayNameMatches(model, route.DisplayName) {
			return route, true
		}
	}
	for _, route := range candidates {
		if !route.IsGroup() && pattern.IsExact(route.ModelPattern) && pattern.MatchName(model, route.ModelPattern) {
			return route, true
		}
	}
	for _, route := range candidates {
		if !route.IsGroup() && displayNameMatches(model, route.DisplayName) {
			return route, true
		}
	}
	for _, route := range candidates {
		if !route.IsGroup() && pattern.MatchName(model, route.ModelPattern) {
			return route, true
		}
	}
	return domain.Route{}, false
}

// routeAllowedByPolicy mirrors the upstream scope filter: an allow list keeps
// only the selected routes, while an exact model pattern always stays visible.
func routeAllowedByPolicy(route domain.Route, policy domain.RoutingPolicy) bool {
	if len(policy.AllowedRouteIDs) == 0 {
		return true
	}
	for _, id := range policy.AllowedRouteIDs {
		if id == route.ID {
			return true
		}
	}
	return !route.IsGroup() && pattern.IsExact(route.ModelPattern)
}

// channelsFor returns the candidate channels of a matched route. A group route
// draws from its enabled, non-group, exact-pattern source routes.
func (s *MemorySelector) channelsFor(route domain.Route) []domain.Channel {
	if !route.IsGroup() {
		return route.Channels
	}
	sources := make(map[int64]struct{}, len(route.SourceRouteIDs))
	for _, id := range route.SourceRouteIDs {
		sources[id] = struct{}{}
	}
	channels := make([]domain.Channel, 0)
	for _, candidate := range s.routes {
		if !candidate.Enabled || candidate.IsGroup() || !pattern.IsExact(candidate.ModelPattern) {
			continue
		}
		if _, ok := sources[candidate.ID]; !ok {
			continue
		}
		channels = append(channels, candidate.Channels...)
	}
	return channels
}

func (s *MemorySelector) eligible(route domain.Route, request domain.SelectionRequest, bypassSourceModel bool) []domain.Channel {
	channels := s.channelsFor(route)
	eligible := make([]domain.Channel, 0, len(channels))
	for _, channel := range channels {
		if !channel.Enabled {
			continue
		}
		if request.OnlyChannel != "" && channel.ID != request.OnlyChannel {
			continue
		}
		if request.OnlySiteID != 0 && channel.SiteID != request.OnlySiteID {
			continue
		}
		if _, skip := request.Excluded[channel.ID]; skip {
			continue
		}
		if !bypassSourceModel && !channelAdmitsModel(route, channel, request.Model) {
			continue
		}
		if request.Policy.ExcludesSite(channel.SiteID) {
			continue
		}
		if request.Policy.ExcludesCredential(channel) {
			continue
		}
		if s.filter != nil && s.filter.IsBlocked(channel, request.Model) {
			continue
		}
		eligible = append(eligible, channel)
	}
	return eligible
}

// pickWeighted draws one channel from a priority tier, each channel's chance
// proportional to its effective weight: contribution is the channel weight
// scaled by the site weight and the downstream key's site multiplier. A tier
// that is a single channel, or whose weights are all unusable, is answered
// without a draw.
func (s *MemorySelector) pickWeighted(pool []domain.Channel, policy domain.RoutingPolicy) domain.Channel {
	if len(pool) == 0 {
		// Callers only reach here with an eligible channel, so this is
		// unreachable rather than a case with a meaning of its own.
		return domain.Channel{}
	}
	if len(pool) == 1 {
		return pool[0]
	}
	total := 0.0
	weights := make([]float64, len(pool))
	for index, channel := range pool {
		weights[index] = effectiveWeight(channel, policy)
		total += weights[index]
	}
	if !(total > 0) {
		return pool[0]
	}

	draw := s.random.Float64() * total
	for index, weight := range weights {
		if draw < weight {
			return pool[index]
		}
		draw -= weight
	}
	// Floating-point rounding can leave the draw a hair above the last weight.
	return pool[len(pool)-1]
}

func effectiveWeight(channel domain.Channel, policy domain.RoutingPolicy) float64 {
	weight := float64(channel.Weight)
	if weight <= 0 {
		weight = defaultChannelWeight
	}
	siteWeight := channel.SiteGlobalWeight
	if !(siteWeight > 0) {
		siteWeight = 1
	}
	return weight * siteWeight * policy.SiteMultiplier(channel.SiteID)
}

// displayNameMatches reports whether the request named the route by its alias.
func displayNameMatches(model, displayName string) bool {
	trimmed := strings.TrimSpace(displayName)
	if trimmed == "" {
		return false
	}
	return strings.EqualFold(trimmed, strings.TrimSpace(model)) || pattern.Equivalent(trimmed, model)
}

// sourceModelSupports reports whether a channel's source_model admits the
// requested model. An empty value means unrestricted; otherwise the value must
// be the same model, an alias-equivalent name, or a matching pattern.
func sourceModelSupports(sourceModel, requested string) bool {
	source := strings.TrimSpace(sourceModel)
	if source == "" || source == requested {
		return true
	}
	if pattern.Equivalent(source, requested) {
		return true
	}
	return pattern.MatchName(requested, source)
}

// channelAdmitsModel reports whether a channel may serve the requested model.
// Besides the source-model rule, a channel on an exact-model route is a
// candidate whenever it names its own upstream model. That is what lets one
// exposed model reach several upstreams that each spell it differently: the
// route carries the name clients ask for, and the channel carries the name its
// upstream knows.
func channelAdmitsModel(route domain.Route, channel domain.Channel, requested string) bool {
	if sourceModelSupports(channel.SourceModel, requested) {
		return true
	}
	return hasExplicitSourceModel(route, channel)
}

// hasExplicitSourceModel reports whether a channel names its own upstream model
// instead of inheriting the route's exact pattern.
func hasExplicitSourceModel(route domain.Route, channel domain.Channel) bool {
	source := strings.TrimSpace(channel.SourceModel)
	if source == "" {
		return false
	}
	routePattern := strings.TrimSpace(route.ModelPattern)
	if routePattern == "" || !pattern.IsExact(routePattern) {
		return false
	}
	return !pattern.Equivalent(source, routePattern)
}

// ActualModel picks the model written into the upstream request body, and with
// it the name a per-model circuit is filed under. A display-name request uses
// the channel source model, a channel that names its own model on an exact route
// uses that name, and everything else uses the mapped model.
//
// It is exported so a management view can ask the same question about the model
// a request would carry, instead of guessing it from the route's pattern.
func ActualModel(requested string, route domain.Route, channel domain.Channel) string {
	sourceModel := strings.TrimSpace(channel.SourceModel)
	if displayNameMatches(requested, route.DisplayName) && sourceModel != "" {
		return sourceModel
	}
	if hasExplicitSourceModel(route, channel) && pattern.MatchName(requested, route.ModelPattern) {
		return sourceModel
	}
	mapped := route.ModelMapping.Resolve(requested)
	routePattern := strings.TrimSpace(route.ModelPattern)
	if mapped == requested && pattern.IsExact(routePattern) && pattern.MatchName(requested, routePattern) {
		if sourceModel != "" {
			return sourceModel
		}
		return routePattern
	}
	return mapped
}
