package router

import (
	"errors"
	"math/rand"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yhw5231/fluxgate/internal/domain"
	"github.com/yhw5231/fluxgate/internal/pattern"
)

const defaultChannelWeight = 10

// The two per-key selection modes an upstream can be configured with. The console
// stores the upstream's key mode as the routing strategy of the routes it manages,
// so the loader hands it to the router on every channel of that upstream.
const (
	// keyModeFirstAvailable prefers the upstream's first key, moving to the next
	// one only when the first is unusable.
	keyModeFirstAvailable = "stable_first"
	// keyModeRotate spreads a request across the upstream's keys by key weight;
	// equal weights make it a plain rotation.
	keyModeRotate = "round_robin"
)

var (
	// ErrNoChannel reports that a route matched but no channel could serve the
	// request, for example because every candidate is blocked or excluded.
	ErrNoChannel = errors.New("no eligible upstream channel")
	// ErrModelNotRoutable reports that no enabled route serves the model.
	ErrModelNotRoutable = errors.New("no enabled route matches the requested model")
)

// ChannelFilter can remove temporarily or permanently blocked channels at selection time.
type ChannelFilter interface {
	// IsBlocked reports whether a channel is held out of rotation for one model.
	// The model is the one the channel would be asked for — its own name for the
	// requested one, as ActualModel resolves it — because that is the name a
	// per-model circuit is filed under when the channel fails.
	IsBlocked(channel domain.Channel, model string) bool
}

// MemorySelector resolves the route that owns the requested model and picks one
// of its channels.
//
// Selection asks two questions in order, the way an operator reasons about a pool
// of upstreams:
//
//  1. Which upstream? The highest upstream priority with an eligible channel
//     wins; upstreams that tie on it are drawn by upstream weight.
//  2. Which key of that upstream? The upstream's own key mode answers that — the
//     first available key, or a rotation across its keys — and nothing outside the
//     upstream influences it. A key can never lift its upstream above a preferred
//     one, which is what makes priority mean "use this upstream first" rather than
//     "give it more traffic".
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

	selected := s.pickKey(s.pickUpstream(eligible, request.Policy), request.Policy)
	return domain.Selection{
		Channel: selected,
		Model:   ActualModel(request.Model, route, selected),
	}, nil
}

// pickUpstream keeps the channels of the upstream a request should be tried on:
// the highest upstream priority present, and among the upstreams that tie on it
// the one the draw gives, each upstream's chance proportional to its own weight
// (its global weight scaled by the downstream key's site multiplier). A channel
// of a lower-priority upstream is reached only when every channel of the higher
// one is unusable, which is what makes priority mean "try these first" rather
// than "give these more traffic".
//
// The return value is the chosen upstream's eligible channels; picking the key
// out of them is a separate step, because which keys are still available is the
// upstream's business and not a property of the pool.
func (s *MemorySelector) pickUpstream(eligible []domain.Channel, policy domain.RoutingPolicy) []domain.Channel {
	bestPriority := eligible[0].SitePriority
	for _, channel := range eligible {
		if channel.SitePriority > bestPriority {
			bestPriority = channel.SitePriority
		}
	}

	order := make([]int64, 0, len(eligible))
	bySite := make(map[int64][]domain.Channel, len(eligible))
	for _, channel := range eligible {
		if channel.SitePriority != bestPriority {
			continue
		}
		if _, seen := bySite[channel.SiteID]; !seen {
			order = append(order, channel.SiteID)
		}
		bySite[channel.SiteID] = append(bySite[channel.SiteID], channel)
	}
	if len(order) == 1 {
		return bySite[order[0]]
	}

	weights := make([]float64, len(order))
	total := 0.0
	for index, siteID := range order {
		weights[index] = siteWeight(bySite[siteID][0], policy)
		total += weights[index]
	}
	if !(total > 0) {
		return bySite[order[0]]
	}

	draw := s.random.Float64() * total
	for index, weight := range weights {
		if draw < weight {
			return bySite[order[index]]
		}
		draw -= weight
	}
	// Floating-point rounding can leave the draw a hair above the last weight.
	return bySite[order[len(order)-1]]
}

// pickKey picks the key inside the chosen upstream, which the upstream's key mode
// answers on its own: an upstream that prefers its first available key takes the
// lowest one, and one that rotates spreads the request across its keys by key
// weight, which with equal weights is a plain rotation. Blocked keys are already
// gone by this point, so "first available" is the first key still in service.
func (s *MemorySelector) pickKey(keys []domain.Channel, policy domain.RoutingPolicy) domain.Channel {
	if len(keys) == 0 {
		// Callers only reach here with an eligible channel, so this is
		// unreachable rather than a case with a meaning of its own.
		return domain.Channel{}
	}
	if len(keys) == 1 || firstKeyMode(keys) == keyModeFirstAvailable {
		return firstKey(keys)
	}
	return s.pickWeighted(keys, policy)
}

// firstKeyMode reads the key mode of an upstream off its channels. Every channel
// of one upstream carries the strategy of the route that created it, so the first
// one answers for all of them.
func firstKeyMode(keys []domain.Channel) string {
	mode := strings.TrimSpace(keys[0].RoutingStrategy)
	if mode == "" {
		return keyModeRotate
	}
	return mode
}

// firstKey is the key an upstream that prefers its first available key is tried
// on: the lowest line of that upstream, which is the order its keys are stored in
// and the order the console shows them in.
func firstKey(keys []domain.Channel) domain.Channel {
	first := keys[0]
	for _, channel := range keys[1:] {
		if lineOrderBefore(channel.ID, first.ID) {
			first = channel
		}
	}
	return first
}

// lineOrderBefore orders two lines the way an upstream lists its keys. Line ids
// are numbers in practice, so they are compared as numbers when they parse and as
// text when they do not.
func lineOrderBefore(left, right string) bool {
	leftID, leftErr := strconv.ParseInt(left, 10, 64)
	rightID, rightErr := strconv.ParseInt(right, 10, 64)
	if leftErr == nil && rightErr == nil {
		return leftID < rightID
	}
	return left < right
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
		// A circuit is filed under the model the line was actually asked for, so
		// the question has to be asked about that same name: asking about the
		// requested one would look up a circuit that is never written when the
		// route maps the model, and the line would stay in rotation while its
		// recorded failures kept escalating.
		if s.filter != nil && s.filter.IsBlocked(channel, ActualModel(request.Model, route, channel)) {
			continue
		}
		eligible = append(eligible, channel)
	}
	return eligible
}

// pickWeighted draws one key out of the chosen upstream's keys, each key's chance
// proportional to its effective weight. Inside one upstream the site factors are
// the same for every key, so the draw is proportional to the key weights alone. A
// pool that is a single key, or whose weights are all unusable, is answered
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
	return weight * siteWeight(channel, policy)
}

// siteWeight is what one upstream contributes to a draw: the upstream's global
// weight scaled by the downstream key's multiplier for it. A draw between
// upstreams uses this on its own, because there the upstream is the unit being
// chosen and the key's own weight is not part of the question.
func siteWeight(channel domain.Channel, policy domain.RoutingPolicy) float64 {
	siteWeight := channel.SiteGlobalWeight
	if !(siteWeight > 0) {
		siteWeight = 1
	}
	return siteWeight * policy.SiteMultiplier(channel.SiteID)
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
