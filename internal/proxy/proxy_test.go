package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/yhw5231/fluxgate/internal/domain"
	"github.com/yhw5231/fluxgate/internal/router"
)

type recordingObserver struct {
	mu        sync.Mutex
	failures  []domain.Failure
	successes []domain.Attempt
}

func (o *recordingObserver) RecordFailure(failure domain.Failure) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.failures = append(o.failures, failure)
}

func (o *recordingObserver) RecordSuccess(attempt domain.Attempt) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.successes = append(o.successes, attempt)
}

func noSleep(context.Context, time.Duration) error {
	return nil
}

// routeFor wraps channels in one catch-all route. Model mapping lives on the
// route, so tests exercising mapping declare it here.
func routeFor(mapping domain.ModelMapping, channels ...domain.Channel) []domain.Route {
	return []domain.Route{{
		ID:           1,
		ModelPattern: "*",
		Mode:         domain.RouteModePattern,
		Enabled:      true,
		ModelMapping: mapping,
		Channels:     channels,
	}}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

func TestEngineRetriesSameChannelThenFailsOver(t *testing.T) {
	var firstCalls int
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		firstCalls++
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"temporary"}`))
	}))
	defer first.Close()

	var secondCalls int
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondCalls++
		if got := r.Header.Get("Authorization"); got != "Bearer second-key" {
			t.Errorf("Authorization = %q, want second channel credential", got)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		if got := body["model"]; got != "mapped-model" {
			t.Errorf("model = %#v, want mapped-model", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer second.Close()

	selector := router.NewMemorySelector(routeFor(
		domain.ModelMapping{{Pattern: "gpt-*", Target: "mapped-model"}},
		domain.Channel{
			ID:       "first",
			BaseURL:  first.URL,
			Enabled:  true,
			Priority: 20,
			Weight:   1,
		},
		domain.Channel{
			ID:       "second",
			BaseURL:  second.URL,
			APIKey:   "second-key",
			Enabled:  true,
			Priority: 10,
			Weight:   1,
		},
	))
	observer := &recordingObserver{}
	engine := Engine{
		Selector: selector,
		Observer: observer,
		Client:   &http.Client{},
		Policy: domain.RetryPolicy{
			MaxAttempts:           4,
			MaxAttemptsPerChannel: 2,
			RetryStatuses:         map[int]struct{}{http.StatusServiceUnavailable: {}},
		},
		Sleep: noSleep,
	}

	result, err := engine.Forward(context.Background(), domain.Request{
		Method:  http.MethodPost,
		Path:    "/v1/chat/completions",
		Headers: http.Header{"Authorization": []string{"Bearer downstream"}},
		Body:    []byte(`{"model":"gpt-4.1","messages":[]}`),
		Model:   "gpt-4.1",
	})
	if err != nil {
		t.Fatalf("Forward() error = %v", err)
	}
	defer result.Response.Body.Close()

	if result.Attempt.Number != 3 {
		t.Fatalf("successful attempt number = %d, want 3", result.Attempt.Number)
	}
	if result.Attempt.ChannelID != "second" {
		t.Fatalf("successful channel = %q, want second", result.Attempt.ChannelID)
	}
	if firstCalls != 2 {
		t.Fatalf("first channel calls = %d, want 2", firstCalls)
	}
	if secondCalls != 1 {
		t.Fatalf("second channel calls = %d, want 1", secondCalls)
	}
	responseBody, err := io.ReadAll(result.Response.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	if string(responseBody) != `{"ok":true}` {
		t.Fatalf("response body = %s, want final successful response", responseBody)
	}
	if len(observer.failures) != 2 {
		t.Fatalf("recorded failures = %d, want 2", len(observer.failures))
	}
	if len(observer.successes) != 1 {
		t.Fatalf("recorded successes = %d, want 1", len(observer.successes))
	}
}

func TestEngineReturnsNonRetryableResponseWithoutFailover(t *testing.T) {
	var firstCalls int
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		firstCalls++
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"bad request"}`))
	}))
	defer first.Close()

	var secondCalls int
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		secondCalls++
		w.WriteHeader(http.StatusOK)
	}))
	defer second.Close()

	engine := Engine{
		Selector: router.NewMemorySelector(routeFor(nil,
			domain.Channel{ID: "first", BaseURL: first.URL, Enabled: true, Priority: 20, Weight: 1},
			domain.Channel{ID: "second", BaseURL: second.URL, Enabled: true, Priority: 10, Weight: 1},
		)),
		Policy: domain.RetryPolicy{
			MaxAttempts:           4,
			MaxAttemptsPerChannel: 1,
			RetryStatuses:         map[int]struct{}{http.StatusServiceUnavailable: {}},
		},
		Sleep: noSleep,
	}

	result, err := engine.Forward(context.Background(), domain.Request{
		Method: http.MethodPost,
		Path:   "/v1/responses",
		Body:   []byte(`{"model":"gpt-4.1"}`),
		Model:  "gpt-4.1",
	})
	if err != nil {
		t.Fatalf("Forward() error = %v", err)
	}
	defer result.Response.Body.Close()

	if result.Response.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", result.Response.StatusCode)
	}
	if firstCalls != 1 || secondCalls != 0 {
		t.Fatalf("channel calls = first:%d second:%d, want 1 and 0", firstCalls, secondCalls)
	}
}

func TestEngineStopsAtGlobalAttemptLimit(t *testing.T) {
	var calls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer upstream.Close()

	engine := Engine{
		Selector: router.NewMemorySelector(routeFor(nil,
			domain.Channel{ID: "only", BaseURL: upstream.URL, Enabled: true, Priority: 1, Weight: 1},
		)),
		Policy: domain.RetryPolicy{
			MaxAttempts:           3,
			MaxAttemptsPerChannel: 5,
			RetryStatuses:         map[int]struct{}{http.StatusTooManyRequests: {}},
		},
		Sleep: noSleep,
	}

	_, err := engine.Forward(context.Background(), domain.Request{
		Method: http.MethodPost,
		Path:   "/v1/messages",
		Body:   []byte(`{"model":"claude"}`),
		Model:  "claude",
	})
	if err == nil {
		t.Fatal("Forward() error = nil, want attempt-limit failure")
	}
	if calls != 3 {
		t.Fatalf("upstream calls = %d, want 3", calls)
	}
}

func TestEngineAppliesChannelRequestTimeout(t *testing.T) {
	requestCanceled := make(chan struct{}, 1)
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		<-request.Context().Done()
		requestCanceled <- struct{}{}
		return nil, request.Context().Err()
	})}

	engine := Engine{
		Selector: router.NewMemorySelector(routeFor(nil, domain.Channel{
			ID:             "slow",
			BaseURL:        "http://upstream.test",
			Enabled:        true,
			Weight:         1,
			RequestTimeout: 25 * time.Millisecond,
		})),
		Client: client,
		Policy: domain.RetryPolicy{MaxAttempts: 1, MaxAttemptsPerChannel: 1},
		Sleep:  noSleep,
	}

	_, err := engine.Forward(context.Background(), domain.Request{
		Method: http.MethodPost,
		Path:   "/v1/chat/completions",
		Body:   []byte(`{"model":"gpt-4.1"}`),
		Model:  "gpt-4.1",
	})
	if err == nil {
		t.Fatal("Forward() error = nil, want request timeout")
	}

	select {
	case <-requestCanceled:
	case <-time.After(time.Second):
		t.Fatal("upstream request context was not canceled by the channel timeout")
	}
}

// channelServer answers with a status and records how many times it was called.
func channelServer(t *testing.T, status int, calls *int) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*calls++
		w.WriteHeader(status)
		if status < http.StatusMultipleChoices {
			_, _ = w.Write([]byte(`{"ok":true}`))
		} else {
			_, _ = w.Write([]byte(`{"error":"failed"}`))
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// Switching failover off keeps a request on the line it started with, which is
// what makes a single upstream's behaviour observable.
func TestEngineWithoutFailoverRetriesTheSameChannel(t *testing.T) {
	var firstCalls, secondCalls int
	first := channelServer(t, http.StatusServiceUnavailable, &firstCalls)
	second := channelServer(t, http.StatusOK, &secondCalls)

	engine := &Engine{
		Selector: router.NewMemorySelector(routeFor(nil,
			domain.Channel{ID: "first", Enabled: true, Weight: 10, BaseURL: first.URL, APIKey: "first-key"},
			domain.Channel{ID: "second", Enabled: true, Weight: 10, BaseURL: second.URL, APIKey: "second-key"},
		)),
		Policy: domain.RetryPolicy{
			MaxAttempts:           4,
			MaxAttemptsPerChannel: 1,
			RetryStatuses:         map[int]struct{}{503: {}},
		},
		Sleep: noSleep,
	}
	engine.SetPolicy(DispatchPolicy{
		Retry:    engine.Policy,
		Failover: domain.FailoverPolicy{Enabled: false},
	})

	if _, err := engine.Forward(context.Background(), domain.Request{Method: http.MethodPost, Path: "/v1/chat/completions", Body: []byte(`{"model":"m"}`), Model: "m"}); err == nil {
		t.Fatal("Forward() succeeded, want the failing upstream reported")
	}
	if firstCalls != 4 {
		t.Errorf("first channel calls = %d, want 4 attempts on the same channel", firstCalls)
	}
	if secondCalls != 0 {
		t.Errorf("second channel calls = %d, want none while failover is off", secondCalls)
	}
}

// Failover limited to one upstream moves between its keys but never reaches for
// a different upstream.
func TestEngineCrossUpstreamFailoverCanBeLimitedToOneUpstream(t *testing.T) {
	var sameSiteCalls, otherSiteCalls int
	first := channelServer(t, http.StatusServiceUnavailable, &sameSiteCalls)
	other := channelServer(t, http.StatusOK, &otherSiteCalls)

	engine := &Engine{
		Selector: router.NewMemorySelector(routeFor(nil,
			domain.Channel{ID: "first", Enabled: true, Weight: 10, SiteID: 1, BaseURL: first.URL, APIKey: "first-key"},
			domain.Channel{ID: "other", Enabled: true, Weight: 10, SiteID: 2, BaseURL: other.URL, APIKey: "other-key"},
		)),
		Policy: domain.RetryPolicy{
			MaxAttempts:           4,
			MaxAttemptsPerChannel: 1,
			RetryStatuses:         map[int]struct{}{503: {}},
		},
		Sleep: noSleep,
	}
	engine.SetPolicy(DispatchPolicy{
		Retry:    engine.Policy,
		Failover: domain.FailoverPolicy{Enabled: true, CrossUpstream: false},
	})

	if _, err := engine.Forward(context.Background(), domain.Request{Method: http.MethodPost, Path: "/v1/chat/completions", Body: []byte(`{"model":"m"}`), Model: "m"}); err == nil {
		t.Fatal("Forward() succeeded, want the failing upstream reported")
	}
	if otherSiteCalls != 0 {
		t.Errorf("the other upstream was called %d times, want none", otherSiteCalls)
	}
	if sameSiteCalls == 0 {
		t.Error("the request never reached the upstream it started on")
	}
}

// A retry or failover change made while the gateway runs has to reach the next
// request, without a restart.
func TestEnginePolicyCanBeReplacedAtRuntime(t *testing.T) {
	var calls int
	server := channelServer(t, http.StatusServiceUnavailable, &calls)
	engine := &Engine{
		Selector: router.NewMemorySelector(routeFor(nil,
			domain.Channel{ID: "only", Enabled: true, Weight: 10, BaseURL: server.URL, APIKey: "key"},
		)),
		Policy: domain.RetryPolicy{
			MaxAttempts:           1,
			MaxAttemptsPerChannel: 1,
			RetryStatuses:         map[int]struct{}{503: {}},
		},
		Sleep: noSleep,
	}

	if _, err := engine.Forward(context.Background(), domain.Request{Method: http.MethodPost, Path: "/v1/chat/completions", Body: []byte(`{"model":"m"}`), Model: "m"}); err == nil {
		t.Fatal("Forward() succeeded, want the failure reported")
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want the configured single attempt", calls)
	}

	engine.SetPolicy(DispatchPolicy{
		Retry: domain.RetryPolicy{
			MaxAttempts:           3,
			MaxAttemptsPerChannel: 3,
			RetryStatuses:         map[int]struct{}{503: {}},
		},
		Failover: domain.FailoverPolicy{Enabled: false},
	})
	if _, err := engine.Forward(context.Background(), domain.Request{Method: http.MethodPost, Path: "/v1/chat/completions", Body: []byte(`{"model":"m"}`), Model: "m"}); err == nil {
		t.Fatal("Forward() succeeded, want the failure reported")
	}
	if calls != 4 {
		t.Errorf("calls = %d, want three more attempts under the installed policy", calls)
	}
}
