package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/yhw5231/fluxgate/internal/domain"
	"github.com/yhw5231/fluxgate/internal/probe"
)

// Checking that an upstream credential still works.
//
//	GET    /management/probes?limit=100
//	POST   /management/probes/run            {"channel_id": "12"}
//	POST   /management/probes/run            {"site_id": 3}
//	POST   /management/probes/run            {}
//	DELETE /management/probes
//
// A key can stop working between two requests: a quota expires, an account is
// suspended, a subscription lapses. The gateway's own probe schedule finds that
// out on its own (after a restart, after a line has been idle, after a cooldown
// expires), and this family of endpoints is both how the console reads what it
// found and how an operator asks for a check right now.
//
// A probe never changes a circuit: it is evidence, not a decision. Bringing a
// line back is 鎭㈠ or its cooldown expiring.

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
	// ProbeSite probes every probeable line of one upstream, which is what the
	// 涓婃父 view's 妫€娴?action asks for: one check per key of the upstream the
	// operator named, rather than one per line of the whole gateway.
	ProbeSite(ctx context.Context, siteID int64) []probe.Result
	// Targets reports the lines that could be probed now, with the newest result
	// recorded for each.
	Targets(ctx context.Context) []probe.Target
	// ListRuns returns the newest results first, with the total the log holds.
	ListRuns(ctx context.Context, limit int) ([]probe.Result, int64, error)
	// ClearRuns empties the log.
	ClearRuns(ctx context.Context) (int64, error)
	// DiscoverPin asks an upstream model which internal providers it has and which
	// pipeline it speaks.
	DiscoverPin(ctx context.Context, line probe.Line) probe.PinDiscovery
	// ValidatePins asks each named provider, exclusively, whether it answers.
	ValidatePins(ctx context.Context, line probe.Line, providers []string, pipeline string) []probe.PinValidation
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
	// The targets are what the schedule could ask right now, each with the newest
	// result recorded for it: the view says "these are the lines and models a probe
	// can check, and this is how each last answered" instead of only listing checks
	// that already happened.
	targets := s.Probes.Targets(r.Context())
	if targets == nil {
		targets = []probe.Target{}
	}
	applied := s.currentPolicy().Probe
	writeJSON(w, http.StatusOK, map[string]any{
		"probes":  runs,
		"targets": targets,
		"total":   total,
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
		ChannelID string          `json:"channel_id"`
		SiteID    json.RawMessage `json:"site_id"`
	}
	if raw := strings.TrimSpace(r.Header.Get("Content-Type")); strings.HasPrefix(raw, "application/json") {
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
		if err := decoder.Decode(&body); err != nil && err != io.EOF {
			writeError(w, http.StatusBadRequest, "invalid_probe", "the probe request must be a JSON object")
			return
		}
		var extra json.RawMessage
		if decoder.Decode(&extra) != io.EOF {
			writeError(w, http.StatusBadRequest, "invalid_probe", "the probe request must contain one JSON object")
			return
		}
	}

	if body.SiteID != nil {
		siteID, ok := probeSiteID(w, body.SiteID, body.ChannelID)
		if !ok {
			return
		}
		// One upstream is checked while the operator waits: it is one small request
		// per key, and its answer is what they clicked for. The upstream is named
		// rather than its lines, so the console does not have to know which channel
		// ids belong to the row it is looking at.
		ctx, cancel := context.WithTimeout(r.Context(), manualProbeTimeout)
		defer cancel()
		results := s.Probes.ProbeSite(ctx, siteID)
		if ctx.Err() != nil {
			writeError(w, http.StatusGatewayTimeout, "probe_timeout",
				"the upstream probe did not finish before its deadline")
			return
		}
		if len(results) == 0 {
			// The upstream holds no line a probe can ask: every key it has is
			// disabled, its routes are off, or no model name its upstream knows can
			// be sent. That is a different condition from a failed check, and the
			// console says so rather than reporting a healthy answer.
			writeError(w, http.StatusNotFound, "unknown_site",
				"no enabled line of that upstream can be probed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"probes": results})
		return
	}

	if channelID := strings.TrimSpace(body.ChannelID); channelID != "" {
		// One line is checked while the operator waits: it is one small request,
		// and its answer is what they clicked for.
		ctx, cancel := context.WithTimeout(r.Context(), manualProbeTimeout)
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

// manualProbeTimeout bounds a check an operator is waiting for. It is one small
// request per key of one upstream, so it is generous enough for an upstream with
// many accounts and short enough that a hung upstream answers instead of hanging
// the click.
const manualProbeTimeout = 2 * time.Minute

// probeSiteID reads the site_id of a probe request. The value is decoded from raw
// JSON because a missing key must start a sweep while a key that is present and
// wrong must be refused: a zero, a fraction, a string, or a boolean is a mistake
// the console has to hear about, not something to guess around.
func probeSiteID(w http.ResponseWriter, raw json.RawMessage, channelID string) (int64, bool) {
	var siteID int64
	if err := json.Unmarshal(raw, &siteID); err != nil || siteID <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_probe",
			"site_id must be a positive integer")
		return 0, false
	}
	if strings.TrimSpace(channelID) != "" {
		writeError(w, http.StatusBadRequest, "invalid_probe",
			"a probe names one line, one upstream, or everything, not two of them")
		return 0, false
	}
	return siteID, true
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

// RecordProbeRun stores one finished probe in the request log. A probe is a request
// the gateway made on its own behalf, and 璇锋眰璁板綍 is where an operator looks for
// what the gateway did and when; leaving probes out of it would keep the checks
// nobody asked for out of the one place that explains failures.
func (s *Server) RecordProbeRun(result probe.Result) {
	if s == nil || s.RequestLog == nil {
		return
	}
	applied := s.currentPolicy().RequestLog
	if !applied.Enabled {
		return
	}
	attempt := domain.AttemptTrace{
		Number:      1,
		ChannelID:   result.ChannelID,
		ChannelName: result.ChannelName,
		Model:       result.Model,
		StatusCode:  result.Status,
		StartedAt:   result.At,
		DurationMS:  result.LatencyMS,
	}
	if result.Status == 0 {
		// The upstream never answered, so there is no body to quote: the transport
		// error is the whole explanation.
		attempt.Error = result.Detail
	} else {
		attempt.Response = result.Detail
	}
	record := domain.RequestRecord{
		RequestID:  newRequestID(),
		At:         result.At,
		Method:     "PROBE",
		Path:       "/management/probes/" + string(result.Kind),
		Model:      result.Model,
		Status:     result.Status,
		DurationMS: result.LatencyMS,
		Attempts:   []domain.AttemptTrace{attempt},
		Source:     domain.RequestSourceProbe,
	}
	if !result.OK {
		record.ErrorCode = result.Reason
		record.ErrorMessage = result.Detail
	}
	// A probe outlives the request that may have triggered it 鈥?a sweep runs in the
	// background 鈥?so the write is not tied to a caller's context.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.RequestLog.AppendRequestRecord(ctx, record, applied.Keep); err != nil {
		s.logger().Warn("probe_request_log_write_failed", "error", err.Error())
	}
}
