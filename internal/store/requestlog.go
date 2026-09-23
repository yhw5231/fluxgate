package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/yhw5231/fluxgate/internal/domain"
)

// The request log.
//
// Every proxied request can leave a record: what was asked for, which lines were
// tried, what each upstream answered, and how the request ended. It is what
// turns "the gateway answered 502" into the upstream's own words, which is the
// only way an operator can tell a wrong address from an exhausted key from a
// rate limit.
//
// The table is the gateway's own, like gateway_breaker_states, and is created
// on demand rather than being part of the upstream schema the gateway only
// reads. Records are bounded: the log keeps the newest N and drops the rest as
// it writes, so a long-running gateway cannot grow the shared database without
// limit.

// gatewayRequestLogDDL is the schema of the gateway-owned request log.
const gatewayRequestLogDDL = `CREATE TABLE IF NOT EXISTS gateway_request_log (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	request_id TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	method TEXT NOT NULL DEFAULT '',
	path TEXT NOT NULL DEFAULT '',
	client_ip TEXT NOT NULL DEFAULT '',
	key_id INTEGER NOT NULL DEFAULT 0,
	key_name TEXT NOT NULL DEFAULT '',
	model TEXT NOT NULL DEFAULT '',
	stream INTEGER NOT NULL DEFAULT 0,
	status INTEGER NOT NULL DEFAULT 0,
	error_code TEXT NOT NULL DEFAULT '',
	error_message TEXT NOT NULL DEFAULT '',
	duration_ms INTEGER NOT NULL DEFAULT 0,
	attempts INTEGER NOT NULL DEFAULT 0,
	detail TEXT NOT NULL DEFAULT ''
)`

// EnsureRequestLogSchema creates the request log on a database that does not have
// it yet. It is called at startup, so the console's request view works from the
// first request on.
func (s *SQLiteStore) EnsureRequestLogSchema(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, gatewayRequestLogDDL); err != nil {
		return fmt.Errorf("create gateway_request_log: %w", err)
	}
	return nil
}

// AppendRequestRecord stores one record and keeps the log bounded to the newest
// keep records. A keep of zero or less stores the record without pruning, which
// is what a caller that does not bound its log asks for.
func (s *SQLiteStore) AppendRequestRecord(ctx context.Context, record domain.RequestRecord, keep int) error {
	detail, err := json.Marshal(attemptsOf(record))
	if err != nil {
		return fmt.Errorf("encode request record attempts: %w", err)
	}
	createdAt := record.At
	if createdAt.IsZero() {
		createdAt = time.Now()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.db.ExecContext(ctx, `INSERT INTO gateway_request_log (
		request_id, created_at, method, path, client_ip, key_id, key_name, model,
		stream, status, error_code, error_message, duration_ms, attempts, detail
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		strings.TrimSpace(record.RequestID),
		createdAt.UTC().Format(time.RFC3339Nano),
		record.Method,
		record.Path,
		record.ClientIP,
		record.KeyID,
		record.KeyName,
		record.Model,
		boolInt(record.Stream),
		record.Status,
		record.ErrorCode,
		record.ErrorMessage,
		record.DurationMS,
		len(record.Attempts),
		string(detail),
	); err != nil {
		return fmt.Errorf("store request record: %w", err)
	}
	if keep <= 0 {
		return nil
	}
	// Pruning here rather than on a timer keeps the bound true at every moment,
	// and the OFFSET form drops exactly the rows past the newest keep rather than
	// guessing from the row ids.
	if _, err := s.db.ExecContext(ctx, `DELETE FROM gateway_request_log WHERE id IN (
		SELECT id FROM gateway_request_log ORDER BY id DESC LIMIT -1 OFFSET ?)`, keep); err != nil {
		return fmt.Errorf("prune request records: %w", err)
	}
	return nil
}

// RequestLogFilter narrows the records a management view asks for. Every field is
// optional; the zero value asks for the newest records whatever they say.
type RequestLogFilter struct {
	// Limit bounds how many records are returned, newest first. Zero means the
	// store's own default.
	Limit int
	// Offset skips that many matching records, which is how a view reads a page
	// beyond the first one. It is counted over the records the rest of the filter
	// matches, so page two of a filtered view is the next page of what the
	// operator is looking at.
	Offset int
	// FailedOnly keeps the requests that were not served, which is what an
	// operator hunting a failure wants.
	FailedOnly bool
	// Model matches the requested model, compared without case.
	Model string
	// SinceID returns only records newer than the one an earlier read ended at,
	// which is how a live view fetches what it does not have yet.
	SinceID int64
}

// defaultRequestLogLimit is how many records a read returns when the caller does
// not say: one page of the console's request view.
const defaultRequestLogLimit = 20

// maxRequestLogLimit bounds a single read, so a request for "everything" cannot
// put an unbounded result in memory.
const maxRequestLogLimit = 2000

// ListRequestRecords returns the stored records that match a filter, newest
// first.
func (s *SQLiteStore) ListRequestRecords(ctx context.Context, filter RequestLogFilter) ([]domain.RequestRecord, error) {
	limit := filter.Limit
	if limit <= 0 {
		limit = defaultRequestLogLimit
	}
	if limit > maxRequestLogLimit {
		limit = maxRequestLogLimit
	}
	offset := filter.Offset
	if offset < 0 {
		offset = 0
	}

	where, args := requestLogWhere(filter)
	query := `SELECT id, request_id, created_at, method, path, client_ip, key_id, key_name, model,
		stream, status, error_code, error_message, duration_ms, detail FROM gateway_request_log` +
		where + " ORDER BY id DESC LIMIT ? OFFSET ?"
	args = append(args, limit, offset)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list request records: %w", err)
	}
	defer rows.Close()

	records := make([]domain.RequestRecord, 0)
	for rows.Next() {
		record, err := scanRequestRecord(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

// CountRequestRecords reports how many records match a filter. The page window is
// not part of it: this is the number a view needs to know how many pages there
// are, so it is counted over exactly the records the filter selects.
func (s *SQLiteStore) CountRequestRecords(ctx context.Context, filter RequestLogFilter) (int, error) {
	where, args := requestLogWhere(filter)
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM gateway_request_log`+where, args...).Scan(&total); err != nil {
		return 0, fmt.Errorf("count request records: %w", err)
	}
	return total, nil
}

// requestLogWhere builds the WHERE clause a filter asks for. A page read and its
// count share it, so a page and the total it is measured against never describe
// different sets of records.
func requestLogWhere(filter RequestLogFilter) (string, []any) {
	clauses := make([]string, 0, 3)
	args := make([]any, 0, 4)
	if filter.SinceID > 0 {
		clauses = append(clauses, "id > ?")
		args = append(args, filter.SinceID)
	}
	if filter.FailedOnly {
		// A request that never reached an upstream carries no status at all and is
		// a failure the operator needs to see just as much as a 502.
		clauses = append(clauses, "(status = 0 OR status >= 400)")
	}
	if model := strings.TrimSpace(filter.Model); model != "" {
		clauses = append(clauses, "model = ? COLLATE NOCASE")
		args = append(args, model)
	}
	if len(clauses) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(clauses, " AND "), args
}

// ClearRequestRecords empties the log and reports how many records it dropped.
func (s *SQLiteStore) ClearRequestRecords(ctx context.Context) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	result, err := s.db.ExecContext(ctx, `DELETE FROM gateway_request_log`)
	if err != nil {
		return 0, fmt.Errorf("clear request records: %w", err)
	}
	return result.RowsAffected()
}

// rowScanner is the part of *sql.Rows a record is read from.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanRequestRecord(row rowScanner) (domain.RequestRecord, error) {
	var (
		record    domain.RequestRecord
		createdAt string
		stream    int
		detail    string
	)
	if err := row.Scan(&record.ID, &record.RequestID, &createdAt, &record.Method, &record.Path,
		&record.ClientIP, &record.KeyID, &record.KeyName, &record.Model, &stream, &record.Status,
		&record.ErrorCode, &record.ErrorMessage, &record.DurationMS, &detail); err != nil {
		return domain.RequestRecord{}, fmt.Errorf("scan request record: %w", err)
	}
	record.Stream = stream != 0
	parsed, err := time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return domain.RequestRecord{}, fmt.Errorf("parse request record time: %w", err)
	}
	record.At = parsed
	if strings.TrimSpace(detail) != "" {
		if err := json.Unmarshal([]byte(detail), &record.Attempts); err != nil {
			// A record whose attempt detail cannot be read is still a record of a
			// request, and the reason it failed may be in the row itself.
			record.Attempts = nil
		}
	}
	return record, nil
}

// attemptsOf renders the attempts of a record for storage, as an empty list
// rather than a null so a reader can iterate the column without a special case.
func attemptsOf(record domain.RequestRecord) []domain.AttemptTrace {
	if record.Attempts == nil {
		return []domain.AttemptTrace{}
	}
	return record.Attempts
}
