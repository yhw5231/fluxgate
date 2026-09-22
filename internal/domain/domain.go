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
	// SitePriority is the upstream's own priority. It outranks Priority, the
	// line's: every line of a preferred upstream is tried before any line of a
	// lower-priority one, so a key mode that orders one upstream's keys never
	// lifts them past a preferred upstream.
	SitePriority int
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
// ordered matching pattern, returning the requested name when nothing maps. A
// key or pattern written for one spelling of a model also answers for the other
// spellings of it, so a mapping written for "deepseek-v4.1-flash" covers a
// request for "cline-free/deepseek-v4.1-flash:free".
func (m ModelMapping) Resolve(requested string) string {
	for _, entry := range m {
		if strings.TrimSpace(entry.Target) == "" {
			continue
		}
		if pattern.Equivalent(entry.Pattern, requested) {
			return strings.TrimSpace(entry.Target)
		}
	}
	for _, entry := range m {
		if strings.TrimSpace(entry.Target) == "" {
			continue
		}
		if pattern.MatchName(requested, entry.Pattern) {
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

// DeniesModel reports whether any deny pattern matches the requested model. A
// pattern written for the plain model name also covers the decorated spellings
// of it, so a deny list cannot be walked around by asking for a channel's own
// spelling of a model the key may not use.
func (p RoutingPolicy) DeniesModel(model string) bool {
	for _, candidate := range p.DeniedModelPatterns {
		if pattern.MatchName(model, candidate) {
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
// routes, while an exact route pattern is always visible. A name is compared by
// model identity rather than as a string, so an allowed route answers for every
// spelling of the model it exposes.
func AllowsModel(routes []Route, policy RoutingPolicy, model string) bool {
	if policy.DeniesModel(model) {
		return false
	}
	for _, route := range routes {
		if route.Enabled && pattern.Equivalent(route.ModelPattern, model) && pattern.IsExact(route.ModelPattern) {
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
		if pattern.Equivalent(route.ExposedName(), model) {
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

// FailoverPolicy decides how far a request may travel after a channel failed.
// Both flags are independent steps: switching off Enabled keeps the request on
// the channel it started with, and switching off CrossUpstream keeps it on that
// channel's upstream while still allowing its other keys to be tried.
type FailoverPolicy struct {
	Enabled       bool
	CrossUpstream bool
}

// Request is the protocol-neutral input passed through selection and dispatch.
type Request struct {
	Method  string
	Path    string
	Headers http.Header
	Body    []byte
	Model   string
	Policy  RoutingPolicy
	// RequestID identifies this downstream request. It travels into every attempt
	// the request makes, which is what lets a circuit count one failing request
	// once instead of once per attempt, and it is what a record of the request is
	// filed under.
	RequestID string
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
	// RequestID identifies the downstream request this attempt belongs to. A
	// circuit counts a request that fails on it once, however many attempts that
	// request made, which is what the breaker uses the identity for. An empty
	// value means the attempt belongs to no identified request and is counted on
	// its own.
	RequestID string
}

// AttemptTrace is one upstream dispatch as the request log records it: which line
// was used, what that upstream answered, and how long it took. Response carries
// the beginning of the upstream's own body when the attempt failed, because that
// text is what explains the failure.
type AttemptTrace struct {
	Number      int       `json:"number"`
	ChannelID   string    `json:"channel_id"`
	ChannelName string    `json:"channel_name"`
	Model       string    `json:"model"`
	StatusCode  int       `json:"status,omitempty"`
	Error       string    `json:"error,omitempty"`
	Retryable   bool      `json:"retryable,omitempty"`
	StartedAt   time.Time `json:"started_at"`
	DurationMS  int64     `json:"duration_ms"`
	Response    string    `json:"response,omitempty"`
}

// RequestTrace is what became of one downstream request: every upstream attempt
// it made, in the order it made them. It is filled in whether the request was
// served or not, so a request that failed can be explained by the attempts that
// led to it.
type RequestTrace struct {
	Attempts []AttemptTrace `json:"attempts"`
}

// LastFailure returns the last attempt that did not succeed, which is the one
// that explains why a request failed.
func (t RequestTrace) LastFailure() (AttemptTrace, bool) {
	for index := len(t.Attempts) - 1; index >= 0; index-- {
		attempt := t.Attempts[index]
		if attempt.StatusCode == 0 || attempt.StatusCode >= 300 || attempt.Error != "" {
			return attempt, true
		}
	}
	return AttemptTrace{}, false
}

// RequestRecord is one downstream request as the request log keeps it: what was
// asked for, which lines were tried, what the upstreams answered, and how the
// request ended. It names the client key by its row rather than by its secret,
// and the upstream by the line an attempt ran on, so the record never carries a
// credential.
type RequestRecord struct {
	// ID is the row the record was stored as, which is what a view reads forward
	// from when it refreshes. It is zero for a record that has not been stored.
	ID           int64
	RequestID    string
	At           time.Time
	Method       string
	Path         string
	ClientIP     string
	KeyID        int64
	KeyName      string
	Model        string
	Stream       bool
	Status       int
	ErrorCode    string
	ErrorMessage string
	DurationMS   int64
	Attempts     []AttemptTrace
}

// Failed reports whether the record describes a request that was not served.
func (r RequestRecord) Failed() bool {
	return r.Status == 0 || r.Status >= 400
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
	// OnlyChannel and OnlySiteID pin a lookup to one channel or to one upstream
	// site. The retry loop uses them to hold a request on the channel it already
	// started with when failover is switched off, or on that channel's upstream
	// when only same-upstream failover is allowed.
	OnlyChannel string
	OnlySiteID  int64
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
