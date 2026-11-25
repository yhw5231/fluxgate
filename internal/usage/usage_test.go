package usage

import (
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// readTap runs an answer through a tap the way the proxy relays it — in chunks —
// and returns what the tap read.
func readTap(t *testing.T, body string, stream bool, chunk int) (int64, int64, bool) {
	t.Helper()
	tap := NewTap(io.NopCloser(strings.NewReader(body)), stream)
	scratch := make([]byte, chunk)
	for {
		_, err := tap.Read(scratch)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read answer: %v", err)
		}
	}
	return tap.Usage()
}

func TestParseBodyReadsTheShapesUpstreamsUse(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		prompt     int64
		completion int64
		ok         bool
	}{
		{"chat", `{"usage":{"prompt_tokens":12,"completion_tokens":34,"total_tokens":46}}`, 12, 34, true},
		{"responses", `{"usage":{"input_tokens":7,"output_tokens":9,"total_tokens":16}}`, 7, 9, true},
		{"embeddings", `{"usage":{"prompt_tokens":5,"total_tokens":5}}`, 5, 0, true},
		{"total only", `{"usage":{"total_tokens":40}}`, 40, 0, true},
		{"no usage", `{"choices":[]}`, 0, 0, false},
		{"empty usage", `{"usage":{}}`, 0, 0, false},
		{"not json", `<html>oops</html>`, 0, 0, false},
		{"empty", ``, 0, 0, false},
	}
	for _, testcase := range cases {
		prompt, completion, ok := ParseBody([]byte(testcase.body))
		if prompt != testcase.prompt || completion != testcase.completion || ok != testcase.ok {
			t.Fatalf("%s: ParseBody = %d, %d, %v; want %d, %d, %v",
				testcase.name, prompt, completion, ok, testcase.prompt, testcase.completion, testcase.ok)
		}
	}
}

func TestTapRelaysAnAnswerUnchanged(t *testing.T) {
	body := `{"usage":{"prompt_tokens":3,"completion_tokens":4}}`
	tap := NewTap(io.NopCloser(strings.NewReader(body)), false)
	relayed, err := io.ReadAll(tap)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(relayed) != body {
		t.Fatalf("relayed %q, want the upstream body unchanged", relayed)
	}
	if prompt, completion, ok := tap.Usage(); !ok || prompt != 3 || completion != 4 {
		t.Fatalf("Usage = %d, %d, %v", prompt, completion, ok)
	}
}

func TestTapReadsUsageFromTheFinalEventOfAStream(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"he"}}]}`,
		"",
		`data: {"choices":[{"delta":{"content":"llo"}}]}`,
		"",
		`data: {"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":22}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	// A chunk of 7 bytes splits every line and the usage object itself across
	// reads, which is what a real transport does.
	prompt, completion, ok := readTap(t, stream, true, 7)
	if !ok || prompt != 11 || completion != 22 {
		t.Fatalf("Usage = %d, %d, %v; want the final event's counts", prompt, completion, ok)
	}
}

func TestTapStreamWithoutUsageReportsUnknown(t *testing.T) {
	stream := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"
	if _, _, ok := readTap(t, stream, true, 16); ok {
		t.Fatal("a stream that states no usage must report unknown, not zero")
	}
}

func TestTapStreamFallsBackToAJSONAnswer(t *testing.T) {
	// A streaming request answered with an ordinary body: some providers reject
	// the request that way, and the error body can still carry usage.
	body := `{"error":{"message":"nope"},"usage":{"prompt_tokens":2,"completion_tokens":1}}`
	prompt, completion, ok := readTap(t, body, true, 5)
	if !ok || prompt != 2 || completion != 1 {
		t.Fatalf("Usage = %d, %d, %v; want the JSON body's counts", prompt, completion, ok)
	}
}

func TestTapDoesNotKeepAnAnswerTooLargeToScan(t *testing.T) {
	large := strings.Repeat("x", maxBodyScan+16)
	tap := NewTap(io.NopCloser(strings.NewReader(large)), false)
	if _, err := io.Copy(io.Discard, tap); err != nil {
		t.Fatalf("relay: %v", err)
	}
	if _, _, ok := tap.Usage(); ok {
		t.Fatal("an answer larger than the scan bound must report unknown usage")
	}
}

func TestTapReportsUsageOnlyOnce(t *testing.T) {
	body := `{"usage":{"prompt_tokens":1,"completion_tokens":1}}`
	tap := NewTap(io.NopCloser(strings.NewReader(body)), false)
	if _, err := io.Copy(io.Discard, tap); err != nil {
		t.Fatalf("relay: %v", err)
	}
	var group sync.WaitGroup
	for index := 0; index < 4; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			if _, _, ok := tap.Usage(); !ok {
				t.Error("Usage reported unknown after the answer was read")
			}
		}()
	}
	group.Wait()
}

func TestResolveWindow(t *testing.T) {
	now := time.Date(2026, 3, 15, 18, 30, 0, 0, time.UTC)

	// Today is the operator's local calendar day, so the span is computed in the
	// local zone and reported in UTC.
	name, since, until := ResolveWindow("today", now)
	if name != WindowToday || !until.Equal(now) {
		t.Fatalf("today window = %q, %v, %v", name, since, until)
	}
	local := now.Local()
	expected := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, local.Location())
	if !since.Equal(expected.UTC()) {
		t.Fatalf("today since = %v, want local midnight %v", since, expected.UTC())
	}

	cases := map[string]time.Duration{
		Window24h: 24 * time.Hour,
		Window7d:  7 * 24 * time.Hour,
		Window30d: 30 * 24 * time.Hour,
	}
	for window, span := range cases {
		name, since, until := ResolveWindow(window, now)
		if name != window || !since.Equal(now.Add(-span)) || !until.Equal(now) {
			t.Fatalf("%s window = %q, %v, %v", window, name, since, until)
		}
	}

	allName, allSince, _ := ResolveWindow("all", now)
	if allName != WindowAll || !allSince.IsZero() {
		t.Fatalf("all window = %q, %v; want an unbounded span", allName, allSince)
	}
	// An unknown name falls back to the last seven days rather than an error: a
	// view that asks for a window it does not know still shows something.
	if name, _, _ := ResolveWindow("last-quarter", now); name != Window7d {
		t.Fatalf("unknown window = %q, want %q", name, Window7d)
	}
	if name, _, _ := ResolveWindow("  ", now); name != Window7d {
		t.Fatalf("blank window = %q, want %q", name, Window7d)
	}
}

func TestEventTotals(t *testing.T) {
	event := Event{PromptTokens: 4, CompletionTokens: 6}
	if event.TotalTokens() != 10 {
		t.Fatalf("TotalTokens = %d", event.TotalTokens())
	}
	if !(Event{Status: 0}).Failed() || !(Event{Status: 500}).Failed() {
		t.Fatal("an unanswered or failed request must read as failed")
	}
	if (Event{Status: 200}).Failed() {
		t.Fatal("a served request must not read as failed")
	}
}
