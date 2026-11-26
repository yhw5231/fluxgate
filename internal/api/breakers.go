package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
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
// only the gateway can answer, because the credential itself never leaves it. A
// request that names a credential instead clears it in every upstream that holds
// it, or in the one `site_id` names: a key string can be an account of two
// upstreams, and those are two accounts.
func (s *Server) breakerScopes(w http.ResponseWriter, body map[string]any) ([]breaker.Scope, bool) {
	scope, _ := body["scope"].(string)
	scope = strings.TrimSpace(scope)
	if scope == "" || scope == "all" {
		return nil, true
	}
	channelID := stringField(body, "channel_id")
	keyID := stringField(body, "key_id")
	model := stringField(body, "model")
	siteID, ok := s.siteField(w, body)
	if !ok {
		return nil, false
	}
	// A site-scoped request names an upstream and nothing else, so it is answered
	// before the questions a circuit-scoped one has to answer.
	if scope == "site" {
		if siteID <= 0 {
			writeError(w, http.StatusBadRequest, "invalid_configuration",
				"a site-scoped reset needs the site_id of the upstream")
			return nil, false
		}
		return s.siteScopes(siteID), true
	}
	if channelID == "" && keyID == "" {
		writeError(w, http.StatusBadRequest, "invalid_configuration",
			"a circuit is identified by a channel, a key, or both")
		return nil, false
	}
	switch scope {
	case "channel":
		return s.lineScopes(channelID, model), true
	case "key":
		return s.keyScopes(keyID, "", siteID), true
	case "key_model":
		return s.keyScopes(keyID, model, siteID), true
	default:
		writeError(w, http.StatusBadRequest, "invalid_configuration",
			"scope must be all, site, channel, key, or key_model")
		return nil, false
	}
}

// siteScopes lists every circuit that can hold one upstream's lines out of
// rotation: each line's own, and the circuits filed under each of its credentials.
// A circuit filed under a credential covers every line presenting it, so the lines
// of one upstream are the whole of what an operator means by bringing that
// upstream back — and it is how a circuit the browser cannot name, because it is
// filed under the credential, is still recoverable from the console.
func (s *Server) siteScopes(siteID int64) []breaker.Scope {
	scopes := make([]breaker.Scope, 0)
	for _, route := range s.currentConfiguration().Routes {
		for _, channel := range route.Channels {
			if channel.SiteID != siteID {
				continue
			}
			scopes = append(scopes, s.lineScopes(channel.ID, "")...)
		}
	}
	return dedupeScopes(scopes)
}

// siteField reads the upstream a key-scoped request wants to narrow itself to.
// It is optional: a request that leaves it out addresses the credential itself,
// which is what a client that only knows a key means by it.
func (s *Server) siteField(w http.ResponseWriter, body map[string]any) (int64, bool) {
	raw, present := body["site_id"]
	if !present || raw == nil {
		return 0, true
	}
	number, err := numericField(raw)
	if err != nil || number < 0 {
		writeError(w, http.StatusBadRequest, "invalid_configuration",
			"site_id must be the whole number of an upstream")
		return 0, false
	}
	return number, true
}

// numericField reads a whole number that arrived as a JSON number or as the text
// a console submits.
func numericField(raw any) (int64, error) {
	switch value := raw.(type) {
	case float64:
		if value != float64(int64(value)) {
			return 0, errors.New("not a whole number")
		}
		return int64(value), nil
	case int64:
		return value, nil
	case string:
		return strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	default:
		return 0, errors.New("not a number")
	}
}

// keyScopes lists every circuit filed under one credential: the whole-key one, or
// the one filed for a single model, in every upstream that holds the credential
// and — when the request named one — in that upstream alone. The circuit recorded
// for a given upstream has to be named by its own site, because the site is part
// of what the circuit is filed under; the extra scope is harmless when nothing was
// recorded for it, since clearing a circuit that does not exist removes nothing.
func (s *Server) keyScopes(keyID, model string, siteID int64) []breaker.Scope {
	scopes := []breaker.Scope{{SiteID: siteID, KeyID: keyID, Model: model}}
	if s.BreakerSnapshotter == nil {
		return scopes
	}
	for scope := range s.BreakerSnapshotter.Snapshot() {
		if scope.ChannelID != "" || scope.KeyID != keyID || scope.Model != model {
			continue
		}
		if siteID != 0 && scope.SiteID != siteID {
			continue
		}
		scopes = append(scopes, scope)
	}
	return dedupeScopes(scopes)
}

// lineScopes lists every circuit that can hold one line out of rotation: the
// line's own, the credential's shared one, the credential's per-model one, and
// every per-model circuit already recorded for that credential — a pattern route
// is asked for many model names, and all of them reach the same line. The
// credential's circuits are the ones filed for this line's own upstream: another
// upstream's circuit under the same key string is another account's.
func (s *Server) lineScopes(channelID, model string) []breaker.Scope {
	scopes := []breaker.Scope{{ChannelID: channelID}}
	channel, known := s.findChannel(channelID)
	if !known || channel.APIKey == "" {
		return scopes
	}
	scopes = append(scopes, breaker.Scope{SiteID: channel.SiteID, KeyID: channel.APIKey})
	if model != "" {
		scopes = append(scopes, breaker.Scope{SiteID: channel.SiteID, KeyID: channel.APIKey, Model: model})
	}
	if s.BreakerSnapshotter != nil {
		for scope := range s.BreakerSnapshotter.Snapshot() {
			if scope.KeyID == channel.APIKey && scope.SiteID == channel.SiteID && scope.Model != "" {
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
