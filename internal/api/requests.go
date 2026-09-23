package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/yhw5231/fluxgate/internal/domain"
	"github.com/yhw5231/fluxgate/internal/router"
	"github.com/yhw5231/fluxgate/internal/store"
)

// The request log.
//
//	GET    /management/requests
//	DELETE /management/requests
//
// A proxied request is answered with an upstream status and, when it fails, a
// gateway error code. Neither says why: the upstream's own message is what names
// the cause, and it is the one thing the client never sees. The request log
// keeps it, together with the lines that were tried, so an operator can tell a
// wrong address from an exhausted key from a rate limit after the fact.
//
// Every request that gets past authentication leaves a record, including the
// ones the gateway itself refuses. A rejected model, an unreadable body, and an
// upstream that answered 429 are all failures an operator has to be able to look
// up, and the record is what says which of them happened.

// RequestLog is the record of served requests the console reads. It is satisfied
// by the SQLite store; a gateway assembled without one keeps no record.
type RequestLog interface {
	AppendRequestRecord(ctx context.Context, record domain.RequestRecord, keep int) error
	ListRequestRecords(ctx context.Context, filter store.RequestLogFilter) ([]domain.RequestRecord, error)
	CountRequestRecords(ctx context.Context, filter store.RequestLogFilter) (int, error)
	ClearRequestRecords(ctx context.Context) (int64, error)
}

// requestLogRecordLimit is one page of the console's request view, and the number
// of records a read returns when the caller does not say: enough to see what the
// gateway has just done, and short enough to read without scrolling far.
const requestLogRecordLimit = 20

// requestRecord is one stored record as the management API answers with it.
type requestRecord struct {
	ID           int64                 `json:"id"`
	RequestID    string                `json:"request_id"`
	At           string                `json:"at"`
	Method       string                `json:"method"`
	Path         string                `json:"path"`
	ClientIP     string                `json:"client_ip,omitempty"`
	KeyID        int64                 `json:"key_id"`
	KeyName      string                `json:"key_name"`
	Model        string                `json:"model"`
	Stream       bool                  `json:"stream"`
	Status       int                   `json:"status"`
	Failed       bool                  `json:"failed"`
	ErrorCode    string                `json:"error_code,omitempty"`
	ErrorMessage string                `json:"error_message,omitempty"`
	DurationMS   int64                 `json:"duration_ms"`
	Attempts     []domain.AttemptTrace `json:"attempts"`
}

// handleRequestLog answers the console's request view.
func (s *Server) handleRequestLog(w http.ResponseWriter, r *http.Request) {
	if !s.authenticateConsole(w, r) {
		return
	}
	if s.RequestLog == nil {
		writeError(w, http.StatusServiceUnavailable, "request_log_unavailable",
			"this gateway keeps no record of its requests")
		return
	}
	applied := s.currentPolicy().RequestLog
	filter := store.RequestLogFilter{
		Limit:      queryInt(r, "limit", requestLogRecordLimit),
		Offset:     queryInt(r, "offset", 0),
		FailedOnly: queryBool(r, "failed"),
		Model:      strings.TrimSpace(r.URL.Query().Get("model")),
		SinceID:    int64(queryInt(r, "since", 0)),
	}
	records, err := s.RequestLog.ListRequestRecords(r.Context(), filter)
	if err != nil {
		s.logger().Error("request_log_unreadable", "error", err.Error())
		writeError(w, http.StatusInternalServerError, "request_log_unreadable", "failed to read the request records")
		return
	}
	// The total is counted under the same filter, so the view can say how many
	// pages it is looking at without reading them.
	total, err := s.RequestLog.CountRequestRecords(r.Context(), filter)
	if err != nil {
		s.logger().Error("request_log_unreadable", "error", err.Error())
		writeError(w, http.StatusInternalServerError, "request_log_unreadable", "failed to read the request records")
		return
	}

	shaped := make([]requestRecord, 0, len(records))
	for _, record := range records {
		attempts := record.Attempts
		if attempts == nil {
			attempts = []domain.AttemptTrace{}
		}
		shaped = append(shaped, requestRecord{
			ID:           record.ID,
			RequestID:    record.RequestID,
			At:           record.At.UTC().Format(time.RFC3339Nano),
			Method:       record.Method,
			Path:         record.Path,
			ClientIP:     record.ClientIP,
			KeyID:        record.KeyID,
			KeyName:      record.KeyName,
			Model:        record.Model,
			Stream:       record.Stream,
			Status:       record.Status,
			Failed:       record.Failed(),
			ErrorCode:    record.ErrorCode,
			ErrorMessage: record.ErrorMessage,
			DurationMS:   record.DurationMS,
			Attempts:     attempts,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"requests": shaped,
		// The page window and the total travel with the records: the view says
		// which slice of the log it is showing, and how much there is in all.
		"total":  total,
		"limit":  filter.Limit,
		"offset": filter.Offset,
		// The retention travels with the records so the view can say how far back
		// it reaches instead of implying the log is everything ever served.
		"retention": map[string]any{
			"enabled": applied.Enabled,
			"keep":    applied.Keep,
		},
		"generated_at": time.Now().UTC().Format(time.RFC3339Nano),
	})
}

// handleRequestLogClear empties the log, which is how an operator clears the
// failures of a problem they have already fixed.
func (s *Server) handleRequestLogClear(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeConfigurationWrite(w, r) {
		return
	}
	if s.RequestLog == nil {
		writeError(w, http.StatusServiceUnavailable, "request_log_unavailable",
			"this gateway keeps no record of its requests")
		return
	}
	removed, err := s.RequestLog.ClearRequestRecords(r.Context())
	if err != nil {
		s.logger().Error("request_log_clear_failed", "error", err.Error())
		writeError(w, http.StatusInternalServerError, "request_log_unreadable", "failed to clear the request records")
		return
	}
	s.logConfigurationChange(r, "console_request_log_cleared", "requests", 0)
	writeJSON(w, http.StatusOK, map[string]any{"cleared": removed})
}

// recordProxiedRequest stores one record. A gateway that keeps no log, or has it
// switched off, drops the record; a log that cannot be written is reported in the
// process log and never changes what the client was told.
func (s *Server) recordProxiedRequest(r *http.Request, record domain.RequestRecord) {
	if s.RequestLog == nil {
		return
	}
	applied := s.currentPolicy().RequestLog
	if !applied.Enabled {
		return
	}
	// The record is written after the response, so the request's own context is
	// already done with; a cancelled one would drop the record of the request
	// that was just cut short, which is often the one worth having.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
	defer cancel()
	if err := s.RequestLog.AppendRequestRecord(ctx, record, applied.Keep); err != nil {
		s.logger().Warn("request_log_write_failed", "error", err.Error())
	}
}

// newRequestID returns the identity a record is filed under and the client is
// told. It is short enough to quote in a bug report and random enough that two
// requests never share one.
func newRequestID() string {
	var buffer [8]byte
	if _, err := rand.Read(buffer[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(buffer[:])
}

// dispatchFailure describes a request the engine could not serve: the error code
// the client is answered with, the status, and the details that say which line
// failed and what it said.
type dispatchFailure struct {
	Status  int
	Code    string
	Message string
	Details map[string]any
}

// classifyDispatchError turns a dispatch error into the answer the client gets.
// A model with no usable route or line is an availability problem, not a bad
// gateway: no upstream request was attempted.
func classifyDispatchError(err error, trace domain.RequestTrace, requestID string) dispatchFailure {
	if errors.Is(err, router.ErrNoChannel) || errors.Is(err, router.ErrModelNotRoutable) {
		return dispatchFailure{
			Status:  http.StatusServiceUnavailable,
			Code:    "no_available_channel",
			Message: "no upstream channel can serve the requested model",
			Details: map[string]any{"request_id": requestID},
		}
	}
	message := err.Error()
	details := map[string]any{"request_id": requestID, "attempts": len(trace.Attempts)}
	if last, ok := trace.LastFailure(); ok {
		// The upstream's own answer is the part of a failure that names the cause,
		// so it is handed to the client rather than left in the console. The
		// gateway is the operator's own proxy and the caller already holds a key
		// for it; an opaque 502 only costs them a round trip through the logs.
		if last.Response != "" {
			message += ": " + last.Response
		}
		if last.Error != "" {
			message += ": " + last.Error
		}
		if last.StatusCode != 0 {
			details["upstream_status"] = last.StatusCode
		}
		if last.Response != "" {
			details["upstream_message"] = last.Response
		}
	}
	return dispatchFailure{
		Status:  http.StatusBadGateway,
		Code:    "upstream_unavailable",
		Message: message,
		Details: details,
	}
}

func queryInt(r *http.Request, name string, fallback int) int {
	value := strings.TrimSpace(r.URL.Query().Get(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func queryBool(r *http.Request, name string) bool {
	switch strings.ToLower(strings.TrimSpace(r.URL.Query().Get(name))) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}
