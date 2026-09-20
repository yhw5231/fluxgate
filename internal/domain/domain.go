package domain

import (
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/yhw5231/fluxgate/internal/pattern"
)

// Route modes. A group route exposes a display name and draws its channels from
// the exact-pattern routes listed in route_group_sources.
const (
	RouteModePattern       = "pattern"
	RouteModeExplicitGroup = "explicit_group"
)

// Channel identifies one upstream route candidate.
type Channel struct {
	ID                 string
	Name               string
	BaseURL            string
	APIKey             string
	Enabled            bool
	Priority           int
	Weight             int
	RoutingStrategy    string
	BreakerMode        string
	ProxyURL           string
	UseSystemProxy     bool
	SiteProxyURL       string
	SiteUseSystemProxy bool
	Transform          TransformRules
	RequestTimeout     time.Duration

	// Routing identity. SiteID, AccountID and TokenID identify the upstream
	// credential so downstream key exclusions can be applied.
	SiteID           int64
	AccountID        int64
	TokenID          *int64
	SiteGlobalWeight float64
	SourceModel      string
}

// ModelMappingEntry is one pattern-to-upstream-model pair.
type ModelMappingEntry struct {
	Pattern string
	Target  string
}

// ModelMapping preserves the declaration order stored in model_mapping so
// overlapping patterns always resolve to the same target.
type ModelMapping []ModelMappingEntry

// Resolve applies a case-insensitive exact key first and then the first
// ordered matching pattern, returning the requested name when nothing maps.
func (m ModelMapping) Resolve(requested string) string {
	for _, entry := range m {
		if strings.TrimSpace(entry.Target) == "" {
			continue
		}
		if pattern.Normalize(entry.Pattern) == pattern.Normalize(requested) {
			return strings.TrimSpace(entry.Target)
		}
	}
	for _, entry := range m {
		if strings.TrimSpace(entry.Target) == "" {
			continue
		}
		if pattern.Match(requested, entry.Pattern) {
			return strings.TrimSpace(entry.Target)
		}
	}
	return requested
}

// Route is one token_routes row together with the channels it may serve from.
type Route struct {
	ID              int64
	ModelPattern    string
	DisplayName     string
	Mode            string
	ModelMapping    ModelMapping
	RoutingStrategy string
	Enabled         bool
	SourceRouteIDs  []int64
	Channels        []Channel
}

// IsGroup reports whether the route is an explicit group.
func (r Route) IsGroup() bool {
	return r.Mode == RouteModeExplicitGroup
}

// ExposedName is the model name clients use to reach this route.
func (r Route) ExposedName() string {
	if trimmed := strings.TrimSpace(r.DisplayName); trimmed != "" {
		return trimmed
	}
	return strings.TrimSpace(r.ModelPattern)
}

// ExcludedCredential identifies one upstream credential excluded for a
// downstream key without retaining the credential itself.
type ExcludedCredential struct {
	Kind      string
	SiteID    int64
	AccountID int64
	TokenID   int64
}

// RoutingPolicy carries the downstream key restrictions that shape selection.
type RoutingPolicy struct {
	// DeniedModelPatterns comes from the supported_models column, which is an
	// exclusion list: a matching model is rejected rather than permitted.
	DeniedModelPatterns []string
	AllowedRouteIDs     []int64
	ExcludedSiteIDs     []int64
	SiteMultipliers     map[int64]float64
	ExcludedCredentials []ExcludedCredential
}

// DeniesModel reports whether any deny pattern matches the requested model.
func (p RoutingPolicy) DeniesModel(model string) bool {
	for _, candidate := range p.DeniedModelPatterns {
		if pattern.Match(model, candidate) {
			return true
		}
	}
	return false
}

// ExcludesSite reports whether the site is excluded for this policy.
func (p RoutingPolicy) ExcludesSite(siteID int64) bool {
	for _, excluded := range p.ExcludedSiteIDs {
		if excluded == siteID {
			return true
		}
	}
	return false
}

// ExcludesCredential reports whether the channel credential is excluded. Every
// identifier must match, so a channel without a token never matches a
// token-scoped exclusion.
func (p RoutingPolicy) ExcludesCredential(channel Channel) bool {
	if channel.TokenID == nil {
		return false
	}
	for _, excluded := range p.ExcludedCredentials {
		if excluded.Kind != "account_token" {
			continue
		}
		if excluded.TokenID == *channel.TokenID &&
			excluded.AccountID == channel.AccountID &&
			excluded.SiteID == channel.SiteID {
			return true
		}
	}
	return false
}

// SiteMultiplier returns the configured site weight multiplier, defaulting to 1.
func (p RoutingPolicy) SiteMultiplier(siteID int64) float64 {
	multiplier, ok := p.SiteMultipliers[siteID]
	if !ok || !(multiplier > 0) {
		return 1
	}
	return multiplier
}

// AllowsModel reports whether the policy permits the requested model. A deny
// pattern always wins; otherwise the model must be visible through the allowed
// routes, while an exact route pattern is always visible.
func AllowsModel(routes []Route, policy RoutingPolicy, model string) bool {
	if policy.DeniesModel(model) {
		return false
	}
	for _, route := range routes {
		if route.Enabled && strings.TrimSpace(route.ModelPattern) == model {
			return true
		}
	}
	if len(policy.AllowedRouteIDs) == 0 {
		return true
	}
	allowed := make(map[int64]struct{}, len(policy.AllowedRouteIDs))
	for _, id := range policy.AllowedRouteIDs {
		allowed[id] = struct{}{}
	}
	for _, route := range routes {
		if !route.Enabled {
			continue
		}
		if _, ok := allowed[route.ID]; !ok {
			continue
		}
		if route.ExposedName() == model {
			return true
		}
	}
	return false
}

// ExposedModels lists the model names clients can request, sorted and unique.
// Exact routes that are fully covered by a group route or by a glob route with
// a custom display name are hidden so an alias does not leak its members.
func ExposedModels(routes []Route) []string {
	enabled := make([]Route, 0, len(routes))
	for _, route := range routes {
		if route.Enabled {
			enabled = append(enabled, route)
		}
	}

	exactNames := make(map[string]struct{})
	for _, route := range enabled {
		if route.IsGroup() || !pattern.IsExact(route.ModelPattern) {
			continue
		}
		if name := strings.TrimSpace(route.ModelPattern); name != "" {
			exactNames[name] = struct{}{}
		}
	}

	covering := make([]Route, 0, len(enabled))
	for _, route := range enabled {
		switch {
		case route.IsGroup():
			if strings.TrimSpace(route.DisplayName) != "" && len(route.SourceRouteIDs) > 0 {
				covering = append(covering, route)
			}
		case !pattern.IsExact(route.ModelPattern) && hasCustomDisplayName(route):
			covering = append(covering, route)
		}
	}

	names := make(map[string]struct{}, len(enabled))
	for _, route := range enabled {
		if !isRouteVisible(route, covering, exactNames) {
			continue
		}
		if name := route.ExposedName(); name != "" {
			names[name] = struct{}{}
		}
	}

	sorted := make([]string, 0, len(names))
	for name := range names {
		sorted = append(sorted, name)
	}
	sort.Strings(sorted)
	return sorted
}

func isRouteVisible(route Route, covering []Route, exactNames map[string]struct{}) bool {
	if route.IsGroup() {
		return strings.TrimSpace(route.DisplayName) != ""
	}
	if !pattern.IsExact(route.ModelPattern) || hasCustomDisplayName(route) {
		return true
	}
	exactModel := strings.TrimSpace(route.ModelPattern)
	if exactModel == "" {
		return true
	}
	for _, cover := range covering {
		if cover.ID == route.ID {
			continue
		}
		coverName := strings.TrimSpace(cover.DisplayName)
		if coverName == "" {
			continue
		}
		if _, shadowed := exactNames[coverName]; shadowed {
			continue
		}
		if cover.IsGroup() {
			if containsID(cover.SourceRouteIDs, route.ID) {
				return false
			}
			continue
		}
		if pattern.Match(exactModel, cover.ModelPattern) {
			return false
		}
	}
	return true
}

func hasCustomDisplayName(route Route) bool {
	displayName := strings.TrimSpace(route.DisplayName)
	if displayName == "" {
		return false
	}
	return !strings.EqualFold(displayName, strings.TrimSpace(route.ModelPattern))
}

func containsID(values []int64, target int64) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

// TransformRules describes protocol-neutral request mutations applied before dispatch.
type TransformRules struct {
	RemoveHeaders []string
	SetHeaders    http.Header
	DeleteJSON    []string
	OverrideJSON  map[string]any
}

// RetryPolicy bounds server-side retries and channel failover.
type RetryPolicy struct {
	MaxAttempts           int
	MaxAttemptsPerChannel int
	RetryStatuses         map[int]struct{}
	BaseBackoff           time.Duration
	MaxBackoff            time.Duration
}

// Request is the protocol-neutral input passed through selection and dispatch.
type Request struct {
	Method  string
	Path    string
	Headers http.Header
	Body    []byte
	Model   string
	Policy  RoutingPolicy
}

// Attempt describes a single upstream dispatch for observability and selectors.
type Attempt struct {
	Number       int
	ChannelID    string
	ChannelIndex int
	KeyID        string
	Model        string
	BreakerMode  string
	StartedAt    time.Time
}

// Failure captures a failed upstream attempt without exposing it to the downstream client.
type Failure struct {
	Attempt    Attempt
	StatusCode int
	Err        error
	Retryable  bool
}

// Selection records a selected channel and the model sent to that channel.
type Selection struct {
	Channel Channel
	Model   string
}

// SelectionRequest is one channel lookup for a requested model.
type SelectionRequest struct {
	Model    string
	Policy   RoutingPolicy
	Excluded map[string]struct{}
}

// Selector supplies failover candidates while excluding channels already exhausted.
type Selector interface {
	Select(request SelectionRequest) (Selection, error)
	// HasCandidate reports whether any channel could serve the model without
	// consuming weighted round-robin state.
	HasCandidate(model string, policy RoutingPolicy) bool
}

// FailureObserver receives attempt outcomes so future selectors can implement cooldowns.
type FailureObserver interface {
	RecordFailure(failure Failure)
	RecordSuccess(attempt Attempt)
}
