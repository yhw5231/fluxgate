package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/yhw5231/fluxgate/internal/probe"
)

// Finding out what an upstream model is really served from.
//
//	POST /management/pins/probe     {"site_id": 3, "model": "gpt-4.1"}
//	POST /management/pins/validate  {"site_id": 3, "model": "gpt-4.1", "providers": [...]}
//
// A pin the operator writes by hand names providers the upstream has never been
// asked about. The probe asks: it reads the provider that served a small request,
// asks the upstream to name the providers it could have used, and writes the
// pipeline and provider list back onto the pin. The validation asks each named
// provider, one at a time and exclusively, whether it answers right now.
//
// Both spend a little of the account's quota, which is why they are actions an
// operator takes rather than something that runs on a schedule.

// PinProbeStore records what a pin probe found.
type PinProbeStore interface {
	SavePinProbe(ctx context.Context, siteID int64, model, pipeline string, providers []string, lastProvider string, at time.Time) error
}

type pinProbeRequest struct {
	SiteID int64  `json:"site_id"`
	Model  string `json:"model"`
}

type pinValidateRequest struct {
	SiteID    int64    `json:"site_id"`
	Model     string   `json:"model"`
	Providers []string `json:"providers"`
}

// decodePinRequest reads and validates the body the two endpoints share.
func (s *Server) decodePinRequest(w http.ResponseWriter, r *http.Request, target any) (int64, string, bool) {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(target); err != nil && err != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid_pin_probe", "the request must be a JSON object")
		return 0, "", false
	}
	var siteID int64
	var model string
	switch value := target.(type) {
	case *pinProbeRequest:
		siteID, model = value.SiteID, value.Model
	case *pinValidateRequest:
		siteID, model = value.SiteID, value.Model
	}
	model = strings.TrimSpace(model)
	if siteID <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_pin_probe", "site_id must be a row id")
		return 0, "", false
	}
	if model == "" {
		writeError(w, http.StatusBadRequest, "invalid_pin_probe", "model is required")
		return 0, "", false
	}
	if len(model) > 512 {
		writeError(w, http.StatusBadRequest, "invalid_pin_probe", "model is too long")
		return 0, "", false
	}
	return siteID, model, true
}

// handlePinProbe discovers the internal providers of one upstream model.
func (s *Server) handlePinProbe(w http.ResponseWriter, r *http.Request) {
	if !s.authenticateConsole(w, r) {
		return
	}
	if !sameOriginRequest(r) {
		writeError(w, http.StatusForbidden, "cross_origin_rejected",
			"a pin probe must come from the console's own origin")
		return
	}
	if s.Probes == nil {
		writeError(w, http.StatusServiceUnavailable, "probe_log_unavailable",
			"this gateway runs no probe schedule")
		return
	}
	var request pinProbeRequest
	siteID, model, ok := s.decodePinRequest(w, r, &request)
	if !ok {
		return
	}
	line, ok := s.pinLine(siteID, model)
	if !ok {
		writeError(w, http.StatusNotFound, "unknown_channel",
			"no enabled line serves a model on that upstream")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	discovery := s.Probes.DiscoverPin(ctx, line)
	if !discovery.OK {
		details := map[string]any{"detail": discovery.Detail, "model": model}
		if discovery.Status != 0 {
			details["status"] = discovery.Status
		}
		writeConfigurationError(w, http.StatusBadGateway, "pin_probe_failed",
			"the upstream did not answer the probe", details)
		return
	}

	saved := false
	if s.PinStore != nil {
		if err := s.PinStore.SavePinProbe(r.Context(), siteID, model, discovery.Pipeline,
			discovery.Providers, discovery.LastProvider, time.Now()); err != nil {
			s.logger().Warn("pin_probe_save_failed", "error", err.Error())
		} else {
			saved = true
		}
	}
	// The reload republishes the configuration so the gateway picks up the pipeline
	// the probe just wrote, without waiting for the next console write.
	if saved {
		if err := s.reloadConfiguration(r.Context()); err != nil {
			s.logger().Warn("pin_probe_reload_failed", "error", err.Error())
		}
	}
	s.logConfigurationChange(r, "console_pin_probed", "pins", siteID)
	writeJSON(w, http.StatusOK, map[string]any{
		"pipeline":      discovery.Pipeline,
		"providers":     discovery.Providers,
		"last_provider": discovery.LastProvider,
		"saved":         saved,
	})
}

// handlePinValidate asks each named provider whether it answers.
func (s *Server) handlePinValidate(w http.ResponseWriter, r *http.Request) {
	if !s.authenticateConsole(w, r) {
		return
	}
	if !sameOriginRequest(r) {
		writeError(w, http.StatusForbidden, "cross_origin_rejected",
			"a pin validation must come from the console's own origin")
		return
	}
	if s.Probes == nil {
		writeError(w, http.StatusServiceUnavailable, "probe_log_unavailable",
			"this gateway runs no probe schedule")
		return
	}
	var request pinValidateRequest
	siteID, model, ok := s.decodePinRequest(w, r, &request)
	if !ok {
		return
	}
	providers := make([]string, 0, len(request.Providers))
	for _, provider := range request.Providers {
		if trimmed := strings.TrimSpace(provider); trimmed != "" {
			providers = append(providers, trimmed)
		}
	}
	if len(providers) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_pin_probe", "at least one provider is required")
		return
	}
	if len(providers) > 25 {
		writeError(w, http.StatusBadRequest, "invalid_pin_probe", "at most 25 providers can be validated at once")
		return
	}
	line, ok := s.pinLine(siteID, model)
	if !ok {
		writeError(w, http.StatusNotFound, "unknown_channel",
			"no enabled line serves a model on that upstream")
		return
	}
	pipeline := s.pinPipeline(siteID, model)

	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()
	results := s.Probes.ValidatePins(ctx, line, providers, pipeline)
	if results == nil {
		results = []probe.PinValidation{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

// pinLine picks the line a pin probe leaves through: the first enabled line of
// that upstream, presenting the model the pin names. A probe does not need the
// route to serve that model — the operator is asking the upstream directly.
func (s *Server) pinLine(siteID int64, model string) (probe.Line, bool) {
	for _, route := range s.currentRoutes() {
		for _, channel := range route.Channels {
			if channel.SiteID != siteID || !channel.Enabled {
				continue
			}
			if strings.TrimSpace(channel.APIKey) == "" {
				continue
			}
			return probe.Line{
				ChannelID:      channel.ID,
				ChannelName:    channel.Name,
				BaseURL:        channel.BaseURL,
				APIKey:         channel.APIKey,
				Model:          model,
				BreakerMode:    channel.BreakerMode,
				ClientProfile:  channel.ClientProfile,
				Transform:      channel.Transform,
				ProxyURL:       channel.ProxyURL,
				UseSystemProxy: channel.UseSystemProxy,
			}, true
		}
	}
	return probe.Line{}, false
}

// pinPipeline reports the pipeline recorded for one pin, or an empty string when
// nothing has been discovered yet.
func (s *Server) pinPipeline(siteID int64, model string) string {
	if s.ConfigStore == nil {
		return ""
	}
	rows, err := s.ConfigStore.ListResource(context.Background(), "pins")
	if err != nil {
		return ""
	}
	for _, row := range rows {
		rowSite, _ := row["site_id"].(int64)
		rowModel, _ := row["model"].(string)
		if rowSite != siteID || !strings.EqualFold(strings.TrimSpace(rowModel), strings.TrimSpace(model)) {
			continue
		}
		if pipeline, ok := row["pipeline"].(string); ok {
			return pipeline
		}
	}
	return ""
}
