package store

import (
	"context"
	"testing"
	"time"

	"github.com/yhw5231/fluxgate/internal/usage"
)

func prepareUsageStore(t *testing.T) *SQLiteStore {
	t.Helper()
	store := openTestStore(t)
	if err := store.EnsureUsageSchema(context.Background()); err != nil {
		t.Fatalf("EnsureUsageSchema() error = %v", err)
	}
	return store
}

func appendUsage(t *testing.T, store *SQLiteStore, event usage.Event) {
	t.Helper()
	if err := store.AppendUsageEvent(context.Background(), event, 0); err != nil {
		t.Fatalf("AppendUsageEvent() error = %v", err)
	}
}

func TestUsageLogAggregatesWhatItKeeps(t *testing.T) {
	store := prepareUsageStore(t)
	ctx := context.Background()
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)

	appendUsage(t, store, usage.Event{
		RequestID: "a", At: now.Add(-2 * time.Hour), Path: "/v1/chat/completions",
		Model: "gpt-4.1", ChannelID: "1", ChannelName: "Example",
		KeyID: 7, KeyName: "内部服务", Status: 200,
		PromptTokens: 100, CompletionTokens: 40, Reported: true,
	})
	appendUsage(t, store, usage.Event{
		RequestID: "b", At: now.Add(-time.Hour), Path: "/v1/chat/completions",
		Model: "gpt-4.1", ChannelID: "1", ChannelName: "Example",
		KeyID: 7, KeyName: "内部服务", Status: 200,
		PromptTokens: 50, CompletionTokens: 10, Reported: true,
	})
	appendUsage(t, store, usage.Event{
		RequestID: "c", At: now.Add(-30 * time.Minute), Path: "/v1/chat/completions",
		Model: "claude-3", ChannelID: "2", ChannelName: "Other",
		KeyID: 8, KeyName: "外部", Status: 502,
	})

	retention, err := store.UsageRetention(ctx, 1000, true)
	if err != nil {
		t.Fatalf("UsageRetention() error = %v", err)
	}
	if retention.Count != 3 || !retention.Enabled || retention.Keep != 1000 {
		t.Fatalf("retention = %#v, want three records and the configured bound", retention)
	}
	if retention.Oldest.IsZero() || retention.Newest.IsZero() {
		t.Fatalf("retention bounds = %v..%v, want both set", retention.Oldest, retention.Newest)
	}

	filter := usage.Filter{Since: now.Add(-3 * time.Hour), Until: now}
	summary, err := store.QueryUsage(ctx, filter, retention)
	if err != nil {
		t.Fatalf("QueryUsage() error = %v", err)
	}
	if summary.Requests != 3 || summary.Succeeded != 2 || summary.Failed != 1 {
		t.Fatalf("summary = %#v, want 3 requests, 2 served, 1 failed", summary)
	}
	if summary.PromptTokens != 150 || summary.CompletionTokens != 50 || summary.TotalTokens != 200 {
		t.Fatalf("tokens = %d/%d/%d, want 150/50/200",
			summary.PromptTokens, summary.CompletionTokens, summary.TotalTokens)
	}
	if summary.Reported != 2 {
		t.Fatalf("reported = %d, want the two requests that stated their usage", summary.Reported)
	}
	if len(summary.Models) != 2 {
		t.Fatalf("models = %#v, want two groups", summary.Models)
	}
	// The groups are ordered by what they cost, so the model that used the most
	// tokens is read first.
	if summary.Models[0].Name != "gpt-4.1" || summary.Models[0].TotalTokens != 200 {
		t.Fatalf("first model group = %#v, want gpt-4.1 with 200 tokens", summary.Models[0])
	}
	// A request that stated no usage is still grouped by its model, with zero
	// tokens: it happened, it just cannot be costed.
	if summary.Models[1].Name != "claude-3" || summary.Models[1].Requests != 1 {
		t.Fatalf("second model group = %#v, want claude-3 counted without tokens", summary.Models[1])
	}
	if len(summary.Channels) != 2 || summary.Channels[0].Name != "Example" {
		t.Fatalf("channel groups = %#v", summary.Channels)
	}
	if len(summary.Keys) != 2 {
		t.Fatalf("key groups = %#v", summary.Keys)
	}
	if summary.Retention.Count != retention.Count {
		t.Fatalf("summary retention = %#v, want the retention it was given", summary.Retention)
	}
}

func TestUsageLogNarrowsByModelKeyAndChannel(t *testing.T) {
	store := prepareUsageStore(t)
	ctx := context.Background()
	at := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	for _, event := range []usage.Event{
		{RequestID: "1", At: at, Model: "gpt-4.1", ChannelID: "10", ChannelName: "Alpha", KeyID: 3, KeyName: "k3", Status: 200, PromptTokens: 10, Reported: true},
		{RequestID: "2", At: at.Add(time.Minute), Model: "GPT-4.1", ChannelID: "20", ChannelName: "Beta", KeyID: 4, KeyName: "k4", Status: 200, PromptTokens: 20, Reported: true},
		{RequestID: "3", At: at.Add(2 * time.Minute), Model: "claude-3", ChannelID: "10", ChannelName: "Alpha", KeyID: 3, KeyName: "k3", Status: 200, PromptTokens: 30, Reported: true},
	} {
		appendUsage(t, store, event)
	}
	retention, err := store.UsageRetention(ctx, 1000, true)
	if err != nil {
		t.Fatalf("UsageRetention() error = %v", err)
	}

	// A model is matched without case, so the two spellings of one model are one
	// question.
	summary, err := store.QueryUsage(ctx, usage.Filter{Model: "gpt-4.1"}, retention)
	if err != nil {
		t.Fatalf("QueryUsage(model) error = %v", err)
	}
	if summary.Requests != 2 || summary.PromptTokens != 30 {
		t.Fatalf("model filter = %#v, want the two spellings", summary)
	}

	summary, err = store.QueryUsage(ctx, usage.Filter{KeyID: 3}, retention)
	if err != nil {
		t.Fatalf("QueryUsage(key) error = %v", err)
	}
	if summary.Requests != 2 || summary.PromptTokens != 40 {
		t.Fatalf("key filter = %#v", summary)
	}

	// The channel is matched by either its name or its id, because the usage
	// rows carry both.
	summary, err = store.QueryUsage(ctx, usage.Filter{Channel: "Alpha"}, retention)
	if err != nil {
		t.Fatalf("QueryUsage(channel name) error = %v", err)
	}
	if summary.Requests != 2 {
		t.Fatalf("channel name filter = %#v", summary)
	}
	summary, err = store.QueryUsage(ctx, usage.Filter{Channel: "20"}, retention)
	if err != nil {
		t.Fatalf("QueryUsage(channel id) error = %v", err)
	}
	if summary.Requests != 1 {
		t.Fatalf("channel id filter = %#v", summary)
	}
}

func TestUsageLogKeepsTheNewestRecords(t *testing.T) {
	store := prepareUsageStore(t)
	ctx := context.Background()
	at := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	for index := 0; index < 5; index++ {
		if err := store.AppendUsageEvent(ctx, usage.Event{
			RequestID: string(rune('a' + index)), At: at.Add(time.Duration(index) * time.Minute),
			Model: "m", PromptTokens: int64(index), Reported: true,
		}, 3); err != nil {
			t.Fatalf("AppendUsageEvent() error = %v", err)
		}
	}
	retention, err := store.UsageRetention(ctx, 3, true)
	if err != nil {
		t.Fatalf("UsageRetention() error = %v", err)
	}
	if retention.Count != 3 {
		t.Fatalf("retention count = %d, want the log bounded to 3", retention.Count)
	}
	summary, err := store.QueryUsage(ctx, usage.Filter{}, retention)
	if err != nil {
		t.Fatalf("QueryUsage() error = %v", err)
	}
	// The newest three are records 2, 3 and 4, whose prompt tokens are 2+3+4.
	if summary.PromptTokens != 9 {
		t.Fatalf("prompt tokens = %d, want the newest three records", summary.PromptTokens)
	}
}

func TestUsageRetentionReportsADisabledLog(t *testing.T) {
	store := prepareUsageStore(t)
	retention, err := store.UsageRetention(context.Background(), 500, false)
	if err != nil {
		t.Fatalf("UsageRetention() error = %v", err)
	}
	if retention.Enabled || retention.Count != 0 || retention.Keep != 500 {
		t.Fatalf("retention = %#v, want a disabled log", retention)
	}
}

func TestUsageLogCanBeCleared(t *testing.T) {
	store := prepareUsageStore(t)
	ctx := context.Background()
	appendUsage(t, store, usage.Event{RequestID: "a", At: time.Now(), Model: "m"})
	removed, err := store.ClearUsageEvents(ctx)
	if err != nil {
		t.Fatalf("ClearUsageEvents() error = %v", err)
	}
	if removed != 1 {
		t.Fatalf("cleared = %d, want 1", removed)
	}
	retention, err := store.UsageRetention(ctx, 10, true)
	if err != nil {
		t.Fatalf("UsageRetention() error = %v", err)
	}
	if retention.Count != 0 {
		t.Fatalf("retention count = %d, want an empty log", retention.Count)
	}
}

func TestUsageSchemaIsIdempotent(t *testing.T) {
	store := prepareUsageStore(t)
	if err := store.EnsureUsageSchema(context.Background()); err != nil {
		t.Fatalf("EnsureUsageSchema() second run error = %v", err)
	}
}
