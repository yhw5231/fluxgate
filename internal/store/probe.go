package store

import (
	"context"
	"fmt"
	"time"

	"github.com/yhw5231/fluxgate/internal/probe"
)

// The probe log.
//
// A probe asks one line one question and records what came back. The record is
// what lets an operator see a credential that has quietly stopped working before
// a request fails on it: which line, which model, when, how long it took, and
// why it was not healthy.
//
// The table is the gateway's own, like gateway_usage_events and
// gateway_request_log, and is bounded to the newest records so a long-running
// gateway cannot grow the shared database without limit.

// gatewayProbeDDL is the schema of the gateway-owned probe log.
const gatewayProbeDDL = `CREATE TABLE IF NOT EXISTS gateway_probe_runs (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	at TEXT NOT NULL,
	kind TEXT NOT NULL DEFAULT '',
	channel_id TEXT NOT NULL DEFAULT '',
	channel_name TEXT NOT NULL DEFAULT '',
	model TEXT NOT NULL DEFAULT '',
	status INTEGER NOT NULL DEFAULT 0,
	latency_ms INTEGER NOT NULL DEFAULT 0,
	ok INTEGER NOT NULL DEFAULT 0,
	reason TEXT NOT NULL DEFAULT '',
	detail TEXT NOT NULL DEFAULT ''
)`

// EnsureProbeSchema creates the probe log and its index on a database that does
// not have them yet.
func (s *SQLiteStore) EnsureProbeSchema(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, gatewayProbeDDL); err != nil {
		return fmt.Errorf("create gateway_probe_runs: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS gateway_probe_runs_at_idx ON gateway_probe_runs(at)`); err != nil {
		return fmt.Errorf("index gateway_probe_runs: %w", err)
	}
	return nil
}

// AppendProbeRun stores one probe result and keeps the log bounded to the newest
// keep records. A keep of zero or less stores the record without pruning.
func (s *SQLiteStore) AppendProbeRun(ctx context.Context, result probe.Result, keep int) error {
	at := result.At
	if at.IsZero() {
		at = time.Now()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.db.ExecContext(ctx, `INSERT INTO gateway_probe_runs (
		at, kind, channel_id, channel_name, model, status, latency_ms, ok, reason, detail
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		at.UTC().Format(time.RFC3339Nano),
		string(result.Kind),
		result.ChannelID,
		result.ChannelName,
		result.Model,
		result.Status,
		result.LatencyMS,
		boolInt(result.OK),
		result.Reason,
		result.Detail,
	); err != nil {
		return fmt.Errorf("store probe result: %w", err)
	}
	if keep <= 0 {
		return nil
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM gateway_probe_runs WHERE id IN (
		SELECT id FROM gateway_probe_runs ORDER BY id DESC LIMIT -1 OFFSET ?)`, keep); err != nil {
		return fmt.Errorf("prune probe results: %w", err)
	}
	return nil
}

// ListProbeRuns returns the newest probe results first, and how many the log
// holds in total so a view can say how far back it reaches.
func (s *SQLiteStore) ListProbeRuns(ctx context.Context, limit int) ([]probe.Result, int64, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	var total int64
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM gateway_probe_runs`).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count probe results: %w", err)
	}
	rows, err := s.db.QueryContext(ctx, probeResultQuery+` ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, 0, fmt.Errorf("list probe results: %w", err)
	}
	defer rows.Close()
	results := make([]probe.Result, 0, limit)
	for rows.Next() {
		result, err := scanProbeResult(rows)
		if err != nil {
			return nil, 0, err
		}
		results = append(results, result)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate probe results: %w", err)
	}
	return results, total, nil
}

// probeResultQuery is the columns a stored probe result is read from, in the order
// scanProbeResult expects them.
const probeResultQuery = `SELECT at, kind, channel_id, channel_name, model, status, latency_ms, ok, reason, detail
	FROM gateway_probe_runs`

// scanProbeResult reads one stored probe result.
func scanProbeResult(row rowScanner) (probe.Result, error) {
	var (
		at                                  string
		kind, channelID, channelName, model string
		status                              int
		latencyMS                           int64
		ok                                  int
		reason, detail                      string
	)
	if err := row.Scan(&at, &kind, &channelID, &channelName, &model, &status, &latencyMS, &ok, &reason, &detail); err != nil {
		return probe.Result{}, fmt.Errorf("scan probe result: %w", err)
	}
	parsed, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return probe.Result{}, fmt.Errorf("parse probe result time: %w", err)
	}
	return probe.Result{
		At:          parsed.UTC(),
		Kind:        probe.Kind(kind),
		ChannelID:   channelID,
		ChannelName: channelName,
		Model:       model,
		Status:      status,
		LatencyMS:   latencyMS,
		OK:          ok != 0,
		Reason:      reason,
		Detail:      detail,
	}, nil
}

// LatestProbeResults returns the newest recorded result for each line, keyed by
// channel id. The list of lines a probe could ask is a live view of the
// configuration; this is what says how each of those lines last answered, and it
// must not depend on the newest records happening to include every line.
func (s *SQLiteStore) LatestProbeResults(ctx context.Context) (map[string]probe.Result, error) {
	rows, err := s.db.QueryContext(ctx, probeResultQuery+`
		WHERE id IN (SELECT MAX(id) FROM gateway_probe_runs GROUP BY channel_id)`)
	if err != nil {
		return nil, fmt.Errorf("list latest probe results: %w", err)
	}
	defer rows.Close()
	latest := make(map[string]probe.Result)
	for rows.Next() {
		result, err := scanProbeResult(rows)
		if err != nil {
			return nil, err
		}
		latest[result.ChannelID] = result
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate latest probe results: %w", err)
	}
	return latest, nil
}

// ClearProbeRuns empties the probe log, returning how many records it removed.
func (s *SQLiteStore) ClearProbeRuns(ctx context.Context) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	result, err := s.db.ExecContext(ctx, `DELETE FROM gateway_probe_runs`)
	if err != nil {
		return 0, fmt.Errorf("clear probe results: %w", err)
	}
	removed, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count cleared probe results: %w", err)
	}
	return removed, nil
}
