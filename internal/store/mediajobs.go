package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// The record of long-running upstream work.
//
// A generation that an upstream answers with a job identifier is not over when
// the gateway has replied: the client comes back with that identifier to ask how
// the job is going and, eventually, to fetch the artifact. Those follow-up
// requests name no model, so the routing table cannot say where they belong — and
// only the upstream that accepted the job can answer for it. The gateway is
// therefore the one party that can keep the two together, and it records which
// line a job was created on when it creates it.
//
// The table is the gateway's own, like gateway_breaker_states and
// gateway_request_log, and is created on demand. Rows are bounded the same way
// the request log is bounded: the newest are kept and the rest are dropped as a
// new job is written, so a long-running gateway cannot grow the shared database
// without limit.
//
// A row names the downstream key that created the job, because a job is that
// caller's work: another key asking for it is answered as if the job did not
// exist, which is the same answer a client gets for an identifier it invented.

// MediaJob is the upstream-side work one recorded identifier stands for.
type MediaJob struct {
	// JobID is the identifier the upstream gave the work, which is what a
	// follow-up request carries.
	JobID string
	// Kind names the endpoint family the job belongs to, so an identifier from one
	// family cannot be used to reach another.
	Kind string
	// ChannelID is the line the job was created on. It is the route_channels row
	// the follow-up request has to be pinned to, because that row is what holds
	// the upstream, the account and the key that know about the job.
	ChannelID string
	// KeyID is the downstream key that created the job. A job belongs to the
	// caller that asked for it.
	KeyID int64
	// Model is the model the job was created for, which is what the request log
	// reports for the follow-up requests.
	Model string
	// CreatedAt is when the job was recorded.
	CreatedAt time.Time
}

// JobKindVideo is the video endpoint family. A job recorded for one family is not
// offered to a request for another, because two families that happen to name
// their identifiers alike would otherwise reach each other's work.
const JobKindVideo = "video"

// gatewayMediaJobDDL is the schema of the gateway-owned job record.
const gatewayMediaJobDDL = `CREATE TABLE IF NOT EXISTS gateway_media_jobs (
	job_id TEXT PRIMARY KEY,
	kind TEXT NOT NULL DEFAULT '',
	channel_id TEXT NOT NULL DEFAULT '',
	key_id INTEGER NOT NULL DEFAULT 0,
	model TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
)`

// EnsureMediaJobSchema creates the job record on a database that does not have it
// yet, and indexes it by creation time so the bound can be enforced cheaply.
func (s *SQLiteStore) EnsureMediaJobSchema(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, gatewayMediaJobDDL); err != nil {
		return fmt.Errorf("create gateway_media_jobs: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS gateway_media_jobs_created_at_idx ON gateway_media_jobs(created_at)`); err != nil {
		return fmt.Errorf("create media job expiry index: %w", err)
	}
	return nil
}

// RecordMediaJob stores where a job was created, keeping the record bounded to the
// newest keep jobs. A keep of zero or less stores the job without pruning.
//
// Writing the same identifier twice replaces the row rather than failing: the
// identifier is the upstream's, and an upstream that reuses one is describing the
// same work.
func (s *SQLiteStore) RecordMediaJob(ctx context.Context, job MediaJob, keep int) error {
	jobID := strings.TrimSpace(job.JobID)
	if jobID == "" {
		return errors.New("a media job needs the identifier its upstream gave it")
	}
	if strings.TrimSpace(job.ChannelID) == "" {
		return errors.New("a media job needs the channel it was created on")
	}
	createdAt := job.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now()
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.db.ExecContext(ctx, `INSERT INTO gateway_media_jobs (
		job_id, kind, channel_id, key_id, model, created_at, updated_at
	) VALUES (?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(job_id) DO UPDATE SET
		kind=excluded.kind,
		channel_id=excluded.channel_id,
		key_id=excluded.key_id,
		model=excluded.model,
		updated_at=excluded.updated_at`,
		jobID, job.Kind, job.ChannelID, job.KeyID, job.Model,
		createdAt.UTC().Format(time.RFC3339Nano), now); err != nil {
		return fmt.Errorf("record media job: %w", err)
	}
	if keep <= 0 {
		return nil
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM gateway_media_jobs WHERE job_id NOT IN (
		SELECT job_id FROM gateway_media_jobs ORDER BY created_at DESC, job_id DESC LIMIT ?
	)`, keep); err != nil {
		return fmt.Errorf("prune media jobs: %w", err)
	}
	return nil
}

// LookupMediaJob resolves an identifier to the work it stands for, for the
// downstream key that is asking. A job recorded for another key is reported as
// unknown rather than as forbidden, so the answer does not confirm that some
// other caller's job exists.
func (s *SQLiteStore) LookupMediaJob(ctx context.Context, kind, jobID string, keyID int64) (MediaJob, bool, error) {
	jobID = strings.TrimSpace(jobID)
	if jobID == "" {
		return MediaJob{}, false, nil
	}
	var (
		job       MediaJob
		createdAt string
	)
	err := s.db.QueryRowContext(ctx, `SELECT job_id, kind, channel_id, key_id, model, created_at
		FROM gateway_media_jobs WHERE job_id = ?`, jobID).Scan(
		&job.JobID, &job.Kind, &job.ChannelID, &job.KeyID, &job.Model, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return MediaJob{}, false, nil
	}
	if err != nil {
		return MediaJob{}, false, fmt.Errorf("look up media job: %w", err)
	}
	if kind != "" && job.Kind != kind {
		return MediaJob{}, false, nil
	}
	// A job belongs to the caller that asked for it, so another key asking for the
	// same identifier is answered as if it did not exist.
	if job.KeyID != keyID {
		return MediaJob{}, false, nil
	}
	if parsed, err := time.Parse(time.RFC3339Nano, createdAt); err == nil {
		job.CreatedAt = parsed
	}
	return job, true, nil
}

// DeleteMediaJob forgets a job, which is what an upstream-confirmed deletion
// leaves behind. A job that was never recorded is not an error: the caller wanted
// it gone and it is gone.
func (s *SQLiteStore) DeleteMediaJob(ctx context.Context, jobID string) error {
	jobID = strings.TrimSpace(jobID)
	if jobID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.db.ExecContext(ctx, `DELETE FROM gateway_media_jobs WHERE job_id = ?`, jobID); err != nil {
		return fmt.Errorf("delete media job: %w", err)
	}
	return nil
}
