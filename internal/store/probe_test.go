package store

import (
	"context"
	"testing"
	"time"

	"github.com/yhw5231/fluxgate/internal/probe"
)

func prepareProbeStore(t *testing.T) *SQLiteStore {
	t.Helper()
	store := openTestStore(t)
	if err := store.EnsureProbeSchema(context.Background()); err != nil {
		t.Fatalf("EnsureProbeSchema() error = %v", err)
	}
	return store
}

// A probe result is stored and read back as a whole: the line, the kind, the
// answer, and why it was not healthy.
func TestProbeLogRoundTripsAResult(t *testing.T) {
	store := prepareProbeStore(t)
	ctx := context.Background()
	at := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	if err := store.AppendProbeRun(ctx, probe.Result{
		At:          at,
		Kind:        probe.KindIdle,
		ChannelID:   "12",
		ChannelName: "relay",
		Model:       "gpt-4.1",
		Status:      401,
		LatencyMS:   42,
		OK:          false,
		Reason:      probe.ReasonAuthRejected,
		Detail:      "the upstream rejected the credential (HTTP 401)",
	}, 100); err != nil {
		t.Fatalf("AppendProbeRun() error = %v", err)
	}

	runs, total, err := store.ListProbeRuns(ctx, 10)
	if err != nil {
		t.Fatalf("ListProbeRuns() error = %v", err)
	}
	if total != 1 || len(runs) != 1 {
		t.Fatalf("total = %d, runs = %d, want one", total, len(runs))
	}
	run := runs[0]
	if !run.At.Equal(at) {
		t.Fatalf("at = %v, want %v", run.At, at)
	}
	if run.Kind != probe.KindIdle || run.ChannelID != "12" || run.ChannelName != "relay" ||
		run.Model != "gpt-4.1" || run.Status != 401 || run.LatencyMS != 42 ||
		run.OK || run.Reason != probe.ReasonAuthRejected || run.Detail == "" {
		t.Fatalf("run = %#v, want the stored values", run)
	}
}

// The log is bounded and read newest first, which is what the console shows.
func TestProbeLogKeepsTheNewestRuns(t *testing.T) {
	store := prepareProbeStore(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	for index := 0; index < 5; index++ {
		if err := store.AppendProbeRun(ctx, probe.Result{
			At:          base.Add(time.Duration(index) * time.Minute),
			Kind:        probe.KindManual,
			ChannelID:   "12",
			ChannelName: "relay",
			Model:       "gpt-4.1",
			OK:          true,
			Reason:      probe.ReasonOK,
		}, 3); err != nil {
			t.Fatalf("AppendProbeRun() error = %v", err)
		}
	}
	runs, total, err := store.ListProbeRuns(ctx, 10)
	if err != nil {
		t.Fatalf("ListProbeRuns() error = %v", err)
	}
	if total != 3 {
		t.Fatalf("total = %d, want the log bounded to 3", total)
	}
	if len(runs) != 3 {
		t.Fatalf("runs = %d, want 3", len(runs))
	}
	if !runs[0].At.Equal(base.Add(4 * time.Minute)) {
		t.Fatalf("first run = %v, want the newest", runs[0].At)
	}
	if !runs[2].At.Equal(base.Add(2 * time.Minute)) {
		t.Fatalf("last run = %v, want the oldest kept", runs[2].At)
	}
}

func TestProbeLogCanBeCleared(t *testing.T) {
	store := prepareProbeStore(t)
	ctx := context.Background()
	if err := store.AppendProbeRun(ctx, probe.Result{At: time.Now(), Kind: probe.KindManual, OK: true, Reason: probe.ReasonOK}, 10); err != nil {
		t.Fatalf("AppendProbeRun() error = %v", err)
	}
	removed, err := store.ClearProbeRuns(ctx)
	if err != nil {
		t.Fatalf("ClearProbeRuns() error = %v", err)
	}
	if removed != 1 {
		t.Fatalf("cleared = %d, want 1", removed)
	}
	_, total, err := store.ListProbeRuns(ctx, 10)
	if err != nil {
		t.Fatalf("ListProbeRuns() error = %v", err)
	}
	if total != 0 {
		t.Fatalf("total = %d, want an empty log", total)
	}
}

func TestProbeSchemaIsIdempotent(t *testing.T) {
	store := prepareProbeStore(t)
	if err := store.EnsureProbeSchema(context.Background()); err != nil {
		t.Fatalf("EnsureProbeSchema() second run error = %v", err)
	}
}

// The newest result per line is what the console shows beside a line it could
// probe, and it does not depend on the newest records happening to include every
// line: a line probed once an hour ago still has its answer.
func TestLatestProbeResultsKeepsTheNewestPerLine(t *testing.T) {
	store := prepareProbeStore(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	writes := []probe.Result{
		{At: base, ChannelID: "1", ChannelName: "relay", Model: "gpt-4.1", Status: 401, Reason: probe.ReasonAuthRejected},
		{At: base.Add(time.Minute), ChannelID: "2", ChannelName: "backup", Model: "gpt-4.1", Status: 200, OK: true, Reason: probe.ReasonOK},
		{At: base.Add(2 * time.Minute), ChannelID: "1", ChannelName: "relay", Model: "gpt-4.1", Status: 200, OK: true, Reason: probe.ReasonOK},
	}
	for _, result := range writes {
		if err := store.AppendProbeRun(ctx, result, 100); err != nil {
			t.Fatalf("AppendProbeRun() error = %v", err)
		}
	}

	latest, err := store.LatestProbeResults(ctx)
	if err != nil {
		t.Fatalf("LatestProbeResults() error = %v", err)
	}
	if len(latest) != 2 {
		t.Fatalf("latest = %#v, want one per line", latest)
	}
	if result := latest["1"]; !result.OK || !result.At.Equal(base.Add(2*time.Minute)) {
		t.Errorf("line 1 = %#v, want its newest result", result)
	}
	if result := latest["2"]; !result.OK || !result.At.Equal(base.Add(time.Minute)) {
		t.Errorf("line 2 = %#v, want the only result it has", result)
	}
}
