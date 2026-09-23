package store

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yhw5231/fluxgate/internal/domain"
)

// A request record has to survive the round trip through the database whole: the
// attempts are what explain a failure, and a view that lost them would show the
// status without the reason.
func TestRequestLogStoresAndReturnsRecords(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	if err := store.EnsureRequestLogSchema(ctx); err != nil {
		t.Fatalf("EnsureRequestLogSchema() error = %v", err)
	}

	at := time.Date(2026, time.September, 20, 9, 30, 0, 0, time.UTC)
	record := domain.RequestRecord{
		RequestID:    "req-abc",
		At:           at,
		Method:       "POST",
		Path:         "/v1/chat/completions",
		ClientIP:     "203.0.113.7",
		KeyID:        4,
		KeyName:      "内部服务",
		Model:        "gpt-4o",
		Stream:       true,
		Status:       502,
		ErrorCode:    "upstream_unavailable",
		ErrorMessage: "upstream request failed after attempt 2 with status 429",
		DurationMS:   812,
		Attempts: []domain.AttemptTrace{
			{
				Number: 1, ChannelID: "7", ChannelName: "relay", Model: "gpt-4o",
				StatusCode: 429, Retryable: true, StartedAt: at, DurationMS: 400,
				Response: `{"error":{"message":"insufficient balance"}}`,
			},
			{
				Number: 2, ChannelID: "9", ChannelName: "backup", Model: "gpt-4o",
				StatusCode: 503, Retryable: true, StartedAt: at, DurationMS: 412,
				Response: "service unavailable",
			},
		},
	}
	if err := store.AppendRequestRecord(ctx, record, 0); err != nil {
		t.Fatalf("AppendRequestRecord() error = %v", err)
	}

	records, err := store.ListRequestRecords(ctx, RequestLogFilter{})
	if err != nil {
		t.Fatalf("ListRequestRecords() error = %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("records = %d, want 1", len(records))
	}
	stored := records[0]
	if stored.ID == 0 {
		t.Error("stored record has no row id")
	}
	if stored.RequestID != "req-abc" || stored.KeyName != "内部服务" || stored.Model != "gpt-4o" {
		t.Errorf("record = %+v, want the stored request identity", stored)
	}
	if !stored.Stream || stored.Status != 502 || stored.DurationMS != 812 {
		t.Errorf("record = %+v, want the stored outcome", stored)
	}
	if !stored.At.Equal(at) {
		t.Errorf("record time = %v, want %v", stored.At, at)
	}
	if len(stored.Attempts) != 2 {
		t.Fatalf("attempts = %d, want 2", len(stored.Attempts))
	}
	if stored.Attempts[0].Response != `{"error":{"message":"insufficient balance"}}` {
		t.Errorf("attempt response = %q, want the upstream's message", stored.Attempts[0].Response)
	}
	if stored.Attempts[1].ChannelName != "backup" || stored.Attempts[1].StatusCode != 503 {
		t.Errorf("attempt = %+v, want the second line's answer", stored.Attempts[1])
	}
}

// The log is bounded by what it keeps, not by how long the gateway runs: the
// newest records stay and the rest go as new ones are written.
func TestRequestLogKeepsOnlyTheNewestRecords(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	if err := store.EnsureRequestLogSchema(ctx); err != nil {
		t.Fatalf("EnsureRequestLogSchema() error = %v", err)
	}

	for index := 1; index <= 5; index++ {
		record := domain.RequestRecord{
			RequestID: "req-" + strconv.Itoa(index),
			At:        time.Now().UTC(),
			Model:     "m" + strconv.Itoa(index),
			Status:    200,
		}
		if err := store.AppendRequestRecord(ctx, record, 3); err != nil {
			t.Fatalf("AppendRequestRecord(%d) error = %v", index, err)
		}
	}

	records, err := store.ListRequestRecords(ctx, RequestLogFilter{})
	if err != nil {
		t.Fatalf("ListRequestRecords() error = %v", err)
	}
	if len(records) != 3 {
		t.Fatalf("records = %d, want the log bounded to 3", len(records))
	}
	// Newest first, and the three that were kept are the last three written.
	for index, want := range []string{"m5", "m4", "m3"} {
		if records[index].Model != want {
			t.Errorf("record %d model = %q, want %q", index, records[index].Model, want)
		}
	}
}

// A view hunting a failure asks for the failed ones, or for the records of one
// model, or for what happened since it last looked.
func TestRequestLogFiltersRecords(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	if err := store.EnsureRequestLogSchema(ctx); err != nil {
		t.Fatalf("EnsureRequestLogSchema() error = %v", err)
	}

	written := []domain.RequestRecord{
		{RequestID: "ok", At: time.Now().UTC(), Model: "gpt-4o", Status: 200},
		{RequestID: "bad", At: time.Now().UTC(), Model: "gpt-4o", Status: 502, ErrorCode: "upstream_unavailable"},
		{RequestID: "other", At: time.Now().UTC(), Model: "claude", Status: 429},
		{RequestID: "unrouted", At: time.Now().UTC(), Model: "gpt-4o", Status: 0, ErrorCode: "no_available_channel"},
	}
	for _, record := range written {
		if err := store.AppendRequestRecord(ctx, record, 0); err != nil {
			t.Fatalf("AppendRequestRecord(%s) error = %v", record.RequestID, err)
		}
	}

	failed, err := store.ListRequestRecords(ctx, RequestLogFilter{FailedOnly: true})
	if err != nil {
		t.Fatalf("ListRequestRecords(failed) error = %v", err)
	}
	if got := requestIDs(failed); got != "unrouted,other,bad" {
		t.Errorf("failed records = %s, want the three that were not served, newest first", got)
	}

	byModel, err := store.ListRequestRecords(ctx, RequestLogFilter{Model: "GPT-4O"})
	if err != nil {
		t.Fatalf("ListRequestRecords(model) error = %v", err)
	}
	if got := requestIDs(byModel); got != "unrouted,bad,ok" {
		t.Errorf("records of one model = %s, want the model's records regardless of case, newest first", got)
	}

	newest, err := store.ListRequestRecords(ctx, RequestLogFilter{Limit: 1})
	if err != nil {
		t.Fatalf("ListRequestRecords(limit) error = %v", err)
	}
	if len(newest) != 1 || newest[0].RequestID != "unrouted" {
		t.Fatalf("newest record = %+v, want the last one written", newest)
	}
	since, err := store.ListRequestRecords(ctx, RequestLogFilter{SinceID: newest[0].ID - 1})
	if err != nil {
		t.Fatalf("ListRequestRecords(since) error = %v", err)
	}
	if got := requestIDs(since); got != "unrouted" {
		t.Errorf("records since = %s, want only the newer ones", got)
	}
}

// A view shows one page at a time, so a read says which page it wants and the
// count says how many there are — under the same filter, or a page and its total
// would describe different records.
func TestRequestLogPagesRecords(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	if err := store.EnsureRequestLogSchema(ctx); err != nil {
		t.Fatalf("EnsureRequestLogSchema() error = %v", err)
	}

	// Five served requests, of which two failed.
	for index, record := range []domain.RequestRecord{
		{RequestID: "s1", At: time.Now().UTC(), Model: "gpt-4o", Status: 200},
		{RequestID: "f2", At: time.Now().UTC(), Model: "gpt-4o", Status: 502},
		{RequestID: "s3", At: time.Now().UTC(), Model: "gpt-4o", Status: 200},
		{RequestID: "f4", At: time.Now().UTC(), Model: "claude", Status: 429},
		{RequestID: "s5", At: time.Now().UTC(), Model: "gpt-4o", Status: 200},
	} {
		if err := store.AppendRequestRecord(ctx, record, 0); err != nil {
			t.Fatalf("AppendRequestRecord(%d) error = %v", index, err)
		}
	}

	first, err := store.ListRequestRecords(ctx, RequestLogFilter{Limit: 2})
	if err != nil {
		t.Fatalf("ListRequestRecords(page 1) error = %v", err)
	}
	if got := requestIDs(first); got != "s5,f4" {
		t.Errorf("page 1 = %s, want the two newest", got)
	}
	second, err := store.ListRequestRecords(ctx, RequestLogFilter{Limit: 2, Offset: 2})
	if err != nil {
		t.Fatalf("ListRequestRecords(page 2) error = %v", err)
	}
	if got := requestIDs(second); got != "s3,f2" {
		t.Errorf("page 2 = %s, want the next two", got)
	}
	last, err := store.ListRequestRecords(ctx, RequestLogFilter{Limit: 2, Offset: 4})
	if err != nil {
		t.Fatalf("ListRequestRecords(page 3) error = %v", err)
	}
	if got := requestIDs(last); got != "s1" {
		t.Errorf("page 3 = %s, want the remainder", got)
	}

	total, err := store.CountRequestRecords(ctx, RequestLogFilter{})
	if err != nil {
		t.Fatalf("CountRequestRecords() error = %v", err)
	}
	if total != 5 {
		t.Errorf("total = %d, want every record counted", total)
	}

	// The window is not part of the count: the failed filter counts three
	// pages' worth of a smaller log, not the window that was asked for.
	failed, err := store.CountRequestRecords(ctx, RequestLogFilter{FailedOnly: true, Limit: 1, Offset: 1})
	if err != nil {
		t.Fatalf("CountRequestRecords(failed) error = %v", err)
	}
	if failed != 2 {
		t.Errorf("failed total = %d, want the two failures counted regardless of the page", failed)
	}
}

func TestClearRequestRecordsEmptiesTheLog(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	if err := store.EnsureRequestLogSchema(ctx); err != nil {
		t.Fatalf("EnsureRequestLogSchema() error = %v", err)
	}
	for index := 0; index < 3; index++ {
		if err := store.AppendRequestRecord(ctx, domain.RequestRecord{At: time.Now().UTC(), Status: 200}, 0); err != nil {
			t.Fatalf("AppendRequestRecord() error = %v", err)
		}
	}
	removed, err := store.ClearRequestRecords(ctx)
	if err != nil {
		t.Fatalf("ClearRequestRecords() error = %v", err)
	}
	if removed != 3 {
		t.Errorf("removed = %d, want 3", removed)
	}
	records, err := store.ListRequestRecords(ctx, RequestLogFilter{})
	if err != nil {
		t.Fatalf("ListRequestRecords() error = %v", err)
	}
	if len(records) != 0 {
		t.Errorf("records = %d, want the log empty", len(records))
	}
}

func requestIDs(records []domain.RequestRecord) string {
	ids := make([]string, 0, len(records))
	for _, record := range records {
		ids = append(ids, record.RequestID)
	}
	return strings.Join(ids, ",")
}
