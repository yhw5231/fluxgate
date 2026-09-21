package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/yhw5231/fluxgate/internal/breaker"
)

// Bringing a channel back into service.
//
//	POST /management/breakers/reset
//
// A cooled-down channel returns to rotation by itself when its cooldown expires,
// but an operator who has just fixed an upstream should not have to wait, and a
// channel tripped in disable mode would never return at all. This endpoint
// clears the recorded circuit, which is the "recovery" half of the breaker
// settings the console exposes.

// BreakerResetter clears recorded circuit state. A scope of the empty value
// clears everything the gateway has recorded.
type BreakerResetter interface {
	ResetBreakerStates(ctx context.Context, scopes []breaker.Scope) (int64, error)
}

type breakerResetRequest struct {
	Scope     string `json:"scope"`
	ChannelID string `json:"channel_id"`
	KeyID     string `json:"key_id"`
	Model     string `json:"model"`
}

// handleBreakerReset clears one circuit, or all of them.
func (s *Server) handleBreakerReset(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeConfigurationWrite(w, r) {
		return
	}
	if s.BreakerReset == nil {
		writeError(w, http.StatusServiceUnavailable, "breakers_not_supported",
			"this gateway has no recorded breaker state to clear")
		return
	}
	body, ok := decodeConfigurationBody(w, r)
	if !ok {
		return
	}
	scopes, ok := breakerScopes(w, body)
	if !ok {
		return
	}
	removed, err := s.BreakerReset.ResetBreakerStates(r.Context(), scopes)
	if err != nil {
		s.writeConfigurationFailure(w, r, err)
		return
	}
	s.logConfigurationChange(r, "console_breakers_reset", "breakers", int64(len(scopes)))
	writeJSON(w, http.StatusOK, map[string]any{"reset": removed})
}

// breakerScopes reads the circuit a request wants cleared. A request that names
// nothing clears every circuit, which is what the console's "全部恢复" action
// sends.
func breakerScopes(w http.ResponseWriter, body map[string]any) ([]breaker.Scope, bool) {
	scope, _ := body["scope"].(string)
	scope = strings.TrimSpace(scope)
	if scope == "" || scope == "all" {
		return nil, true
	}
	channelID := stringField(body, "channel_id")
	keyID := stringField(body, "key_id")
	model := stringField(body, "model")
	if channelID == "" && keyID == "" {
		writeError(w, http.StatusBadRequest, "invalid_configuration",
			"a circuit is identified by a channel, a key, or both")
		return nil, false
	}
	switch scope {
	case "channel":
		return []breaker.Scope{{ChannelID: channelID}}, true
	case "key":
		// The key scope carries no channel, which is how the breaker records it:
		// a key-cooled circuit is shared by every channel presenting that key.
		return []breaker.Scope{{KeyID: keyID}}, true
	case "key_model":
		return []breaker.Scope{{KeyID: keyID, Model: model}}, true
	default:
		writeError(w, http.StatusBadRequest, "invalid_configuration",
			"scope must be all, channel, key, or key_model")
		return nil, false
	}
}

func stringField(body map[string]any, name string) string {
	value, _ := body[name].(string)
	return strings.TrimSpace(value)
}
