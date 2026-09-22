package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yhw5231/fluxgate/internal/domain"
	"github.com/yhw5231/fluxgate/internal/router"
)

// A request that ran out of attempts has to be explainable afterwards: the trace
// carries every attempt, the status the upstream answered with, and the
// upstream's own text, which is what names the cause.
func TestForwardTracesEveryFailedAttempt(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte("{\"error\":{\"message\":\"insufficient balance\"}}\n"))
	}))
	defer upstream.Close()

	engine := Engine{
		Selector: router.NewMemorySelector(routeFor(nil, domain.Channel{
			ID:       "line",
			Name:     "relay",
			BaseURL:  upstream.URL,
			APIKey:   "sk-upstream",
			Enabled:  true,
			Priority: 1,
		})),
		Client: &http.Client{},
		Policy: domain.RetryPolicy{
			MaxAttempts:           2,
			MaxAttemptsPerChannel: 2,
			RetryStatuses:         map[int]struct{}{http.StatusTooManyRequests: {}},
		},
		Sleep: noSleep,
	}

	result, err := engine.Forward(context.Background(), domain.Request{
		Method:    http.MethodPost,
		Path:      "/v1/chat/completions",
		Body:      []byte(`{"model":"m"}`),
		Model:     "m",
		RequestID: "req-1",
	})
	if err == nil {
		t.Fatal("Forward() error = nil, want the upstream failure")
	}
	if len(result.Trace.Attempts) != 2 {
		t.Fatalf("traced attempts = %d, want 2", len(result.Trace.Attempts))
	}
	for index, attempt := range result.Trace.Attempts {
		if attempt.StatusCode != http.StatusTooManyRequests {
			t.Errorf("attempt %d status = %d, want 429", index+1, attempt.StatusCode)
		}
		if !attempt.Retryable {
			t.Errorf("attempt %d retryable = false, want true", index+1)
		}
		if attempt.ChannelName != "relay" || attempt.ChannelID != "line" {
			t.Errorf("attempt %d line = %q/%q, want relay/line", index+1, attempt.ChannelName, attempt.ChannelID)
		}
		if !strings.Contains(attempt.Response, "insufficient balance") {
			t.Errorf("attempt %d response = %q, want the upstream message", index+1, attempt.Response)
		}
		if attempt.Number != index+1 {
			t.Errorf("attempt %d number = %d", index+1, attempt.Number)
		}
	}
}

// A failure the gateway passes through reaches the client unchanged: the snippet
// kept for the request log is read ahead and put back, so the body, and its
// length, are the upstream's.
func TestForwardPassesThroughANonRetryableFailureIntact(t *testing.T) {
	body := `{"error":{"message":"model not found","type":"invalid_request_error"}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(body))
	}))
	defer upstream.Close()

	engine := Engine{
		Selector: router.NewMemorySelector(routeFor(nil, domain.Channel{
			ID: "line", BaseURL: upstream.URL, Enabled: true,
		})),
		Client: &http.Client{},
		Policy: domain.RetryPolicy{
			MaxAttempts:           2,
			MaxAttemptsPerChannel: 1,
			RetryStatuses:         map[int]struct{}{http.StatusServiceUnavailable: {}},
		},
		Sleep: noSleep,
	}

	result, err := engine.Forward(context.Background(), domain.Request{
		Method: http.MethodPost, Path: "/v1/chat/completions",
		Body: []byte(`{"model":"m"}`), Model: "m",
	})
	if err != nil {
		t.Fatalf("Forward() error = %v, want the upstream response", err)
	}
	defer result.Response.Body.Close()

	if result.Response.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", result.Response.StatusCode)
	}
	if result.Response.ContentLength != int64(len(body)) {
		t.Errorf("ContentLength = %d, want %d", result.Response.ContentLength, len(body))
	}
	received, err := io.ReadAll(result.Response.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(received) != body {
		t.Errorf("body = %q, want %q", received, body)
	}
	if len(result.Trace.Attempts) != 1 || !strings.Contains(result.Trace.Attempts[0].Response, "model not found") {
		t.Errorf("trace = %+v, want the upstream's explanation", result.Trace.Attempts)
	}
}

// sanitizeSnippet keeps a record readable: an upstream that answers with
// compressed or binary bytes must not put them into the log.
func TestSanitizeSnippetRendersText(t *testing.T) {
	cases := []struct {
		name     string
		raw      string
		expected string
	}{
		{"json error", "{\n  \"error\": \"bad key\"\n}\n", `{ "error": "bad key" }`},
		{"binary", "\x1f\x8b\x08\x00\x00", ""},
		{"control characters", "a\x00b\x07c", "a b c"},
	}
	for _, testCase := range cases {
		if got := sanitizeSnippet([]byte(testCase.raw)); got != testCase.expected {
			t.Errorf("%s: sanitizeSnippet(%q) = %q, want %q", testCase.name, testCase.raw, got, testCase.expected)
		}
	}
}

// An upstream that echoes the key it rejected must not put it into the record:
// the credential is the one thing that never leaves the gateway.
func TestForwardRedactsTheCredentialFromAnUpstreamError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid api key sk-leaky-upstream provided"}}`))
	}))
	defer upstream.Close()

	engine := Engine{
		Selector: router.NewMemorySelector(routeFor(nil, domain.Channel{
			ID: "line", Name: "relay", BaseURL: upstream.URL, APIKey: "sk-leaky-upstream", Enabled: true,
		})),
		Client: &http.Client{},
		Policy: domain.RetryPolicy{MaxAttempts: 1, MaxAttemptsPerChannel: 1},
	}

	result, err := engine.Forward(context.Background(), domain.Request{
		Method: http.MethodPost, Path: "/v1/chat/completions",
		Body: []byte(`{"model":"m"}`), Model: "m",
	})
	if err != nil {
		t.Fatalf("Forward() error = %v, want the upstream response", err)
	}
	defer result.Response.Body.Close()

	attempt := result.Trace.Attempts[0]
	if strings.Contains(attempt.Response, "sk-leaky-upstream") {
		t.Errorf("recorded response = %q, want the credential redacted", attempt.Response)
	}
	if !strings.Contains(attempt.Response, "[redacted]") {
		t.Errorf("recorded response = %q, want the redaction marker", attempt.Response)
	}
	// The client still receives the upstream's body as it was sent.
	received, err := io.ReadAll(result.Response.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !strings.Contains(string(received), "sk-leaky-upstream") {
		t.Errorf("body = %q, want the upstream's own bytes", received)
	}
}
