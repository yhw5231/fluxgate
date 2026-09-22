package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/yhw5231/fluxgate/internal/breaker"
	"github.com/yhw5231/fluxgate/internal/domain"
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
	scopes, ok := s.breakerScopes(w, body)
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

// breakerScopes reads the circuits a request wants cleared. A request that names
// nothing clears every circuit, which is what the console's "全部恢复" action
// sends.
//
// A request that names a line clears every circuit that can hold that line out of
// rotation. Which ones those are — the line's own, the one shared by every line
// presenting the same credential, or that credential for one model — is something
// only the gateway can answer, because the credential itself never leaves it. The
// store's own circuit key is still accepted verbatim, for automation that
// recorded a scope from the snapshot.
func (s *Server) breakerScopes(w http.ResponseWriter, body map[string]any) ([]breaker.Scope, bool) {
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
		return s.lineScopes(channelID, model), true
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

// lineScopes lists every circuit that can hold one line out of rotation: the
// line's own, the credential's shared one, the credential's per-model one, and
// every per-model circuit already recorded for that credential — a pattern route
// is asked for many model names, and all of them reach the same line.
func (s *Server) lineScopes(channelID, model string) []breaker.Scope {
	scopes := []breaker.Scope{{ChannelID: channelID}}
	channel, known := s.findChannel(channelID)
	if !known || channel.APIKey == "" {
		return scopes
	}
	scopes = append(scopes, breaker.Scope{KeyID: channel.APIKey})
	if model != "" {
		scopes = append(scopes, breaker.Scope{KeyID: channel.APIKey, Model: model})
	}
	if s.BreakerSnapshotter != nil {
		for scope := range s.BreakerSnapshotter.Snapshot() {
			if scope.KeyID == channel.APIKey && scope.Model != "" {
				scopes = append(scopes, scope)
			}
		}
	}
	return dedupeScopes(scopes)
}

// findChannel returns the channel the running configuration knows by id, which
// is how a request names the line it wants back in rotation.
func (s *Server) findChannel(id string) (domain.Channel, bool) {
	for _, route := range s.currentConfiguration().Routes {
		for _, channel := range route.Channels {
			if channel.ID == id {
				return channel, true
			}
		}
	}
	return domain.Channel{}, false
}

// dedupeScopes removes the repeats a caller may have assembled, so a circuit is
// cleared — and counted — once.
func dedupeScopes(scopes []breaker.Scope) []breaker.Scope {
	seen := make(map[breaker.Scope]struct{}, len(scopes))
	unique := make([]breaker.Scope, 0, len(scopes))
	for _, scope := range scopes {
		if _, duplicate := seen[scope]; duplicate {
			continue
		}
		seen[scope] = struct{}{}
		unique = append(unique, scope)
	}
	return unique
}

func stringField(body map[string]any, name string) string {
	value, _ := body[name].(string)
	return strings.TrimSpace(value)
}
