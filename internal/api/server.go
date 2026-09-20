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
	"sync/atomic"
	"time"

	"github.com/yhw5231/fluxgate/internal/breaker"
	"github.com/yhw5231/fluxgate/internal/console"
	"github.com/yhw5231/fluxgate/internal/domain"
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
	Engine              *proxy.Engine
	Authenticator       Authenticator
	ManagementToken     string
	Configuration       store.Configuration
	Routes              []domain.Route
	BreakerSnapshotter  BreakerSnapshotter
	Models              []string
	MaxRequestBodyBytes int64
	Logger              *slog.Logger
	StartedAt           time.Time
	Ready               atomic.Bool
}

// Handler returns an isolated HTTP handler without modifying the TypeScript server.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /console/", console.Handler())
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /readyz", s.handleReady)
	mux.HandleFunc("GET /management/status", s.handleStatus)
	mux.HandleFunc("GET /management/snapshot", s.handleManagementSnapshot)
	mux.HandleFunc("GET /v1/models", s.handleModels)
	mux.HandleFunc("POST /v1/chat/completions", s.handleProxy)
	mux.HandleFunc("POST /v1/responses", s.handleProxy)
	mux.HandleFunc("POST /v1/messages", s.handleProxy)
	return s.loggingMiddleware(mux)
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

func (s *Server) authenticateManagement(w http.ResponseWriter, r *http.Request) bool {
	expected := strings.TrimSpace(s.ManagementToken)
	if expected == "" {
		writeError(w, http.StatusServiceUnavailable, "management_auth_not_configured", "management authentication is not configured")
		return false
	}
	credential := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if credential == "" {
		credential = strings.TrimSpace(r.Header.Get("X-Management-Token"))
	}
	if len(credential) != len(expected) || subtle.ConstantTimeCompare([]byte(credential), []byte(expected)) != 1 {
		writeError(w, http.StatusUnauthorized, "unauthorized", "valid management credentials are required")
		return false
	}
	return true
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if !s.authenticateManagement(w, r) {
		return
	}
	started := s.StartedAt
	if started.IsZero() {
		started = time.Now()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"service":     "fluxgate",
		"ready":       s.Engine != nil && s.Ready.Load(),
		"model_count": len(s.Models),
		"uptime_ms":   time.Since(started).Milliseconds(),
	})
}

func (s *Server) handleManagementSnapshot(w http.ResponseWriter, r *http.Request) {
	if !s.authenticateManagement(w, r) {
		return
	}

	type modelMappingSnapshot struct {
		Pattern string `json:"pattern"`
		Target  string `json:"target"`
	}
	type channelSnapshot struct {
		ID              string                 `json:"id"`
		Name            string                 `json:"name"`
		Enabled         bool                   `json:"enabled"`
		Priority        int                    `json:"priority"`
		Weight          int                    `json:"weight"`
		RoutingStrategy string                 `json:"routing_strategy"`
		BreakerMode     string                 `json:"breaker_mode"`
		ProxySource     string                 `json:"proxy_source"`
		ModelMappings   []modelMappingSnapshot `json:"model_mappings"`
	}
	type breakerSnapshot struct {
		Scope               string `json:"scope"`
		ChannelID           string `json:"channel_id,omitempty"`
		KeyID               string `json:"key_id,omitempty"`
		Model               string `json:"model,omitempty"`
		ConsecutiveFailures int    `json:"consecutive_failures"`
		CooldownLevel       int    `json:"cooldown_level"`
		BlockedUntil        string `json:"blocked_until,omitempty"`
		Disabled            bool   `json:"disabled"`
	}

	channels := make([]channelSnapshot, 0, len(s.Configuration.Channels))
	for _, route := range s.Configuration.Routes {
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
			channels = append(channels, channelSnapshot{
				ID:              channel.ID,
				Name:            channel.Name,
				Enabled:         channel.Enabled,
				Priority:        channel.Priority,
				Weight:          channel.Weight,
				RoutingStrategy: channel.RoutingStrategy,
				BreakerMode:     channel.BreakerMode,
				ProxySource:     proxySource,
				ModelMappings:   mappings,
			})
		}
	}
	// Channels are rendered per route so the panel can show each channel
	// together with the model mappings that apply to it.
	sort.Slice(channels, func(i, j int) bool {
		if channels[i].Priority != channels[j].Priority {
			return channels[i].Priority > channels[j].Priority
		}
		if channels[i].Name != channels[j].Name {
			return channels[i].Name < channels[j].Name
		}
		return channels[i].ID < channels[j].ID
	})

	breakers := make([]breakerSnapshot, 0)
	if s.BreakerSnapshotter != nil {
		states := s.BreakerSnapshotter.Snapshot()
		breakers = make([]breakerSnapshot, 0, len(states))
		for scope, state := range states {
			scopeName := "channel"
			if scope.KeyID != "" && scope.Model != "" {
				scopeName = "key_model"
			} else if scope.KeyID != "" {
				scopeName = "key"
			}
			blockedUntil := ""
			if !state.BlockedUntil.IsZero() {
				blockedUntil = state.BlockedUntil.UTC().Format(time.RFC3339Nano)
			}
			breakers = append(breakers, breakerSnapshot{
				Scope:               scopeName,
				ChannelID:           scope.ChannelID,
				KeyID:               scope.KeyID,
				Model:               scope.Model,
				ConsecutiveFailures: state.ConsecutiveFailures,
				CooldownLevel:       state.CooldownLevel,
				BlockedUntil:        blockedUntil,
				Disabled:            state.Disabled,
			})
		}
		sort.Slice(breakers, func(i, j int) bool {
			left := breakers[i].Scope + "\x00" + breakers[i].ChannelID + "\x00" + breakers[i].KeyID + "\x00" + breakers[i].Model
			right := breakers[j].Scope + "\x00" + breakers[j].ChannelID + "\x00" + breakers[j].KeyID + "\x00" + breakers[j].Model
			return left < right
		})
	}

	models := append([]string(nil), s.Models...)
	sort.Strings(models)
	writeJSON(w, http.StatusOK, map[string]any{
		"generated_at": time.Now().UTC().Format(time.RFC3339Nano),
		"loaded_at":    s.Configuration.LoadedAt.UTC().Format(time.RFC3339Nano),
		"channels":     channels,
		"models":       models,
		"breakers":     breakers,
	})
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	key, ok := s.authenticate(w, r)
	if !ok {
		return
	}

	policy := key.Policy()
	data := make([]map[string]any, 0, len(s.Models))
	for _, model := range s.Models {
		if !domain.AllowsModel(s.Routes, policy, model) {
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

	limit := s.MaxRequestBodyBytes
	if limit <= 0 {
		limit = 8 << 20
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) {
			writeError(w, http.StatusRequestEntityTooLarge, "request_too_large", "request body exceeds the configured limit")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid_request", "failed to read request body")
		return
	}

	var envelope struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "request body must be a JSON object")
		return
	}
	if strings.TrimSpace(envelope.Model) == "" {
		writeError(w, http.StatusBadRequest, "missing_model", "request model is required")
		return
	}
	policy := key.Policy()
	if !domain.AllowsModel(s.Routes, policy, envelope.Model) {
		writeError(w, http.StatusForbidden, "model_not_allowed", "requested model is not allowed for this API key")
		return
	}

	result, err := s.Engine.Forward(r.Context(), domain.Request{
		Method:  http.MethodPost,
		Path:    r.URL.Path,
		Headers: sanitizedHeaders(r.Header),
		Body:    body,
		Model:   envelope.Model,
		Policy:  policy,
	})
	if err != nil {
		// A model with no usable route or channel is an availability problem,
		// not a bad gateway: no upstream request was attempted.
		if errors.Is(err, router.ErrNoChannel) || errors.Is(err, router.ErrModelNotRoutable) {
			writeError(w, http.StatusServiceUnavailable, "no_available_channel", "no upstream channel can serve the requested model")
			return
		}
		writeError(w, http.StatusBadGateway, "upstream_unavailable", "upstream request failed")
		return
	}
	defer result.Response.Body.Close()

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
	writeJSON(w, status, map[string]any{
		"error": map[string]string{
			"code":    code,
			"message": message,
			"type":    "gateway_error",
		},
	})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
