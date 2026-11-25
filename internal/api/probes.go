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

// Checking that an upstream credential still works.
//
//	GET    /management/probes?limit=100
//	POST   /management/probes/run            {"channel_id": "12"}
//	DELETE /management/probes
//
// A key can stop working between two requests: a quota expires, an account is
// suspended, a subscription lapses. The gateway's own probe schedule finds that
// out on its own (after a restart, after a line has been idle, after a cooldown
// expires), and this family of endpoints is both how the console reads what it
// found and how an operator asks for a check right now.
//
// A probe never changes a circuit: it is evidence, not a decision. Bringing a
// line back is 恢复 or its cooldown expiring.

// ProbeService runs probes and reads their record. It is satisfied by the probe
// service the process starts; a gateway assembled without one answers 503.
type ProbeService interface {
	// ProbeAll probes every eligible line, synchronously, and answers what it
	// found.
	ProbeAll(ctx context.Context) []probe.Result
	// ProbeAllAsync starts the same sweep in the background and reports whether it
	// started.
	ProbeAllAsync(ctx context.Context) bool
	// ProbeChannel probes every line of one upstream.
	ProbeChannel(ctx context.Context, channelID string) []probe.Result
	// ListRuns returns the newest results first, with the total the log holds.
	ListRuns(ctx context.Context, limit int) ([]probe.Result, int64, error)
	// ClearRuns empties the log.
	ClearRuns(ctx context.Context) (int64, error)
}

// probeRunsLimit is how many results the console's view reads by default.
const probeRunsLimit = 100

// handleProbes answers what the probes found, and how they are configured.
func (s *Server) handleProbes(w http.ResponseWriter, r *http.Request) {
	if !s.authenticateConsole(w, r) {
		return
	}
	if s.Probes == nil {
		writeError(w, http.StatusServiceUnavailable, "probe_log_unavailable",
			"this gateway runs no probe schedule")
		return
	}
	limit := queryInt(r, "limit", probeRunsLimit)
	runs, total, err := s.Probes.ListRuns(r.Context(), limit)
	if err != nil {
		s.logger().Error("probe_log_read_failed", "error", err.Error())
		writeError(w, http.StatusInternalServerError, "probe_log_unreadable", "failed to read the probe records")
		return
	}
	if runs == nil {
		runs = []probe.Result{}
	}
	applied := s.currentPolicy().Probe
	writeJSON(w, http.StatusOK, map[string]any{
		"probes": runs,
		"total":  total,
		"retention": map[string]any{
			"keep":  applied.Keep,
			"count": total,
		},
		"policy": map[string]any{
			"startup":      applied.Startup,
			"idle_seconds": applied.IdleSeconds,
			"concurrency":  applied.Concurrency,
		},
	})
}

// handleProbeRun checks one upstream now, or starts a sweep of all of them.
func (s *Server) handleProbeRun(w http.ResponseWriter, r *http.Request) {
	if !s.authenticateConsole(w, r) {
		return
	}
	if !sameOriginRequest(r) {
		writeError(w, http.StatusForbidden, "cross_origin_rejected",
			"a probe must come from the console's own origin")
		return
	}
	if s.Probes == nil {
		writeError(w, http.StatusServiceUnavailable, "probe_log_unavailable",
			"this gateway runs no probe schedule")
		return
	}
	var body struct {
		ChannelID string `json:"channel_id"`
	}
	if raw := strings.TrimSpace(r.Header.Get("Content-Type")); strings.HasPrefix(raw, "application/json") {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil && err != io.EOF {
			writeError(w, http.StatusBadRequest, "invalid_probe", "the probe request must be a JSON object")
			return
		}
	}

	if channelID := strings.TrimSpace(body.ChannelID); channelID != "" {
		// One upstream is checked while the operator waits: it is one small request,
		// and its answer is what they clicked for.
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
		defer cancel()
		results := s.Probes.ProbeChannel(ctx, channelID)
		if len(results) == 0 {
			writeError(w, http.StatusNotFound, "unknown_channel",
				"no enabled line matches that channel id")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"probes": results})
		return
	}

	// Everything is a sweep: it can take long enough that holding the request
	// open would look like a hang, so it runs in the background and the console
	// reads the results as they arrive.
	if !s.Probes.ProbeAllAsync(context.WithoutCancel(r.Context())) {
		writeError(w, http.StatusConflict, "probe_in_progress",
			"a probe sweep is already running")
		return
	}
	s.logConfigurationChange(r, "console_probe_sweep_started", "probes", 0)
	writeJSON(w, http.StatusAccepted, map[string]any{"started": true})
}

// handleProbeClear empties the probe record.
func (s *Server) handleProbeClear(w http.ResponseWriter, r *http.Request) {
	if !s.authenticateConsole(w, r) {
		return
	}
	if !sameOriginRequest(r) {
		writeError(w, http.StatusForbidden, "cross_origin_rejected",
			"clearing the probe record must come from the console's own origin")
		return
	}
	if s.Probes == nil {
		writeError(w, http.StatusServiceUnavailable, "probe_log_unavailable",
			"this gateway runs no probe schedule")
		return
	}
	removed, err := s.Probes.ClearRuns(r.Context())
	if err != nil {
		s.logger().Error("probe_clear_failed", "error", err.Error())
		writeError(w, http.StatusInternalServerError, "probe_log_unreadable", "failed to clear the probe records")
		return
	}
	s.logConfigurationChange(r, "console_probe_log_cleared", "probes", 0)
	writeJSON(w, http.StatusOK, map[string]any{"cleared": removed})
}
