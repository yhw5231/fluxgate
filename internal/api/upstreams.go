package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/yhw5231/fluxgate/internal/store"
	"github.com/yhw5231/fluxgate/internal/upstream"
)

// Asking an upstream what it serves.
//
//	POST /management/upstreams/models
//
// The console cannot know the model names an upstream accepts, and typing them
// from memory is how a route ends up pointing at a model the upstream rejects.
// This endpoint asks the upstream itself and answers with the list, so the
// operator picks models instead of spelling them.
//
// The request carries the address a site holds, one credential, and any custom
// headers, which is exactly what the console form has at that moment — the
// upstream does not have to be saved first. When the credential in the form is
// still the mask of a stored one, the upstream can be named by id instead and
// the gateway probes with the key it already holds.

// UpstreamProber asks an upstream which models it serves. It is an interface so
// the gateway can be tested without an upstream to talk to.
type UpstreamProber interface {
	Models(ctx context.Context, request upstream.Request) (upstream.Result, error)
}

// prober returns the probe client, creating the default one on first use.
func (s *Server) prober() UpstreamProber {
	if s.Prober != nil {
		return s.Prober
	}
	return &upstream.Client{}
}

type upstreamProbeRequest struct {
	ID      int64             `json:"id"`
	URL     string            `json:"url"`
	Key     string            `json:"key"`
	Headers map[string]string `json:"headers"`
}

// handleUpstreamModels answers the model list of an upstream.
func (s *Server) handleUpstreamModels(w http.ResponseWriter, r *http.Request) {
	if !s.authenticateConsole(w, r) {
		return
	}
	if !sameOriginRequest(r) {
		writeError(w, http.StatusForbidden, "cross_origin_rejected",
			"an upstream probe must come from the console's own origin")
		return
	}
	body, ok := decodeConfigurationBody(w, r)
	if !ok {
		return
	}
	request, ok := s.upstreamProbeRequest(w, r, body)
	if !ok {
		return
	}

	result, err := s.prober().Models(r.Context(), request)
	if err != nil {
		// A probe the gateway would not make is the client's problem; one the
		// upstream failed is reported as a bad gateway. Both carry a stable
		// reason so the console can phrase them in its own language.
		var failed upstream.Failure
		if !errors.As(err, &failed) {
			// Anything that is not a described probe failure is a fault in the
			// gateway rather than a condition of the upstream.
			if s.Logger != nil {
				s.Logger.Error("upstream_probe_failed", "error", err.Error())
			}
			writeError(w, http.StatusInternalServerError, "upstream_probe_failed",
				"the gateway could not complete the probe")
			return
		}
		details := map[string]any{"reason": failed.Reason, "params": map[string]any{}}
		if failed.Status != 0 {
			details["params"] = map[string]any{"status": failed.Status}
		}
		switch failed.Reason {
		case upstream.ReasonUnreachable:
			writeConfigurationError(w, http.StatusBadGateway, "upstream_unreachable", failed.Error(), details)
		case upstream.ReasonUpstreamStatus:
			writeConfigurationError(w, http.StatusBadGateway, "upstream_probe_failed", failed.Error(), details)
		default:
			writeConfigurationError(w, http.StatusBadRequest, "upstream_probe_failed", failed.Error(), details)
		}
		return
	}
	models := result.Models
	if models == nil {
		models = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"models":   models,
		"endpoint": result.Endpoint,
	})
}

// upstreamProbeRequest assembles the probe from the request body, reading the
// stored credential when the body carries only its mask.
func (s *Server) upstreamProbeRequest(w http.ResponseWriter, r *http.Request, body map[string]any) (upstream.Request, bool) {
	encoded, err := json.Marshal(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_configuration", "the probe request must be a JSON object")
		return upstream.Request{}, false
	}
	var payload upstreamProbeRequest
	if err := json.Unmarshal(encoded, &payload); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_configuration", "the probe request must be a JSON object")
		return upstream.Request{}, false
	}

	request := upstream.Request{
		BaseURL: strings.TrimSpace(payload.URL),
		APIKey:  strings.TrimSpace(payload.Key),
		Headers: payload.Headers,
	}
	if !store.IsMaskedSecret(request.APIKey) {
		return request, true
	}
	// A masked key means the operator did not retype it: probe with the key the
	// gateway already holds. The value is used for this one outbound request and
	// is never part of a response.
	request.APIKey = ""
	if payload.ID <= 0 {
		return request, true
	}
	if s.ConfigStore == nil {
		writeError(w, http.StatusServiceUnavailable, "configuration_not_supported",
			"this gateway was started without configuration management")
		return upstream.Request{}, false
	}
	stored, err := s.ConfigStore.UpstreamKey(r.Context(), payload.ID)
	if err != nil {
		s.writeConfigurationFailure(w, r, err)
		return upstream.Request{}, false
	}
	request.APIKey = stored
	return request, true
}
