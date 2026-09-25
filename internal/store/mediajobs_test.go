package store

import (
	"context"
	"testing"
	"time"
)

// A generation job is remembered so that a request naming only its identifier can
// find the line that holds it. The record has to survive the round trip whole: the
// channel is the whole point of it, and a record that lost it would route a
// follow-up request to an upstream that never heard of the job.
func TestMediaJobRoundTripsThroughTheDatabase(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	if err := store.EnsureMediaJobSchema(ctx); err != nil {
		t.Fatalf("EnsureMediaJobSchema() error = %v", err)
	}

	createdAt := time.Date(2026, time.September, 24, 10, 0, 0, 0, time.UTC)
	job := MediaJob{
		JobID:     "video_abc123",
		Kind:      JobKindVideo,
		ChannelID: "42",
		KeyID:     7,
		Model:     "sora-2",
		CreatedAt: createdAt,
	}
	if err := store.RecordMediaJob(ctx, job, 0); err != nil {
		t.Fatalf("RecordMediaJob() error = %v", err)
	}

	found, ok, err := store.LookupMediaJob(ctx, JobKindVideo, "video_abc123", 7)
	if err != nil {
		t.Fatalf("LookupMediaJob() error = %v", err)
	}
	if !ok {
		t.Fatal("LookupMediaJob() did not find the recorded job")
	}
	if found.ChannelID != "42" {
		t.Fatalf("ChannelID = %q, want 42", found.ChannelID)
	}
	if found.Model != "sora-2" {
		t.Fatalf("Model = %q, want sora-2", found.Model)
	}
	if found.KeyID != 7 {
		t.Fatalf("KeyID = %d, want 7", found.KeyID)
	}
	if !found.CreatedAt.Equal(createdAt) {
		t.Fatalf("CreatedAt = %s, want %s", found.CreatedAt, createdAt)
	}
}

// A job belongs to the caller that asked for it. Another key asking for the same
// identifier is answered as if the job did not exist, which is the same answer it
// gets for an identifier it invented.
func TestMediaJobIsNotVisibleToAnotherDownstreamKey(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	if err := store.EnsureMediaJobSchema(ctx); err != nil {
		t.Fatalf("EnsureMediaJobSchema() error = %v", err)
	}
	if err := store.RecordMediaJob(ctx, MediaJob{
		JobID: "video_owned", Kind: JobKindVideo, ChannelID: "42", KeyID: 7, Model: "sora-2",
	}, 0); err != nil {
		t.Fatalf("RecordMediaJob() error = %v", err)
	}

	if _, ok, err := store.LookupMediaJob(ctx, JobKindVideo, "video_owned", 8); err != nil {
		t.Fatalf("LookupMediaJob() error = %v", err)
	} else if ok {
		t.Fatal("a job was visible to a downstream key that did not create it")
	}
	if _, ok, err := store.LookupMediaJob(ctx, JobKindVideo, "video_owned", 7); err != nil {
		t.Fatalf("LookupMediaJob() error = %v", err)
	} else if !ok {
		t.Fatal("the job was not visible to the key that created it")
	}
}

// An identifier from one endpoint family must not reach another family's work.
func TestMediaJobIsNotVisibleToAnotherEndpointFamily(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	if err := store.EnsureMediaJobSchema(ctx); err != nil {
		t.Fatalf("EnsureMediaJobSchema() error = %v", err)
	}
	if err := store.RecordMediaJob(ctx, MediaJob{
		JobID: "job_1", Kind: JobKindVideo, ChannelID: "42", KeyID: 7, Model: "sora-2",
	}, 0); err != nil {
		t.Fatalf("RecordMediaJob() error = %v", err)
	}

	if _, ok, err := store.LookupMediaJob(ctx, "audio", "job_1", 7); err != nil {
		t.Fatalf("LookupMediaJob() error = %v", err)
	} else if ok {
		t.Fatal("a video job answered for another endpoint family")
	}
}

func TestUnknownMediaJobIsNotFound(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	if err := store.EnsureMediaJobSchema(ctx); err != nil {
		t.Fatalf("EnsureMediaJobSchema() error = %v", err)
	}

	if _, ok, err := store.LookupMediaJob(ctx, JobKindVideo, "video_never_seen", 7); err != nil {
		t.Fatalf("LookupMediaJob() error = %v", err)
	} else if ok {
		t.Fatal("an identifier that was never recorded was reported as found")
	}
}

// The record is a routing aid and is bounded: the newest jobs are kept and the
// oldest are dropped as a new one is written, so a long-running gateway cannot
// grow the shared database without limit.
func TestMediaJobPruningKeepsTheNewest(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	if err := store.EnsureMediaJobSchema(ctx); err != nil {
		t.Fatalf("EnsureMediaJobSchema() error = %v", err)
	}
	base := time.Date(2026, time.September, 24, 10, 0, 0, 0, time.UTC)
	for index, jobID := range []string{"video_oldest", "video_middle", "video_newest"} {
		if err := store.RecordMediaJob(ctx, MediaJob{
			JobID:     jobID,
			Kind:      JobKindVideo,
			ChannelID: "42",
			KeyID:     7,
			CreatedAt: base.Add(time.Duration(index) * time.Minute),
		}, 2); err != nil {
			t.Fatalf("RecordMediaJob(%s) error = %v", jobID, err)
		}
	}

	for jobID, want := range map[string]bool{"video_oldest": false, "video_middle": true, "video_newest": true} {
		_, ok, err := store.LookupMediaJob(ctx, JobKindVideo, jobID, 7)
		if err != nil {
			t.Fatalf("LookupMediaJob(%s) error = %v", jobID, err)
		}
		if ok != want {
			t.Fatalf("LookupMediaJob(%s) found = %v, want %v", jobID, ok, want)
		}
	}
}

// Recording the same identifier twice describes the same work, so a second write
// replaces the first rather than failing: the identifier is the upstream's, and an
// upstream that reuses one means the same job.
func TestMediaJobRecordIsReplacedByASecondWrite(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	if err := store.EnsureMediaJobSchema(ctx); err != nil {
		t.Fatalf("EnsureMediaJobSchema() error = %v", err)
	}
	if err := store.RecordMediaJob(ctx, MediaJob{
		JobID: "video_1", Kind: JobKindVideo, ChannelID: "1", KeyID: 7, Model: "sora-2",
	}, 0); err != nil {
		t.Fatalf("RecordMediaJob() error = %v", err)
	}
	if err := store.RecordMediaJob(ctx, MediaJob{
		JobID: "video_1", Kind: JobKindVideo, ChannelID: "2", KeyID: 7, Model: "sora-2",
	}, 0); err != nil {
		t.Fatalf("RecordMediaJob() second error = %v", err)
	}

	job, ok, err := store.LookupMediaJob(ctx, JobKindVideo, "video_1", 7)
	if err != nil {
		t.Fatalf("LookupMediaJob() error = %v", err)
	}
	if !ok || job.ChannelID != "2" {
		t.Fatalf("job = %+v (found = %v), want the second write's channel", job, ok)
	}
}

// A job has to name the work and the line that holds it. A record without either
// would be a row the gateway could not route by, so it is refused where it is
// written rather than discovered at the follow-up request.
func TestMediaJobWriteRejectsAnIncompleteRecord(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	if err := store.EnsureMediaJobSchema(ctx); err != nil {
		t.Fatalf("EnsureMediaJobSchema() error = %v", err)
	}

	if err := store.RecordMediaJob(ctx, MediaJob{Kind: JobKindVideo, ChannelID: "42"}, 0); err == nil {
		t.Fatal("RecordMediaJob() accepted a job with no identifier")
	}
	if err := store.RecordMediaJob(ctx, MediaJob{JobID: "video_1", Kind: JobKindVideo}, 0); err == nil {
		t.Fatal("RecordMediaJob() accepted a job with no channel")
	}
}

func TestDeleteMediaJobForgetsTheJob(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	if err := store.EnsureMediaJobSchema(ctx); err != nil {
		t.Fatalf("EnsureMediaJobSchema() error = %v", err)
	}
	if err := store.RecordMediaJob(ctx, MediaJob{
		JobID: "video_1", Kind: JobKindVideo, ChannelID: "42", KeyID: 7,
	}, 0); err != nil {
		t.Fatalf("RecordMediaJob() error = %v", err)
	}

	if err := store.DeleteMediaJob(ctx, "video_1"); err != nil {
		t.Fatalf("DeleteMediaJob() error = %v", err)
	}
	if _, ok, err := store.LookupMediaJob(ctx, JobKindVideo, "video_1", 7); err != nil {
		t.Fatalf("LookupMediaJob() error = %v", err)
	} else if ok {
		t.Fatal("a deleted job was still found")
	}
	// Deleting what is already gone is what the caller asked for, so it is not an
	// error.
	if err := store.DeleteMediaJob(ctx, "video_1"); err != nil {
		t.Fatalf("DeleteMediaJob() second error = %v", err)
	}
}
