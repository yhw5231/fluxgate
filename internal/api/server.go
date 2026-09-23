package api

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yhw5231/fluxgate/internal/breaker"
	"github.com/yhw5231/fluxgate/internal/console"
	"github.com/yhw5231/fluxgate/internal/domain"
	"github.com/yhw5231/fluxgate/internal/policy"
	"github.com/yhw5231/fluxgate/internal/proxy"
	"github.com/yhw5231/fluxgate/internal/router"
	"github.com/yhw5231/fluxgate/internal/store"
)

// Authenticator validates a downstream credential without exposing it.
type Authenticator interface {
	AuthenticateDownstreamKey(context.Context, string, time.Time) (store.DownstreamAPIKey, error)
}

// BreakerSnapshotter returns a defensive runtime snapshot for read-only management views.
type BreakerSnapshotter interface {
	Snapshot() map[breaker.Scope]breaker.State
}

// Server exposes OpenAI-compatible proxy routes and operational endpoints.
type Server struct {
	Engine          *proxy.Engine
	Authenticator   Authenticator
	ManagementToken string
	// Sessions backs console sign-in. When nil, the console and management
	// endpoints accept only the management token.
	Sessions SessionStore
	// Configuration is the snapshot the management views report. It is replaced
	// by SetConfiguration after a console write, so it is read through the
	// accessors below rather than directly.
	Configuration store.Configuration
	Routes        []domain.Route
	// ConfigStore performs configuration writes. When nil, the console is
	// read-only and every write endpoint answers 503.
	ConfigStore ConfigurationStore
	// Applier installs a reloaded snapshot in the routing engine. When nil, a
	// change is reported by the API and applied to proxied traffic at the next
	// restart.
	Applier            ConfigurationApplier
	BreakerSnapshotter BreakerSnapshotter
	// BreakerReset clears recorded circuits, which is how the console brings a
	// cooled-down or disabled channel back into service immediately.
	BreakerReset BreakerResetter
	// Prober asks an upstream for its model list. When nil, a default client with
	// the gateway's own timeouts is used.
	Prober UpstreamProber
	// PolicyDefaults is the runtime policy the environment configured. A console
	// write overrides individual values of it; it is also what a cleared override
	// returns to.
	PolicyDefaults policy.Policy
	// RequestLog keeps the record of the requests the gateway served, which is
	// what the console's request view reads and what explains a failure after the
	// fact. When nil, no record is kept and the request endpoints answer 503.
	RequestLog          RequestLog
	Models              []string
	MaxRequestBodyBytes int64
	Logger              *slog.Logger
	StartedAt           time.Time
	Ready               atomic.Bool

	// configMu guards the snapshot fields above, which the console replaces
	// while request handlers read them.
	configMu sync.RWMutex

	// loginLimiter throttles console sign-in attempts per client address. It is
	// created on first use so a Server assembled by tests needs no constructor.
	loginLimiterOnce sync.Once
	loginLimiter     *loginLimiter
}

// SetConfiguration replaces the configuration snapshot the management endpoints
// report and the proxy endpoints authorize against. The routing engine is
// updated separately by the ConfigurationApplier the process supplies.
func (s *Server) SetConfiguration(configuration store.Configuration) {
	s.configMu.Lock()
	defer s.configMu.Unlock()
	s.Configuration = configuration
	s.Routes = configuration.Routes
	s.Models = configuration.Models
}

// currentConfiguration returns the snapshot under the read lock. The returned
// value shares its slices with the stored one, which is safe because a snapshot
// is never mutated after it is installed.
func (s *Server) currentConfiguration() store.Configuration {
	s.configMu.RLock()
	defer s.configMu.RUnlock()
	return s.Configuration
}

func (s *Server) currentRoutes() []domain.Route {
	s.configMu.RLock()
	defer s.configMu.RUnlock()
	return s.Routes
}

func (s *Server) currentModels() []string {
	s.configMu.RLock()
	defer s.configMu.RUnlock()
	return s.Models
}

// currentPolicy is the runtime policy the gateway is applying: the environment's
// defaults with whatever overrides the settings table holds.
func (s *Server) currentPolicy() policy.Policy {
	defaults := s.PolicyDefaults
	if defaults.Retry.MaxAttempts <= 0 {
		// A gateway assembled without explicit defaults — a test, or an embedder
		// that only wants the proxy — runs with the built-in ones.
		defaults = policy.Default()
	}
	applied, _ := policy.FromSettings(s.currentConfiguration().Settings, defaults)
	return applied
}

// limiter returns the per-server sign-in limiter, creating it on first use.
func (s *Server) limiter() *loginLimiter {
	s.loginLimiterOnce.Do(func() {
		s.loginLimiter = newLoginLimiter(time.Now)
	})
	return s.loginLimiter
}

// discardLogger is what a server assembled without one logs to.
var discardLogger = slog.New(slog.NewTextHandler(io.Discard, nil))

// logger returns the server's logger, or one that discards, so a call site does
// not have to check for the nil a test-assembled server carries.
func (s *Server) logger() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return discardLogger
}

// Handler returns an isolated HTTP handler without modifying the TypeScript server.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	// A bare root hit is someone looking for the console, so send it there
	// instead of answering Go's default plain-text 404.
	mux.HandleFunc("GET /{$}", handleConsoleRedirect)
	// Redirected here rather than by the mux so the Location header stays
	// correct when the gateway is mounted behind a path prefix.
	mux.HandleFunc("GET /console", handleConsoleRedirect)
	mux.Handle("GET /console/", console.Handler())
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /readyz", s.handleReady)
	// Console sign-in. These are unauthenticated by necessity: they are what
	// hands out a session in the first place. The login route is rate limited.
	mux.HandleFunc("GET /management/session", s.handleSession)
	mux.HandleFunc("POST /management/login", s.handleLogin)
	mux.HandleFunc("POST /management/logout", s.handleLogout)
	// Reachable while a password change is pending, which is the only way out of
	// that state.
	mux.HandleFunc("POST /management/password", s.handleChangePassword)
	mux.HandleFunc("GET /management/status", s.handleStatus)
	mux.HandleFunc("GET /management/snapshot", s.handleManagementSnapshot)
	// Console configuration management. Every write is authenticated on its own,
	// bounded, validated against the tables it touches, and followed by a reload
	// so the gateway routes with what the console just saved.
	mux.HandleFunc("GET /management/configuration", s.handleConfiguration)
	mux.HandleFunc("POST /management/configuration/{resource}", s.handleConfigurationCreate)
	mux.HandleFunc("PUT /management/configuration/{resource}/{id}", s.handleConfigurationUpdate)
	mux.HandleFunc("DELETE /management/configuration/{resource}/{id}", s.handleConfigurationDelete)
	mux.HandleFunc("POST /management/configuration/keys/{id}/rotate", s.handleConfigurationKeyRotate)
	// Reading a client key back, for the operator who lost one. It is a POST
	// because it is a deliberate action on one row rather than a listing.
	mux.HandleFunc("POST /management/configuration/keys/{id}/reveal", s.handleConfigurationKeyReveal)
	// The runtime policy is stored like configuration but is not a row of a
	// table, so it has its own endpoint rather than a resource name.
	mux.HandleFunc("PUT /management/policy", s.handlePolicyUpdate)
	// Asking an upstream what it serves, so a model can be picked rather than
	// typed.
	mux.HandleFunc("POST /management/upstreams/models", s.handleUpstreamModels)
	// The record of the requests the gateway served, which is where a failure is
	// looked up: the upstream's own answer to a failed request is kept here and
	// nowhere else.
	mux.HandleFunc("GET /management/requests", s.handleRequestLog)
	mux.HandleFunc("DELETE /management/requests", s.handleRequestLogClear)
	// Clearing a recorded circuit, which is the manual half of breaker recovery.
	mux.HandleFunc("POST /management/breakers/reset", s.handleBreakerReset)
	mux.HandleFunc("GET /v1/models", s.handleModels)
	mux.HandleFunc("POST /v1/chat/completions", s.handleProxy)
	mux.HandleFunc("POST /v1/responses", s.handleProxy)
	mux.HandleFunc("POST /v1/messages", s.handleProxy)
	// Anything else that a browser navigated to is a visitor looking for the
	// console, so unknown pages redirect to it. The API namespaces keep their
	// 404s: a mistyped endpoint must not answer with a login page.
	mux.HandleFunc("GET /{path...}", handleConsoleRedirect)
	return s.loggingMiddleware(mux)
}

// apiNamespaces are the path prefixes that belong to the API surface. A request
// under one of them is an endpoint call rather than a browser navigation, so an
// unknown path there is a 404 instead of a console redirect.
var apiNamespaces = []string{"/v1/", "/management/", "/console/"}

// isAPIPath reports whether a path is part of the API surface rather than a
// page a browser navigated to.
func isAPIPath(path string) bool {
	for _, namespace := range apiNamespaces {
		if strings.HasPrefix(path, namespace) {
			return true
		}
	}
	return false
}

// handleConsoleRedirect sends any browser navigation the gateway does not
// otherwise serve to the console index. This is what makes a bare domain
// (https://gateway.example/) open the console, and it covers unknown paths such
// as /admin so a visitor lands somewhere useful instead of on a bare 404.
//
// API namespaces are excluded: a mistyped endpoint must keep answering 404, both
// because a client is not a browser and because returning a login page there
// would hide the real mistake.
func handleConsoleRedirect(w http.ResponseWriter, r *http.Request) {
	if isAPIPath(r.URL.Path) {
		http.NotFound(w, r)
		return
	}
	// A proxy that strips a path prefix knows the public prefix and can report it
	// in X-Forwarded-Prefix. That is the only way to recover it for a visitor who
	// arrived at a slash-less path such as https://host/gateway, because the
	// browser treats the last segment as a file there and resolves a relative
	// reference against the parent directory.
	if prefix := forwardedPrefix(r); prefix != "" {
		w.Header().Set("Location", prefix+"/console/")
		w.WriteHeader(http.StatusTemporaryRedirect)
		return
	}
	// Without that header the Location stays a relative reference, which the
	// browser resolves against the public URL it requested. http.Redirect cannot
	// be used here because it rewrites the target into an absolute path derived
	// from the path the gateway received, which is the prefix-stripped one.
	w.Header().Set("Location", consoleRedirectTarget(r.URL.Path))
	w.WriteHeader(http.StatusTemporaryRedirect)
}

// forwardedPrefix returns a sanitized X-Forwarded-Prefix value, or an empty
// string when the header is absent or unusable. The value is echoed into a
// Location header, so anything that could escape the intended origin — a
// relative value, a traversal segment, or a scheme — is rejected rather than
// normalized.
func forwardedPrefix(r *http.Request) string {
	value := strings.TrimSpace(r.Header.Get("X-Forwarded-Prefix"))
	if value == "" || !strings.HasPrefix(value, "/") {
		return ""
	}
	if strings.Contains(value, "\\") || strings.Contains(value, "//") {
		return ""
	}
	if strings.ContainsAny(value, "\r\n") {
		return ""
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == ".." || segment == "." {
			return ""
		}
	}
	trimmed := strings.TrimSuffix(value, "/")
	if trimmed == "" {
		return ""
	}
	return trimmed
}

// consoleRedirectTarget builds a relative reference to the console index from
// the path the gateway received. The path is normalized first, because Go's mux
// redirects a request for "/a/../b" before matching and the target must not be
// computed from the unnormalized form.
//
// Depth is the number of segments that would have to be removed for the browser
// to arrive at the console: a leaf path climbs one level per segment, while a
// trailing slash means the last segment is a directory the browser is already
// inside and must be climbed too.
func consoleRedirectTarget(requestPath string) string {
	trimmed := strings.Trim(requestPath, "/")
	if trimmed == "" {
		return "console/"
	}
	depth := len(strings.Split(trimmed, "/"))
	if !strings.HasSuffix(requestPath, "/") {
		depth--
	}
	return strings.Repeat("../", depth) + "console/"
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"status":  "ok",
		"service": "fluxgate",
	})
}

func (s *Server) handleReady(w http.ResponseWriter, _ *http.Request) {
	if s.Engine == nil || !s.Ready.Load() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not_ready"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

// verifyManagementToken reports whether the request carries the configured
// management token. It performs no authorization itself and writes no response,
// so callers can combine it with other credential sources.
func (s *Server) verifyManagementToken(r *http.Request) bool {
	expected := strings.TrimSpace(s.ManagementToken)
	if expected == "" {
		return false
	}
	credential := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if credential == "" {
		credential = strings.TrimSpace(r.Header.Get("X-Management-Token"))
	}
	return len(credential) == len(expected) && subtle.ConstantTimeCompare([]byte(credential), []byte(expected)) == 1
}

func (s *Server) authenticateManagement(w http.ResponseWriter, r *http.Request) bool {
	if s.verifyManagementToken(r) {
		return true
	}
	if strings.TrimSpace(s.ManagementToken) == "" && s.Sessions == nil {
		writeError(w, http.StatusServiceUnavailable, "management_auth_not_configured", "management authentication is not configured")
		return false
	}
	writeError(w, http.StatusUnauthorized, "unauthorized", "valid management credentials are required")
	return false
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if !s.authenticateConsole(w, r) {
		return
	}
	started := s.StartedAt
	if started.IsZero() {
		started = time.Now()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"service":     "fluxgate",
		"ready":       s.Engine != nil && s.Ready.Load(),
		"model_count": len(s.currentModels()),
		"uptime_ms":   time.Since(started).Milliseconds(),
	})
}

func (s *Server) handleManagementSnapshot(w http.ResponseWriter, r *http.Request) {
	if !s.authenticateConsole(w, r) {
		return
	}

	type modelMappingSnapshot struct {
		Pattern string `json:"pattern"`
		Target  string `json:"target"`
	}
	// lineCircuitSnapshot is the circuit state that applies to one line, resolved
	// where the credential is known. It is what lets a view say whether a line is
	// usable, cooling down, or waiting for an operator, without ever naming the
	// credential the circuit is filed under.
	type lineCircuitSnapshot struct {
		Status              string `json:"status"`
		Scope               string `json:"scope,omitempty"`
		Model               string `json:"model,omitempty"`
		BlockedUntil        string `json:"blocked_until,omitempty"`
		CooldownLevel       int    `json:"cooldown_level"`
		ConsecutiveFailures int    `json:"consecutive_failures"`
	}
	type channelSnapshot struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		Enabled bool   `json:"enabled"`
		// SitePriority is the upstream's priority, which is what selection compares
		// first; Priority is the line's own, which it no longer uses.
		SitePriority    int                    `json:"site_priority"`
		Priority        int                    `json:"priority"`
		Weight          int                    `json:"weight"`
		RoutingStrategy string                 `json:"routing_strategy"`
		BreakerMode     string                 `json:"breaker_mode"`
		ProxySource     string                 `json:"proxy_source"`
		ModelMappings   []modelMappingSnapshot `json:"model_mappings"`
		State           lineCircuitSnapshot    `json:"state"`
	}
	type breakerSnapshot struct {
		Scope string `json:"scope"`
		// ChannelID names the line a line-scoped circuit belongs to. A circuit
		// filed under a credential is shared by every line presenting it, so it
		// names those lines instead — the credential itself never leaves the
		// gateway.
		ChannelID           string   `json:"channel_id,omitempty"`
		Lines               []string `json:"lines,omitempty"`
		Model               string   `json:"model,omitempty"`
		ConsecutiveFailures int      `json:"consecutive_failures"`
		CooldownLevel       int      `json:"cooldown_level"`
		BlockedUntil        string   `json:"blocked_until,omitempty"`
		Disabled            bool     `json:"disabled"`
	}

	configuration := s.currentConfiguration()
	// One read of the recorded circuits feeds both the per-line state and the
	// breaker list, so the two views cannot disagree about the same moment.
	circuits := map[breaker.Scope]breaker.State{}
	if s.BreakerSnapshotter != nil {
		circuits = s.BreakerSnapshotter.Snapshot()
	}
	fallbackMode := s.currentPolicy().Breaker.Mode
	now := time.Now()

	channels := make([]channelSnapshot, 0, len(configuration.Channels))
	// A circuit filed under a credential is named by the lines that present it.
	// The credential is what the circuit is keyed by, so it is the only thing that
	// can resolve one to the other, and it never leaves the gateway.
	linesByKey := map[string][]string{}
	for _, route := range configuration.Routes {
		mappings := make([]modelMappingSnapshot, 0, len(route.ModelMapping))
		for _, entry := range route.ModelMapping {
			mappings = append(mappings, modelMappingSnapshot{Pattern: entry.Pattern, Target: entry.Target})
		}
		sort.Slice(mappings, func(i, j int) bool {
			if mappings[i].Pattern == mappings[j].Pattern {
				return mappings[i].Target < mappings[j].Target
			}
			return mappings[i].Pattern < mappings[j].Pattern
		})

		for _, channel := range route.Channels {
			if channel.APIKey != "" && !containsString(linesByKey[channel.APIKey], channel.Name) {
				linesByKey[channel.APIKey] = append(linesByKey[channel.APIKey], channel.Name)
			}
			proxySource := "direct"
			switch {
			case channel.ProxyURL != "":
				proxySource = "key"
			case channel.UseSystemProxy:
				proxySource = "system"
			case channel.SiteProxyURL != "":
				proxySource = "site"
			case channel.SiteUseSystemProxy:
				proxySource = "system"
			}
			state := lineCircuitSnapshot{Status: "ready"}
			if circuit, scope, known := lineCircuit(circuits, channel, route, fallbackMode, now); known {
				state.Scope = circuitScopeName(scope)
				state.Model = scope.Model
				state.CooldownLevel = circuit.CooldownLevel
				state.ConsecutiveFailures = circuit.ConsecutiveFailures
				if !circuit.BlockedUntil.IsZero() {
					state.BlockedUntil = circuit.BlockedUntil.UTC().Format(time.RFC3339Nano)
				}
				switch {
				case circuit.Disabled:
					state.Status = "disabled"
				case circuit.BlockedUntil.After(now):
					state.Status = "cooling"
				}
			}
			if !channel.Enabled {
				// A line the configuration itself holds out of rotation is
				// reported as such whatever a circuit says: bringing it back is
				// the operator's action, not a recovery.
				state.Status = "inactive"
			}
			channels = append(channels, channelSnapshot{
				ID:              channel.ID,
				Name:            channel.Name,
				Enabled:         channel.Enabled,
				SitePriority:    channel.SitePriority,
				Priority:        channel.Priority,
				Weight:          channel.Weight,
				RoutingStrategy: channel.RoutingStrategy,
				BreakerMode:     channel.BreakerMode,
				ProxySource:     proxySource,
				ModelMappings:   mappings,
				State:           state,
			})
		}
	}
	// Channels are rendered per route so the panel can show each channel
	// together with the model mappings that apply to it. They are listed the way
	// the gateway would try them: highest upstream priority first, then by
	// upstream and line so the order is stable between reads.
	sort.Slice(channels, func(i, j int) bool {
		if channels[i].SitePriority != channels[j].SitePriority {
			return channels[i].SitePriority > channels[j].SitePriority
		}
		if channels[i].Name != channels[j].Name {
			return channels[i].Name < channels[j].Name
		}
		return channels[i].ID < channels[j].ID
	})

	breakers := make([]breakerSnapshot, 0, len(circuits))
	for scope, state := range circuits {
		blockedUntil := ""
		if !state.BlockedUntil.IsZero() {
			blockedUntil = state.BlockedUntil.UTC().Format(time.RFC3339Nano)
		}
		lines := append([]string(nil), linesByKey[scope.KeyID]...)
		sort.Strings(lines)
		breakers = append(breakers, breakerSnapshot{
			Scope:               circuitScopeName(scope),
			ChannelID:           scope.ChannelID,
			Lines:               lines,
			Model:               scope.Model,
			ConsecutiveFailures: state.ConsecutiveFailures,
			CooldownLevel:       state.CooldownLevel,
			BlockedUntil:        blockedUntil,
			Disabled:            state.Disabled,
		})
	}
	sort.Slice(breakers, func(i, j int) bool {
		left := breakers[i].Scope + "\x00" + breakers[i].ChannelID + "\x00" + strings.Join(breakers[i].Lines, ",") + "\x00" + breakers[i].Model
		right := breakers[j].Scope + "\x00" + breakers[j].ChannelID + "\x00" + strings.Join(breakers[j].Lines, ",") + "\x00" + breakers[j].Model
		return left < right
	})

	models := append([]string(nil), s.currentModels()...)
	sort.Strings(models)
	writeJSON(w, http.StatusOK, map[string]any{
		"generated_at": time.Now().UTC().Format(time.RFC3339Nano),
		"loaded_at":    configuration.LoadedAt.UTC().Format(time.RFC3339Nano),
		"channels":     channels,
		"models":       models,
		"breakers":     breakers,
	})
}

// lineCircuit resolves the circuit that holds one line out of rotation, together
// with the scope it was filed under.
//
// A line's failures are recorded under one of three circuits: the line itself, the
// credential it presents (shared by every line presenting it), or that credential
// for one upstream model. Only the gateway holds the credential, so the answer is
// resolved here and reported per line rather than by credential to the console.
//
// The circuit the line runs in is asked for first, but a circuit filed under
// another scope still holds it out of rotation — a mode that changed, or a
// per-model circuit for a name a pattern route also matches — so every circuit
// that can block the line is a candidate and the most restrictive one wins.
func lineCircuit(circuits map[breaker.Scope]breaker.State, channel domain.Channel, route domain.Route, fallback breaker.Mode, now time.Time) (breaker.State, breaker.Scope, bool) {
	if len(circuits) == 0 {
		return breaker.State{}, breaker.Scope{}, false
	}
	candidates := []breaker.Scope{
		breaker.ScopeForMode(channel.BreakerMode, fallback, channel.ID, channel.APIKey, router.ActualModel(routedModelName(route), route, channel)),
		{ChannelID: channel.ID},
	}
	if channel.APIKey != "" {
		candidates = append(candidates, breaker.Scope{KeyID: channel.APIKey})
		for scope := range circuits {
			if scope.KeyID == channel.APIKey && scope.Model != "" {
				candidates = append(candidates, scope)
			}
		}
	}

	var (
		selected      breaker.State
		selectedScope breaker.Scope
		selectedRank  = -1
		found         bool
	)
	for _, scope := range candidates {
		state, known := circuits[scope]
		if !known {
			continue
		}
		rank := circuitRank(state, now)
		if !found || rank > selectedRank ||
			(rank == selectedRank && circuitStricter(state, selected, now)) {
			selected, selectedScope, selectedRank, found = state, scope, rank, true
		}
	}
	return selected, selectedScope, found
}

// circuitRank orders circuits by how long they hold a line out of rotation: a
// disabled circuit never recovers on its own, a cooling one recovers when its
// deadline passes, and a circuit with failures recorded but no deadline does not
// hold the line at all.
func circuitRank(state breaker.State, now time.Time) int {
	switch {
	case state.Disabled:
		return 2
	case state.BlockedUntil.After(now):
		return 1
	default:
		return 0
	}
}

// circuitStricter breaks a tie between two circuits of the same rank: the one
// that releases later, then the one that failed further into its backoff.
func circuitStricter(candidate, current breaker.State, now time.Time) bool {
	if !candidate.BlockedUntil.Equal(current.BlockedUntil) {
		return candidate.BlockedUntil.After(now) && !current.BlockedUntil.After(now) ||
			candidate.BlockedUntil.After(current.BlockedUntil)
	}
	if candidate.CooldownLevel != current.CooldownLevel {
		return candidate.CooldownLevel > current.CooldownLevel
	}
	return candidate.ConsecutiveFailures > current.ConsecutiveFailures
}

// circuitScopeName names the kind of circuit a scope describes, which is how a
// recovery request addresses it.
func circuitScopeName(scope breaker.Scope) string {
	switch {
	case scope.KeyID != "" && scope.Model != "":
		return "key_model"
	case scope.KeyID != "":
		return "key"
	default:
		return "channel"
	}
}

// containsString reports whether a list already holds a value.
func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

// routedModelName is the name the routing table lists a route under, which is
// what a client asks for and therefore what a per-model circuit is filed under.
func routedModelName(route domain.Route) string {
	if name := strings.TrimSpace(route.DisplayName); name != "" {
		return name
	}
	return strings.TrimSpace(route.ModelPattern)
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	key, ok := s.authenticate(w, r)
	if !ok {
		return
	}

	policy := key.Policy()
	models := s.currentModels()
	routes := s.currentRoutes()
	data := make([]map[string]any, 0, len(models))
	for _, model := range models {
		if !domain.AllowsModel(routes, policy, model) {
			continue
		}
		if !s.hasRoutableChannel(model, policy) {
			continue
		}
		data = append(data, map[string]any{
			"id":       model,
			"object":   "model",
			"created":  0,
			"owned_by": "fluxgate",
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// hasRoutableChannel reports whether a channel could serve the model, so the
// listing never advertises a model that would immediately fail.
func (s *Server) hasRoutableChannel(model string, policy domain.RoutingPolicy) bool {
	if s.Engine == nil || s.Engine.Selector == nil {
		return true
	}
	return s.Engine.Selector.HasCandidate(model, policy)
}

func (s *Server) handleProxy(w http.ResponseWriter, r *http.Request) {
	if s.Engine == nil {
		writeError(w, http.StatusServiceUnavailable, "gateway_not_ready", "gateway engine is not configured")
		return
	}
	key, ok := s.authenticate(w, r)
	if !ok {
		return
	}

	// Every request that got past authentication leaves a record, whichever way it
	// ends. The record is filled in as the request is handled and written once, so
	// a refusal the gateway itself makes — an unreadable body, a model this key
	// may not use — is as findable as an upstream failure.
	started := time.Now()
	record := domain.RequestRecord{
		RequestID: newRequestID(),
		At:        started.UTC(),
		Method:    r.Method,
		Path:      r.URL.Path,
		ClientIP:  clientIP(r),
		KeyID:     key.ID,
		KeyName:   key.Name,
	}
	// refuse answers the client and records why, so the two cannot disagree about
	// what happened.
	refuse := func(status int, code, message string, details map[string]any) {
		record.Status = status
		record.ErrorCode = code
		record.ErrorMessage = message
		writeErrorDetails(w, status, code, message, details)
	}
	// The client is told which request this was, so a failure it reports can be
	// looked up rather than guessed at from a timestamp.
	w.Header().Set("X-Fluxgate-Request-Id", record.RequestID)
	defer func() {
		record.DurationMS = time.Since(started).Milliseconds()
		s.recordProxiedRequest(r, record)
	}()

	limit := s.MaxRequestBodyBytes
	if limit <= 0 {
		limit = 8 << 20
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) {
			refuse(http.StatusRequestEntityTooLarge, "request_too_large", "request body exceeds the configured limit", nil)
			return
		}
		refuse(http.StatusBadRequest, "invalid_request", "failed to read request body", nil)
		return
	}

	var envelope struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		refuse(http.StatusBadRequest, "invalid_json", "request body must be a JSON object", nil)
		return
	}
	record.Model = strings.TrimSpace(envelope.Model)
	record.Stream = envelope.Stream
	if record.Model == "" {
		refuse(http.StatusBadRequest, "missing_model", "request model is required", nil)
		return
	}
	policy := key.Policy()
	if !domain.AllowsModel(s.currentRoutes(), policy, record.Model) {
		refuse(http.StatusForbidden, "model_not_allowed", "requested model is not allowed for this API key", nil)
		return
	}

	result, err := s.Engine.Forward(r.Context(), domain.Request{
		Method:    http.MethodPost,
		Path:      r.URL.Path,
		Headers:   sanitizedHeaders(r.Header),
		Body:      body,
		Model:     envelope.Model,
		Policy:    policy,
		RequestID: record.RequestID,
	})
	record.Attempts = result.Trace.Attempts
	if err != nil {
		// A model with no usable route or channel is an availability problem,
		// not a bad gateway: no upstream request was attempted.
		failure := classifyDispatchError(err, result.Trace, record.RequestID)
		refuse(failure.Status, failure.Code, failure.Message, failure.Details)
		return
	}
	defer result.Response.Body.Close()

	record.Status = result.Response.StatusCode
	copyResponseHeaders(w.Header(), result.Response.Header)
	w.Header().Set("X-Fluxgate-Upstream-Channel", result.Attempt.ChannelID)
	w.WriteHeader(result.Response.StatusCode)
	if envelope.Stream || strings.Contains(strings.ToLower(result.Response.Header.Get("Content-Type")), "text/event-stream") {
		streamResponse(w, result.Response.Body)
		return
	}
	_, _ = io.Copy(w, result.Response.Body)
}

func (s *Server) authenticate(w http.ResponseWriter, r *http.Request) (store.DownstreamAPIKey, bool) {
	if s.Authenticator == nil {
		writeError(w, http.StatusServiceUnavailable, "authentication_not_configured", "API key authentication is not configured")
		return store.DownstreamAPIKey{}, false
	}
	candidate := extractCredential(r.Header)
	if candidate == "" {
		writeError(w, http.StatusUnauthorized, "invalid_api_key", "a valid API key is required")
		return store.DownstreamAPIKey{}, false
	}
	key, err := s.Authenticator.AuthenticateDownstreamKey(r.Context(), candidate, time.Now().UTC())
	if err != nil {
		writeError(w, http.StatusUnauthorized, "invalid_api_key", "a valid API key is required")
		return store.DownstreamAPIKey{}, false
	}
	return key, true
}

func extractCredential(header http.Header) string {
	if authorization := strings.TrimSpace(header.Get("Authorization")); authorization != "" {
		parts := strings.Fields(authorization)
		if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
			return strings.TrimSpace(parts[1])
		}
	}
	for _, name := range []string{"X-API-Key", "Api-Key"} {
		if value := strings.TrimSpace(header.Get(name)); value != "" {
			return value
		}
	}
	return ""
}

func sanitizedHeaders(source http.Header) http.Header {
	result := source.Clone()
	result.Del("Authorization")
	result.Del("X-API-Key")
	result.Del("Api-Key")
	return result
}

func streamResponse(w http.ResponseWriter, source io.Reader) {
	flusher, canFlush := w.(http.Flusher)
	reader := bufio.NewReader(source)
	buffer := make([]byte, 32*1024)
	for {
		count, err := reader.Read(buffer)
		if count > 0 {
			if _, writeErr := w.Write(buffer[:count]); writeErr != nil {
				return
			}
			if canFlush {
				flusher.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

func (s *Server) loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(recorder, r)
		if s.Logger != nil {
			s.Logger.Info("http_request",
				"method", r.Method,
				"path", r.URL.Path,
				"status", recorder.status,
				"duration_ms", time.Since(started).Milliseconds(),
				"client_ip", clientIP(r),
			)
		}
	})
}

func clientIP(r *http.Request) string {
	if forwardedFor := r.Header.Get("X-Forwarded-For"); forwardedFor != "" {
		for _, value := range strings.Split(forwardedFor, ",") {
			candidate := strings.TrimSpace(value)
			if ip := net.ParseIP(candidate); ip != nil {
				return ip.String()
			}
		}
	}

	if candidate := strings.TrimSpace(r.Header.Get("X-Real-IP")); candidate != "" {
		if ip := net.ParseIP(candidate); ip != nil {
			return ip.String()
		}
	}

	host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr))
	if err == nil {
		if ip := net.ParseIP(host); ip != nil {
			return ip.String()
		}
	}
	if ip := net.ParseIP(strings.TrimSpace(r.RemoteAddr)); ip != nil {
		return ip.String()
	}
	return ""
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (w *statusRecorder) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusRecorder) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func copyResponseHeaders(destination, source http.Header) {
	for name, values := range source {
		if isHopByHopHeader(name) {
			continue
		}
		for _, value := range values {
			destination.Add(name, value)
		}
	}
}

func isHopByHopHeader(name string) bool {
	switch http.CanonicalHeaderKey(name) {
	case "Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade":
		return true
	default:
		return false
	}
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeErrorDetails(w, status, code, message, nil)
}

// writeErrorDetails answers with a gateway error, with whatever the caller knows
// about the failure beside it. The extra fields are how an error that names its
// cause — the upstream's own message, the request an operator can look up —
// reaches the client instead of only the console.
func writeErrorDetails(w http.ResponseWriter, status int, code, message string, details map[string]any) {
	body := map[string]any{
		"code":    code,
		"message": message,
		"type":    "gateway_error",
	}
	for name, value := range details {
		if _, taken := body[name]; taken {
			continue
		}
		body[name] = value
	}
	writeJSON(w, status, map[string]any{"error": body})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
