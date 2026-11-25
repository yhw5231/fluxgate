package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yhw5231/fluxgate/internal/domain"
	"github.com/yhw5231/fluxgate/internal/protocol"
	"github.com/yhw5231/fluxgate/internal/proxy"
	"github.com/yhw5231/fluxgate/internal/router"
	"github.com/yhw5231/fluxgate/internal/store"
)

func allowTestAuthentication() Authenticator {
	return &fakeAuthenticator{
		wantCredential: "test-key",
		key:            store.DownstreamAPIKey{},
	}
}

func TestHealthCheck(t *testing.T) {
	server := &Server{}
	request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	response := httptest.NewRecorder()

	server.Handler().ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Code)
	}
	if got := response.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
	if !strings.Contains(response.Body.String(), `"status":"ok"`) {
		t.Fatalf("body = %s, want healthy status", response.Body.String())
	}
}

func TestOpenAICompatibleForwarding(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("upstream path = %q, want /v1/chat/completions", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Upstream", "present")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"chatcmpl-test"}`))
	}))
	defer upstream.Close()

	engine := &proxy.Engine{
		Selector: router.NewMemorySelector(testRoutes(domain.Channel{
			ID:       "channel-one",
			BaseURL:  upstream.URL,
			Enabled:  true,
			Priority: 10,
			Weight:   1,
		})),
		Policy: domain.RetryPolicy{
			MaxAttempts:           1,
			MaxAttemptsPerChannel: 1,
		},
		Sleep: func(context.Context, time.Duration) error { return nil },
	}
	server := &Server{Engine: engine, Authenticator: allowTestAuthentication(), MaxRequestBodyBytes: 1024}
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4.1","messages":[]}`),
	)
	request.Header.Set("Authorization", "Bearer test-key")
	response := httptest.NewRecorder()

	server.Handler().ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", response.Code, response.Body.String())
	}
	if got := response.Header().Get("X-Fluxgate-Upstream-Channel"); got != "channel-one" {
		t.Fatalf("X-Fluxgate-Upstream-Channel = %q, want channel-one", got)
	}
	if got := response.Header().Get("X-Upstream"); got != "present" {
		t.Fatalf("X-Upstream = %q, want present", got)
	}
	if got := response.Body.String(); got != `{"id":"chatcmpl-test"}` {
		t.Fatalf("body = %s, want upstream response", got)
	}
}

// A caller that speaks chat completions reaches an upstream that serves only the
// Responses API: the endpoint translates both ways, and the caller still gets a
// chat completion.
func TestProxyTranslatesTheCallersProtocolForTheLine(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != protocol.PathResponses {
			t.Errorf("upstream path = %q, want %q", r.URL.Path, protocol.PathResponses)
		}
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"messages"`) {
			t.Errorf("upstream body still carries chat messages: %s", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": "resp_1", "created_at": 1700000000, "model": "gpt-4.1", "status": "completed",
			"output": [{"type": "message", "content": [{"type": "output_text", "text": "translated"}]}]
		}`))
	}))
	defer upstream.Close()

	engine := &proxy.Engine{
		Selector: router.NewMemorySelector(testRoutes(domain.Channel{
			ID: "channel-one", BaseURL: upstream.URL, Enabled: true, Weight: 1,
			UpstreamAPI: protocol.APIResponses,
		})),
		Policy: domain.RetryPolicy{MaxAttempts: 1, MaxAttemptsPerChannel: 1},
		Sleep:  func(context.Context, time.Duration) error { return nil },
	}
	server := &Server{Engine: engine, Authenticator: allowTestAuthentication()}
	request := httptest.NewRequest(http.MethodPost, protocol.PathChatCompletions,
		strings.NewReader(`{"model":"gpt-4.1","messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("Authorization", "Bearer test-key")
	response := httptest.NewRecorder()

	server.Handler().ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"object":"chat.completion"`) {
		t.Fatalf("body = %s, want a chat completion", response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"content":"translated"`) {
		t.Fatalf("body = %s, want the upstream's text", response.Body.String())
	}
}

// A request the line's protocol cannot express is answered as a request error:
// no upstream request was made, and the caller can see why.
func TestProxyReportsARequestTheLineCannotExpress(t *testing.T) {
	var calls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	engine := &proxy.Engine{
		Selector: router.NewMemorySelector(testRoutes(domain.Channel{
			ID: "channel-one", BaseURL: upstream.URL, Enabled: true, Weight: 1,
			UpstreamAPI: protocol.APIResponses,
		})),
		Policy: domain.RetryPolicy{MaxAttempts: 1, MaxAttemptsPerChannel: 1},
		Sleep:  func(context.Context, time.Duration) error { return nil },
	}
	server := &Server{Engine: engine, Authenticator: allowTestAuthentication()}
	// A chat request continued from an assistant turn with no tool result is
	// still a valid chat body, so the role itself is what cannot be expressed.
	request := httptest.NewRequest(http.MethodPost, protocol.PathChatCompletions,
		strings.NewReader(`{"model":"gpt-4.1","messages":[{"role":"unknown-role","content":"hi"}]}`))
	request.Header.Set("Authorization", "Bearer test-key")
	response := httptest.NewRecorder()

	server.Handler().ServeHTTP(response, request)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"code":"request_not_convertible"`) {
		t.Fatalf("body = %s, want request_not_convertible", response.Body.String())
	}
	if calls != 0 {
		t.Fatalf("upstream calls = %d, want none", calls)
	}
}

func TestProxyRejectsInvalidJSON(t *testing.T) {
	server := &Server{Engine: &proxy.Engine{}, Authenticator: allowTestAuthentication()}
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader("not-json"))
	request.Header.Set("Authorization", "Bearer test-key")
	response := httptest.NewRecorder()

	server.Handler().ServeHTTP(response, request)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", response.Code)
	}
	if !strings.Contains(response.Body.String(), `"code":"invalid_json"`) {
		t.Fatalf("body = %s, want invalid_json error", response.Body.String())
	}
}

func TestProxyRejectsMissingModel(t *testing.T) {
	server := &Server{Engine: &proxy.Engine{}, Authenticator: allowTestAuthentication()}
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"messages":[]}`))
	request.Header.Set("Authorization", "Bearer test-key")
	response := httptest.NewRecorder()

	server.Handler().ServeHTTP(response, request)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", response.Code)
	}
	if !strings.Contains(response.Body.String(), `"code":"missing_model"`) {
		t.Fatalf("body = %s, want missing_model error", response.Body.String())
	}
}

func TestProxyEnforcesRequestBodyLimit(t *testing.T) {
	server := &Server{Engine: &proxy.Engine{}, Authenticator: allowTestAuthentication(), MaxRequestBodyBytes: 8}
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4.1"}`),
	)
	request.Header.Set("Authorization", "Bearer test-key")
	response := httptest.NewRecorder()

	server.Handler().ServeHTTP(response, request)

	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", response.Code)
	}
	if !strings.Contains(response.Body.String(), `"code":"request_too_large"`) {
		t.Fatalf("body = %s, want request_too_large error", response.Body.String())
	}
}
