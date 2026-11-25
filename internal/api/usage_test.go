package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/yhw5231/fluxgate/internal/domain"
	"github.com/yhw5231/fluxgate/internal/policy"
	"github.com/yhw5231/fluxgate/internal/store"
	"github.com/yhw5231/fluxgate/internal/usage"
)

// usageServer assembles a gateway that keeps a usage log on top of the request
// log harness, so the whole path — proxy, tap, store, endpoint — is exercised
// against a real table.
func usageServer(t *testing.T, authenticator Authenticator, channels ...domain.Channel) (*Server, *store.SQLiteStore) {
	t.Helper()
	server, persistent := requestLogServer(t, authenticator, channels...)
	if err := persistent.EnsureUsageSchema(context.Background()); err != nil {
		t.Fatalf("EnsureUsageSchema() error = %v", err)
	}
	server.Usage = persistent
	return server, persistent
}

// storedUsage asks the store what it holds, the way the console's view does.
func storedUsage(t *testing.T, persistent *store.SQLiteStore) usage.Summary {
	t.Helper()
	ctx := context.Background()
	retention, err := persistent.UsageRetention(ctx, 1000, true)
	if err != nil {
		t.Fatalf("UsageRetention() error = %v", err)
	}
	summary, err := persistent.QueryUsage(ctx, usage.Filter{}, retention)
	if err != nil {
		t.Fatalf("QueryUsage() error = %v", err)
	}
	return summary
}

// namedAuthentication accepts the test credential under a name, so the usage
// record can be checked for the label an operator sees.
func namedAuthentication(name string) Authenticator {
	return &fakeAuthenticator{
		wantCredential: "test-key",
		key:            store.DownstreamAPIKey{ID: 42, Name: name},
	}
}

func TestProxyRecordsWhatAnAnswerCost(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[],"usage":{"prompt_tokens":12,"completion_tokens":34}}`))
	}))
	defer upstream.Close()

	server, persistent := usageServer(t, namedAuthentication("内部服务"), domain.Channel{
		ID: "line-one", Name: "relay", BaseURL: upstream.URL, APIKey: "sk-upstream", Enabled: true,
	})
	response := proxyRequest(server, `{"model":"gpt-4.1","messages":[]}`)
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}

	summary := storedUsage(t, persistent)
	if summary.Requests != 1 || summary.Succeeded != 1 {
		t.Fatalf("summary = %#v, want one served request", summary)
	}
	if summary.PromptTokens != 12 || summary.CompletionTokens != 34 || summary.TotalTokens != 46 {
		t.Fatalf("tokens = %d/%d/%d, want 12/34/46",
			summary.PromptTokens, summary.CompletionTokens, summary.TotalTokens)
	}
	if summary.Reported != 1 {
		t.Fatalf("reported = %d, want the request measured", summary.Reported)
	}
	if len(summary.Models) != 1 || summary.Models[0].Name != "gpt-4.1" {
		t.Fatalf("models = %#v, want the requested model", summary.Models)
	}
	if len(summary.Channels) != 1 || summary.Channels[0].Name != "relay" {
		t.Fatalf("channels = %#v, want the line that answered", summary.Channels)
	}
	if len(summary.Keys) != 1 || summary.Keys[0].Name != "内部服务" {
		t.Fatalf("keys = %#v, want the client key by name", summary.Keys)
	}
	if summary.Retention.Oldest.IsZero() || !summary.Retention.Enabled {
		t.Fatalf("retention = %#v", summary.Retention)
	}
}

func TestProxyRecordsTheUsageOfAStreamedAnswer(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"choices\":[],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":6}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer upstream.Close()

	server, persistent := usageServer(t, namedAuthentication("内部服务"), domain.Channel{
		ID: "line-one", Name: "relay", BaseURL: upstream.URL, APIKey: "sk-upstream", Enabled: true,
	})
	response := proxyRequest(server, `{"model":"gpt-4.1","stream":true,"messages":[]}`)
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}

	summary := storedUsage(t, persistent)
	if summary.Requests != 1 || summary.PromptTokens != 5 || summary.CompletionTokens != 6 {
		t.Fatalf("summary = %#v, want one streamed request with its final-event usage", summary)
	}
	if !summary.Retention.Enabled {
		t.Fatal("the usage log must report itself as enabled")
	}
}

func TestUsageEndpointSumsTheWindowAndReportsRetention(t *testing.T) {
	server, persistent := usageServer(t, namedAuthentication("内部服务"), domain.Channel{
		ID: "line-one", Name: "relay", BaseURL: "http://127.0.0.1:1", Enabled: true,
	})
	ctx := context.Background()
	at := time.Now().Add(-2 * time.Hour)
	if err := persistent.AppendUsageEvent(ctx, usage.Event{
		RequestID: "a", At: at, Model: "gpt-4.1", ChannelID: "line-one", ChannelName: "relay",
		KeyID: 42, KeyName: "内部服务", Status: 200, PromptTokens: 100, CompletionTokens: 20, Reported: true,
	}, 1000); err != nil {
		t.Fatalf("AppendUsageEvent() error = %v", err)
	}
	// A request from before the window must not move the totals.
	if err := persistent.AppendUsageEvent(ctx, usage.Event{
		RequestID: "old", At: time.Now().Add(-40 * 24 * time.Hour), Model: "gpt-4.1",
		Status: 200, PromptTokens: 900, Reported: true,
	}, 1000); err != nil {
		t.Fatalf("AppendUsageEvent() error = %v", err)
	}

	request := httptest.NewRequest(http.MethodGet, "/management/usage?window=24h", nil)
	request.Header.Set("Authorization", "Bearer management-secret")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", response.Code, response.Body.String())
	}
	var summary usage.Summary
	if err := json.Unmarshal(response.Body.Bytes(), &summary); err != nil {
		t.Fatalf("decode answer: %v", err)
	}
	if summary.Window != usage.Window24h {
		t.Fatalf("window = %q, want %q", summary.Window, usage.Window24h)
	}
	if summary.Requests != 1 || summary.TotalTokens != 120 {
		t.Fatalf("summary = %#v, want only the record inside the window", summary)
	}
	if summary.Retention.Count != 2 || summary.Retention.Keep <= 0 {
		t.Fatalf("retention = %#v, want the log's own reach", summary.Retention)
	}
	if len(summary.Models) != 1 || summary.Models[0].Name != "gpt-4.1" {
		t.Fatalf("models = %#v", summary.Models)
	}
}

func TestUsageEndpointRefusesAnUnreadableFilter(t *testing.T) {
	server, _ := usageServer(t, namedAuthentication("内部服务"))
	request := httptest.NewRequest(http.MethodGet, "/management/usage?key_id=not-a-number", nil)
	request.Header.Set("Authorization", "Bearer management-secret")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", response.Code, response.Body.String())
	}
}

func TestUsageEndpointRequiresAuthentication(t *testing.T) {
	server, _ := usageServer(t, namedAuthentication("内部服务"))
	request := httptest.NewRequest(http.MethodGet, "/management/usage?window=7d", nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", response.Code)
	}
}

func TestUsageEndpointReportsAGatewayWithoutALog(t *testing.T) {
	server := &Server{ManagementToken: "management-secret"}
	request := httptest.NewRequest(http.MethodGet, "/management/usage?window=7d", nil)
	request.Header.Set("Authorization", "Bearer management-secret")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", response.Code)
	}
}

func TestUsageLogCanBeClearedFromTheConsole(t *testing.T) {
	server, persistent := usageServer(t, namedAuthentication("内部服务"))
	if err := persistent.AppendUsageEvent(context.Background(), usage.Event{
		RequestID: "a", At: time.Now(), Model: "gpt-4.1", PromptTokens: 3, Reported: true,
	}, 1000); err != nil {
		t.Fatalf("AppendUsageEvent() error = %v", err)
	}

	request := httptest.NewRequest(http.MethodDelete, "/management/usage", nil)
	request.Header.Set("Authorization", "Bearer management-secret")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", response.Code, response.Body.String())
	}
	var answer struct {
		Cleared int64 `json:"cleared"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode answer: %v", err)
	}
	if answer.Cleared != 1 {
		t.Fatalf("cleared = %d, want 1", answer.Cleared)
	}
	if summary := storedUsage(t, persistent); summary.Requests != 0 {
		t.Fatalf("summary after clearing = %#v", summary)
	}
}

func TestUsageRecordingHonoursTheRuntimePolicy(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer upstream.Close()

	server, persistent := usageServer(t, namedAuthentication("内部服务"), domain.Channel{
		ID: "line-one", Name: "relay", BaseURL: upstream.URL, APIKey: "sk-upstream", Enabled: true,
	})
	disabled := policy.Default()
	disabled.Usage.Enabled = false
	server.PolicyDefaults = disabled

	response := proxyRequest(server, `{"model":"gpt-4.1","messages":[]}`)
	defer response.Body.Close()
	if summary := storedUsage(t, persistent); summary.Requests != 0 {
		t.Fatalf("summary = %#v, want no usage recorded while the log is switched off", summary)
	}
}
