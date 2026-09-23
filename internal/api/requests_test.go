package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yhw5231/fluxgate/internal/domain"
	"github.com/yhw5231/fluxgate/internal/proxy"
	"github.com/yhw5231/fluxgate/internal/router"
	"github.com/yhw5231/fluxgate/internal/store"
)

// requestLogServer assembles a gateway that keeps a request log: a real store
// (the log is a table, so the round trip through it is part of what is tested), a
// management token, and an engine that dispatches to the given channels.
func requestLogServer(t *testing.T, authenticator Authenticator, channels ...domain.Channel) (*Server, *store.SQLiteStore) {
	t.Helper()
	persistent, err := store.OpenSQLite(filepath.Join(t.TempDir(), "request-log-test.db"))
	if err != nil {
		t.Fatalf("OpenSQLite() error = %v", err)
	}
	t.Cleanup(func() {
		if err := persistent.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	if err := persistent.EnsureUpstreamSchema(context.Background()); err != nil {
		t.Fatalf("EnsureUpstreamSchema() error = %v", err)
	}
	if err := persistent.EnsureRequestLogSchema(context.Background()); err != nil {
		t.Fatalf("EnsureRequestLogSchema() error = %v", err)
	}

	server := &Server{
		Engine: &proxy.Engine{
			Selector: router.NewMemorySelector(testRoutes(channels...)),
			Client:   &http.Client{},
			Policy: domain.RetryPolicy{
				MaxAttempts:           2,
				MaxAttemptsPerChannel: 2,
				RetryStatuses:         map[int]struct{}{http.StatusTooManyRequests: {}},
			},
			Sleep: func(context.Context, time.Duration) error { return nil },
		},
		Authenticator:   authenticator,
		ManagementToken: "management-secret",
		ConfigStore:     persistent,
		RequestLog:      persistent,
	}
	server.SetConfiguration(store.Configuration{Routes: testRoutes(channels...), Models: []string{"gpt-4.1"}})
	return server, persistent
}

// denyingAuthenticator accepts the test credential and refuses one model, which
// is how a refusal the gateway makes itself is reached.
func denyingAuthenticator(pattern string) Authenticator {
	return &fakeAuthenticator{
		wantCredential: "test-key",
		key:            store.DownstreamAPIKey{SupportedModels: []string{pattern}},
	}
}

// proxyRequest sends one proxied call through the handler the way a client does.
func proxyRequest(server *Server, body string) *http.Response {
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer test-key")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	return response.Result()
}

// A request the gateway could not serve is recorded with the upstream's own words
// and answered with them, so a failure can be looked up instead of guessed at.
func TestFailedRequestIsRecordedAndExplained(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"insufficient balance"}}`))
	}))
	defer upstream.Close()

	server, _ := requestLogServer(t, allowTestAuthentication(), domain.Channel{
		ID: "line-one", Name: "relay", BaseURL: upstream.URL, APIKey: "sk-upstream", Enabled: true,
	})

	response := proxyRequest(server, `{"model":"gpt-4.1"}`)
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", response.StatusCode)
	}

	// The client is told which request this was and what the upstream said: an
	// opaque 502 is what forces an operator to guess.
	requestID := response.Header.Get("X-Fluxgate-Request-Id")
	if requestID == "" {
		t.Fatal("the response carries no request id")
	}
	var failure struct {
		Error struct {
			Code            string `json:"code"`
			Message         string `json:"message"`
			RequestID       string `json:"request_id"`
			UpstreamStatus  int    `json:"upstream_status"`
			UpstreamMessage string `json:"upstream_message"`
			Attempts        int    `json:"attempts"`
		} `json:"error"`
	}
	if err := json.NewDecoder(response.Body).Decode(&failure); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if failure.Error.Code != "upstream_unavailable" {
		t.Errorf("code = %q, want upstream_unavailable", failure.Error.Code)
	}
	if failure.Error.RequestID != requestID {
		t.Errorf("error request id = %q, want the header's %q", failure.Error.RequestID, requestID)
	}
	if failure.Error.UpstreamStatus != http.StatusTooManyRequests || failure.Error.Attempts != 2 {
		t.Errorf("error details = %+v, want the upstream status and attempt count", failure.Error)
	}
	if !strings.Contains(failure.Error.UpstreamMessage, "insufficient balance") {
		t.Errorf("upstream message = %q, want the upstream's own explanation", failure.Error.UpstreamMessage)
	}

	// The same failure is in the log the console reads, with every attempt.
	logged := callManagement(server, http.MethodGet, "/management/requests", "management-secret", nil)
	if logged.status != http.StatusOK {
		t.Fatalf("request log status = %d, body = %v", logged.status, logged.body)
	}
	records, _ := logged.body["requests"].([]any)
	if len(records) != 1 {
		t.Fatalf("records = %d, want the one request that was made", len(records))
	}
	record, _ := records[0].(map[string]any)
	if record["request_id"] != requestID {
		t.Errorf("record request id = %v, want %q", record["request_id"], requestID)
	}
	if record["failed"] != true || record["status"] != float64(http.StatusBadGateway) {
		t.Errorf("record = %v, want a failed request answered 502", record)
	}
	if record["model"] != "gpt-4.1" || record["path"] != "/v1/chat/completions" {
		t.Errorf("record = %v, want the requested model and path", record)
	}
	if !strings.Contains(record["error_message"].(string), "insufficient balance") {
		t.Errorf("record error = %v, want the upstream's explanation", record["error_message"])
	}
	attempts, _ := record["attempts"].([]any)
	if len(attempts) != 2 {
		t.Fatalf("recorded attempts = %d, want 2", len(attempts))
	}
	first, _ := attempts[0].(map[string]any)
	if first["channel_name"] != "relay" || first["status"] != float64(http.StatusTooManyRequests) {
		t.Errorf("attempt = %v, want the line and the upstream status", first)
	}
	if !strings.Contains(first["response"].(string), "insufficient balance") {
		t.Errorf("attempt response = %v, want the upstream body", first["response"])
	}

	// The record never carries a credential: the line is named, not the key.
	encoded, err := json.Marshal(logged.body)
	if err != nil {
		t.Fatalf("marshal log: %v", err)
	}
	if strings.Contains(string(encoded), "sk-upstream") {
		t.Fatalf("the request log carries the upstream credential: %s", encoded)
	}
}

// A request that was served is recorded too, so the log says what the gateway did
// with it and not only what went wrong.
func TestServedRequestIsRecorded(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"chatcmpl-test"}`))
	}))
	defer upstream.Close()

	server, _ := requestLogServer(t, allowTestAuthentication(), domain.Channel{
		ID: "line-one", Name: "relay", BaseURL: upstream.URL, Enabled: true,
	})
	response := proxyRequest(server, `{"model":"gpt-4.1"}`)
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.StatusCode)
	}

	logged := callManagement(server, http.MethodGet, "/management/requests", "management-secret", nil)
	records, _ := logged.body["requests"].([]any)
	if len(records) != 1 {
		t.Fatalf("records = %d, want the one request that was made", len(records))
	}
	record, _ := records[0].(map[string]any)
	if record["failed"] != false || record["status"] != float64(http.StatusOK) {
		t.Errorf("record = %v, want a served request", record)
	}
	if record["attempts"] == nil {
		t.Errorf("record = %v, want the attempt that served it", record)
	}
}

// A request the gateway refuses itself is recorded as well: a model this key may
// not use is exactly the failure an operator has to be able to look up.
func TestRefusedRequestIsRecorded(t *testing.T) {
	server, _ := requestLogServer(t, denyingAuthenticator("denied-model"), domain.Channel{
		ID: "line-one", Name: "relay", BaseURL: "https://upstream.example", Enabled: true,
	})

	response := proxyRequest(server, `{"model":"denied-model","messages":[]}`)
	defer response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", response.StatusCode)
	}

	logged := callManagement(server, http.MethodGet, "/management/requests", "management-secret", nil)
	records, _ := logged.body["requests"].([]any)
	if len(records) != 1 {
		t.Fatalf("records = %d, want the refused request", len(records))
	}
	record, _ := records[0].(map[string]any)
	if record["error_code"] != "model_not_allowed" || record["status"] != float64(http.StatusForbidden) {
		t.Errorf("record = %v, want the refusal and its reason", record)
	}
	if record["model"] != "denied-model" {
		t.Errorf("record model = %v, want the model that was refused", record["model"])
	}
}

// The failed filter is what an operator hunting a failure reads, and clearing the
// log is how they put a fixed problem behind them.
func TestRequestLogFiltersAndClears(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"chatcmpl-test"}`))
	}))
	defer upstream.Close()

	server, _ := requestLogServer(t, denyingAuthenticator("denied-model"), domain.Channel{
		ID: "line-one", Name: "relay", BaseURL: upstream.URL, Enabled: true,
	})
	served := proxyRequest(server, `{"model":"gpt-4.1"}`)
	served.Body.Close()
	refused := proxyRequest(server, `{"model":"denied-model"}`)
	refused.Body.Close()

	all := callManagement(server, http.MethodGet, "/management/requests", "management-secret", nil)
	if records, _ := all.body["requests"].([]any); len(records) != 2 {
		t.Fatalf("records = %d, want both requests", len(records))
	}
	failed := callManagement(server, http.MethodGet, "/management/requests?failed=1", "management-secret", nil)
	records, _ := failed.body["requests"].([]any)
	if len(records) != 1 {
		t.Fatalf("failed records = %d, want the one the gateway refused", len(records))
	}
	if record, _ := records[0].(map[string]any); record["error_code"] != "model_not_allowed" {
		t.Errorf("failed record = %v, want the refused request", record)
	}
	if retention, _ := all.body["retention"].(map[string]any); retention["enabled"] != true {
		t.Errorf("retention = %v, want the log reported as enabled", all.body["retention"])
	}

	cleared := callManagement(server, http.MethodDelete, "/management/requests", "management-secret", nil)
	if cleared.status != http.StatusOK {
		t.Fatalf("clear status = %d, body = %v", cleared.status, cleared.body)
	}
	after := callManagement(server, http.MethodGet, "/management/requests", "management-secret", nil)
	if records, _ := after.body["requests"].([]any); len(records) != 0 {
		t.Errorf("records after clearing = %d, want none", len(records))
	}
}

// The console reads the log one page at a time, so a read answers with the slice
// it returned and the size of the whole filtered log.
func TestRequestLogPagesAndCounts(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"chatcmpl-test"}`))
	}))
	defer upstream.Close()

	server, _ := requestLogServer(t, denyingAuthenticator("denied-model"), domain.Channel{
		ID: "line-one", Name: "relay", BaseURL: upstream.URL, Enabled: true,
	})
	for index := 0; index < 5; index++ {
		response := proxyRequest(server, `{"model":"gpt-4.1"}`)
		response.Body.Close()
	}

	first := callManagement(server, http.MethodGet, "/management/requests?limit=2", "management-secret", nil)
	firstPage, _ := first.body["requests"].([]any)
	if len(firstPage) != 2 {
		t.Fatalf("page 1 records = %d, want the requested page size", len(firstPage))
	}
	if total, _ := first.body["total"].(float64); total != 5 {
		t.Errorf("total = %v, want every record counted", first.body["total"])
	}
	if offset, _ := first.body["offset"].(float64); offset != 0 {
		t.Errorf("offset = %v, want the first page", first.body["offset"])
	}

	second := callManagement(server, http.MethodGet, "/management/requests?limit=2&offset=2", "management-secret", nil)
	secondPage, _ := second.body["requests"].([]any)
	if len(secondPage) != 2 {
		t.Fatalf("page 2 records = %d, want the requested page size", len(secondPage))
	}
	// Pages do not overlap: the second page holds different requests.
	newest, _ := firstPage[0].(map[string]any)
	nextNewest, _ := secondPage[0].(map[string]any)
	if newest["id"] == nextNewest["id"] {
		t.Errorf("page 2 starts at the record page 1 started at: %v", nextNewest)
	}

	// A filter narrows the total as well as the page, so the view can say how many
	// pages the filtered log has.
	refused := callManagement(server, http.MethodGet, "/management/requests?failed=1", "management-secret", nil)
	if total, _ := refused.body["total"].(float64); total != 0 {
		t.Errorf("failed total = %v, want none for a log of served requests", refused.body["total"])
	}
}

// A gateway that keeps no log answers the view with a reason rather than an empty
// list, so the console can say why there is nothing to show.
func TestRequestLogUnavailableWithoutARecorder(t *testing.T) {
	server := &Server{ManagementToken: "management-secret"}
	response := callManagement(server, http.MethodGet, "/management/requests", "management-secret", nil)
	if response.status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", response.status)
	}
	if response.body["error"] == nil {
		t.Fatalf("body = %v, want an error payload", response.body)
	}
}
