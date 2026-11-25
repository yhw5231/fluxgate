package api

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/yhw5231/fluxgate/internal/domain"
	"github.com/yhw5231/fluxgate/internal/usage"
)

// What the gateway was paid in tokens.
//
//	GET /management/usage?window=today|24h|7d|30d|all
//
// A request record says what happened to a request; it does not say what the
// request cost. The upstream states that in its answer — prompt and completion
// tokens — and this endpoint sums those statements over a window, by model, by
// upstream, and by client key.
//
// The totals are only as good as what the upstream reported: a provider that
// answers without a usage object leaves a request counted but uncosted, and the
// answer says how many of the requests were measured so a view cannot read the
// difference as zero.

// UsageLog keeps the token accounting the console reads and the gateway writes.
// It is satisfied by the SQLite store; a gateway assembled without one keeps no
// usage record and this endpoint answers 503.
type UsageLog interface {
	// AppendUsageEvent stores one record, keeping the log bounded to the newest
	// keep records.
	AppendUsageEvent(ctx context.Context, event usage.Event, keep int) error
	// QueryUsage sums the records a filter matches, given the retention the log
	// actually has.
	QueryUsage(ctx context.Context, filter usage.Filter, retention usage.Retention) (usage.Summary, error)
	// UsageRetention reports how much the log holds and how far back it reaches.
	UsageRetention(ctx context.Context, keep int, enabled bool) (usage.Retention, error)
	// ClearUsageEvents empties the log, which is how an operator starts a fresh
	// measurement after a change.
	ClearUsageEvents(ctx context.Context) (int64, error)
}

// usageBreakdownLimit is how many groups each breakdown lists. The totals are
// always complete; the groups are a reading of where the cost went, and a dozen
// names is more than a view can compare at a glance.
const usageBreakdownLimit = 12

// handleUsage answers one usage question.
func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	if !s.authenticateConsole(w, r) {
		return
	}
	if s.Usage == nil {
		writeError(w, http.StatusServiceUnavailable, "usage_log_unavailable",
			"this gateway keeps no usage record")
		return
	}
	applied := s.currentPolicy().Usage
	now := time.Now()
	window, since, until := usage.ResolveWindow(r.URL.Query().Get("window"), now)

	filter := usage.Filter{
		Since:   since,
		Until:   until,
		Model:   strings.TrimSpace(r.URL.Query().Get("model")),
		Channel: strings.TrimSpace(r.URL.Query().Get("channel")),
		Limit:   usageBreakdownLimit,
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("key_id")); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id < 0 {
			writeError(w, http.StatusBadRequest, "invalid_filter", "key_id must be a row id")
			return
		}
		filter.KeyID = id
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	retention, err := s.Usage.UsageRetention(ctx, applied.Keep, applied.Enabled)
	if err != nil {
		s.logger().Error("usage_retention_read_failed", "error", err.Error())
		writeError(w, http.StatusInternalServerError, "usage_log_unreadable", "failed to read the usage log")
		return
	}
	summary, err := s.Usage.QueryUsage(ctx, filter, retention)
	if err != nil {
		s.logger().Error("usage_query_failed", "error", err.Error())
		writeError(w, http.StatusInternalServerError, "usage_log_unreadable", "failed to read the usage log")
		return
	}
	summary.Window = window
	summary.Since = since
	summary.Until = until
	if summary.Since.IsZero() {
		// The "all" window has no lower bound of its own; the answer reports how
		// far back the log actually reaches.
		summary.Since = retention.Oldest
	}
	writeJSON(w, http.StatusOK, summary)
}

// handleUsageClear empties the usage log. It removes the accounting only: the
// request records that explain what happened, and the circuits that decide where
// requests go, are untouched.
func (s *Server) handleUsageClear(w http.ResponseWriter, r *http.Request) {
	if !s.authenticateConsole(w, r) {
		return
	}
	if !sameOriginRequest(r) {
		writeError(w, http.StatusForbidden, "cross_origin_rejected",
			"clearing the usage log must come from the console's own origin")
		return
	}
	if s.Usage == nil {
		writeError(w, http.StatusServiceUnavailable, "usage_log_unavailable",
			"this gateway keeps no usage record")
		return
	}
	removed, err := s.Usage.ClearUsageEvents(r.Context())
	if err != nil {
		s.logger().Error("usage_clear_failed", "error", err.Error())
		writeError(w, http.StatusInternalServerError, "usage_log_unreadable", "failed to clear the usage log")
		return
	}
	s.logConfigurationChange(r, "console_usage_log_cleared", "usage", 0)
	writeJSON(w, http.StatusOK, map[string]any{"cleared": removed})
}

// recordUsage stores what the answer of one served request cost.
//
// It is written after the answer has been relayed: the caller's context is done
// by then, so a cancelled request would drop the record of the request that was
// just cut short — often the one worth accounting for.
func (s *Server) recordUsage(r *http.Request, record domain.RequestRecord, tap *usage.Tap) {
	if s.Usage == nil || tap == nil {
		return
	}
	applied := s.currentPolicy().Usage
	if !applied.Enabled {
		return
	}
	prompt, completion, reported := tap.Usage()
	event := usage.Event{
		RequestID:        record.RequestID,
		At:               record.At,
		Path:             record.Path,
		Model:            record.Model,
		ClientIP:         record.ClientIP,
		KeyID:            record.KeyID,
		KeyName:          record.KeyName,
		Status:           record.Status,
		Stream:           record.Stream,
		DurationMS:       record.DurationMS,
		PromptTokens:     prompt,
		CompletionTokens: completion,
		Reported:         reported,
	}
	// The line that answered is named by the attempt trace: the gateway is the
	// only party that knows which upstream produced the answer, and the usage log
	// names it by its label rather than by its credential.
	for index := len(record.Attempts) - 1; index >= 0; index-- {
		attempt := record.Attempts[index]
		if attempt.ChannelID == "" {
			continue
		}
		event.ChannelID = attempt.ChannelID
		event.ChannelName = attempt.ChannelName
		break
	}
	if event.ChannelID == "" {
		// A record always carries the attempts that led to it; the fallback keeps
		// the log complete if a caller built one by hand.
		event.ChannelName = "unknown"
	}

	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
	defer cancel()
	if err := s.Usage.AppendUsageEvent(ctx, event, applied.Keep); err != nil {
		s.logger().Warn("usage_write_failed", "error", err.Error())
	}
}
