package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/yhw5231/fluxgate/internal/usage"
)

// The usage log.
//
// The gateway counts what every request cost: the prompt it sent and the
// completion it received, as the upstream itself reported them. The counts are
// the operator's bill, and they are only visible at the moment the answer passes
// through the gateway, so they are kept here rather than derived later from the
// request log — that log is bounded tightly and deliberately carries no numbers.
//
// The table is the gateway's own, like gateway_request_log and
// gateway_breaker_states, and is created on demand. Like the request log it is
// bounded: the newest N records are kept and the rest are dropped as new ones
// are written, so a long-running gateway cannot grow the shared database without
// limit.

// gatewayUsageDDL is the schema of the gateway-owned usage log. The time is
// stored as an RFC 3339 timestamp in UTC, which orders lexicographically, so a
// range comparison on it is also a range comparison on the moment.
const gatewayUsageDDL = `CREATE TABLE IF NOT EXISTS gateway_usage_events (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	request_id TEXT NOT NULL DEFAULT '',
	at TEXT NOT NULL,
	path TEXT NOT NULL DEFAULT '',
	model TEXT NOT NULL DEFAULT '',
	client_ip TEXT NOT NULL DEFAULT '',
	key_id INTEGER NOT NULL DEFAULT 0,
	key_name TEXT NOT NULL DEFAULT '',
	channel_id TEXT NOT NULL DEFAULT '',
	channel_name TEXT NOT NULL DEFAULT '',
	status INTEGER NOT NULL DEFAULT 0,
	stream INTEGER NOT NULL DEFAULT 0,
	duration_ms INTEGER NOT NULL DEFAULT 0,
	prompt_tokens INTEGER NOT NULL DEFAULT 0,
	completion_tokens INTEGER NOT NULL DEFAULT 0,
	reported INTEGER NOT NULL DEFAULT 0
)`

// EnsureUsageSchema creates the usage log and its indexes on a database that does
// not have them yet.
func (s *SQLiteStore) EnsureUsageSchema(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, gatewayUsageDDL); err != nil {
		return fmt.Errorf("create gateway_usage_events: %w", err)
	}
	for _, statement := range []string{
		`CREATE INDEX IF NOT EXISTS gateway_usage_events_at_idx ON gateway_usage_events(at)`,
		`CREATE INDEX IF NOT EXISTS gateway_usage_events_model_idx ON gateway_usage_events(model)`,
		`CREATE INDEX IF NOT EXISTS gateway_usage_events_channel_idx ON gateway_usage_events(channel_id, channel_name)`,
		`CREATE INDEX IF NOT EXISTS gateway_usage_events_key_idx ON gateway_usage_events(key_id)`,
	} {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("index gateway_usage_events: %w", err)
		}
	}
	return nil
}

// AppendUsageEvent stores one usage record and keeps the log bounded to the
// newest keep records. A keep of zero or less stores the record without pruning.
func (s *SQLiteStore) AppendUsageEvent(ctx context.Context, event usage.Event, keep int) error {
	at := event.At
	if at.IsZero() {
		at = time.Now()
	}
	total := event.PromptTokens + event.CompletionTokens
	reported := event.Reported || total > 0
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.db.ExecContext(ctx, `INSERT INTO gateway_usage_events (
		request_id, at, path, model, client_ip, key_id, key_name, channel_id, channel_name,
		status, stream, duration_ms, prompt_tokens, completion_tokens, reported
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		strings.TrimSpace(event.RequestID),
		at.UTC().Format(time.RFC3339Nano),
		event.Path,
		event.Model,
		event.ClientIP,
		event.KeyID,
		event.KeyName,
		event.ChannelID,
		event.ChannelName,
		event.Status,
		boolInt(event.Stream),
		event.DurationMS,
		event.PromptTokens,
		event.CompletionTokens,
		boolInt(reported),
	); err != nil {
		return fmt.Errorf("store usage event: %w", err)
	}
	if keep <= 0 {
		return nil
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM gateway_usage_events WHERE id IN (
		SELECT id FROM gateway_usage_events ORDER BY id DESC LIMIT -1 OFFSET ?)`, keep); err != nil {
		return fmt.Errorf("prune usage events: %w", err)
	}
	return nil
}

// QueryUsage answers one usage question: the totals over a window, the same
// numbers grouped by model, upstream, and client key, and how far back the log
// reaches.
func (s *SQLiteStore) QueryUsage(ctx context.Context, filter usage.Filter, retention usage.Retention) (usage.Summary, error) {
	where, arguments := usageWhere(filter)
	summary := usage.Summary{
		Window:    "",
		Since:     filter.Since,
		Until:     filter.Until,
		Models:    []usage.Breakdown{},
		Channels:  []usage.Breakdown{},
		Keys:      []usage.Breakdown{},
		Retention: retention,
	}
	if summary.Until.IsZero() {
		summary.Until = time.Now().UTC()
	}
	if summary.Since.IsZero() {
		summary.Since = summary.Retention.Oldest
	}

	totalsQuery := `SELECT COUNT(*),
		COALESCE(SUM(CASE WHEN status >= 200 AND status < 300 THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN status >= 400 OR status = 0 THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(prompt_tokens), 0),
		COALESCE(SUM(completion_tokens), 0),
		COALESCE(SUM(reported), 0)
	FROM gateway_usage_events` + where
	if err := s.db.QueryRowContext(ctx, totalsQuery, arguments...).Scan(
		&summary.Requests,
		&summary.Succeeded,
		&summary.Failed,
		&summary.PromptTokens,
		&summary.CompletionTokens,
		&summary.Reported,
	); err != nil {
		return usage.Summary{}, fmt.Errorf("sum usage events: %w", err)
	}
	summary.TotalTokens = summary.PromptTokens + summary.CompletionTokens

	limit := filter.Limit
	if limit <= 0 {
		limit = 12
	}
	groups := []struct {
		column string
		label  string
		target *[]usage.Breakdown
	}{
		{"model", "model", &summary.Models},
		{"channel_name", "channel", &summary.Channels},
		{"key_name", "key", &summary.Keys},
	}
	for _, group := range groups {
		breakdown, err := s.usageBreakdown(ctx, group.column, group.label, where, arguments, limit)
		if err != nil {
			return usage.Summary{}, err
		}
		*group.target = breakdown
	}
	return summary, nil
}

// usageBreakdown groups the records the filter matches by one column. An empty
// value is grouped under "unknown" rather than dropped, so the groups always add
// up to the totals.
func (s *SQLiteStore) usageBreakdown(ctx context.Context, column, label string, where string, arguments []any, limit int) ([]usage.Breakdown, error) {
	expression := "COALESCE(NULLIF(" + column + ", ''), 'unknown')"
	query := `SELECT ` + expression + ` AS name,
		COUNT(*),
		COALESCE(SUM(CASE WHEN status >= 200 AND status < 300 THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN status >= 400 OR status = 0 THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(prompt_tokens), 0),
		COALESCE(SUM(completion_tokens), 0),
		COALESCE(SUM(reported), 0)
	FROM gateway_usage_events` + where + `
	GROUP BY name
	ORDER BY SUM(prompt_tokens + completion_tokens) DESC, COUNT(*) DESC, name
	LIMIT ?`
	rows, err := s.db.QueryContext(ctx, query, append(append([]any{}, arguments...), limit)...)
	if err != nil {
		return nil, fmt.Errorf("sum usage by %s: %w", label, err)
	}
	defer rows.Close()
	breakdown := make([]usage.Breakdown, 0)
	for rows.Next() {
		var entry usage.Breakdown
		if err := rows.Scan(
			&entry.Name,
			&entry.Requests,
			&entry.Succeeded,
			&entry.Failed,
			&entry.PromptTokens,
			&entry.CompletionTokens,
			&entry.Reported,
		); err != nil {
			return nil, fmt.Errorf("scan usage by %s: %w", label, err)
		}
		entry.TotalTokens = entry.PromptTokens + entry.CompletionTokens
		breakdown = append(breakdown, entry)
	}
	return breakdown, rows.Err()
}

// UsageRetention reports how much the log holds and how far back it reaches. A
// gateway that keeps no usage records answers with Enabled false and a zero
// count, so the view can say the numbers are not being kept instead of showing
// an empty chart that looks like an idle gateway.
func (s *SQLiteStore) UsageRetention(ctx context.Context, keep int, enabled bool) (usage.Retention, error) {
	retention := usage.Retention{Enabled: enabled, Keep: int64(keep)}
	if !enabled {
		return retention, nil
	}
	var oldest, newest sql.NullString
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*), MIN(at), MAX(at) FROM gateway_usage_events`).Scan(
		&retention.Count, &oldest, &newest,
	); err != nil {
		return usage.Retention{}, fmt.Errorf("read usage retention: %w", err)
	}
	if oldest.Valid {
		if parsed, err := time.Parse(time.RFC3339Nano, oldest.String); err == nil {
			retention.Oldest = parsed.UTC()
		}
	}
	if newest.Valid {
		if parsed, err := time.Parse(time.RFC3339Nano, newest.String); err == nil {
			retention.Newest = parsed.UTC()
		}
	}
	return retention, nil
}

// ClearUsageEvents empties the usage log, which is how an operator starts a fresh
// measurement after a change. It returns how many records it removed.
func (s *SQLiteStore) ClearUsageEvents(ctx context.Context) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	result, err := s.db.ExecContext(ctx, `DELETE FROM gateway_usage_events`)
	if err != nil {
		return 0, fmt.Errorf("clear usage events: %w", err)
	}
	removed, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count cleared usage events: %w", err)
	}
	return removed, nil
}

// usageWhere renders the WHERE clause for a filter and the arguments it needs.
// Every condition is optional, so an empty filter narrows nothing.
func usageWhere(filter usage.Filter) (string, []any) {
	conditions := make([]string, 0, 4)
	arguments := make([]any, 0, 4)
	if !filter.Since.IsZero() {
		conditions = append(conditions, `at >= ?`)
		arguments = append(arguments, filter.Since.UTC().Format(time.RFC3339Nano))
	}
	if !filter.Until.IsZero() {
		conditions = append(conditions, `at < ?`)
		arguments = append(arguments, filter.Until.UTC().Format(time.RFC3339Nano))
	}
	if model := strings.TrimSpace(filter.Model); model != "" {
		conditions = append(conditions, `model = ? COLLATE NOCASE`)
		arguments = append(arguments, model)
	}
	if filter.KeyID > 0 {
		conditions = append(conditions, `key_id = ?`)
		arguments = append(arguments, filter.KeyID)
	}
	if channel := strings.TrimSpace(filter.Channel); channel != "" {
		conditions = append(conditions, `(channel_id = ? OR channel_name = ? COLLATE NOCASE)`)
		arguments = append(arguments, channel, channel)
	}
	if len(conditions) == 0 {
		return "", arguments
	}
	return " WHERE " + strings.Join(conditions, " AND "), arguments
}
