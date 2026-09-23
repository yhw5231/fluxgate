package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/yhw5231/fluxgate/internal/breaker"
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
		// The failing line belongs to the preferred upstream, so the request
		// starts on it rather than on whichever of the two was drawn.
		domain.Channel{
			ID:           "first",
			BaseURL:      first.URL,
			Enabled:      true,
			SiteID:       1,
			SitePriority: 20,
			Weight:       1,
		},
		domain.Channel{
			ID:           "second",
			BaseURL:      second.URL,
			APIKey:       "second-key",
			Enabled:      true,
			SiteID:       2,
			SitePriority: 10,
			Weight:       1,
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

// A failing key is left for the next key of the upstream the request is already
// on, before any other upstream is considered: the upstream is chosen first, and
// which of its keys answers is the upstream's own business.
func TestEngineFailoverWalksTheKeysOfTheChosenUpstream(t *testing.T) {
	var firstKeyCalls, secondKeyCalls, otherUpstreamCalls int
	firstKey := channelServer(t, http.StatusServiceUnavailable, &firstKeyCalls)
	secondKey := channelServer(t, http.StatusOK, &secondKeyCalls)
	otherUpstream := channelServer(t, http.StatusOK, &otherUpstreamCalls)

	engine := &Engine{
		Selector: router.NewMemorySelector(routeFor(nil,
			// One upstream with two keys, and a second upstream of lower priority
			// that the request should never need.
			domain.Channel{ID: "key-1", Enabled: true, Weight: 10, SiteID: 1, SitePriority: 10, RoutingStrategy: "stable_first", BaseURL: firstKey.URL, APIKey: "key-1"},
			domain.Channel{ID: "key-2", Enabled: true, Weight: 10, SiteID: 1, SitePriority: 10, RoutingStrategy: "stable_first", BaseURL: secondKey.URL, APIKey: "key-2"},
			domain.Channel{ID: "other", Enabled: true, Weight: 10, SiteID: 2, BaseURL: otherUpstream.URL, APIKey: "other"},
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
		Path:   "/v1/chat/completions",
		Body:   []byte(`{"model":"m"}`),
		Model:  "m",
	})
	if err != nil {
		t.Fatalf("Forward() error = %v", err)
	}
	defer result.Response.Body.Close()

	if result.Attempt.ChannelID != "key-2" {
		t.Fatalf("successful channel = %q, want the upstream's second key", result.Attempt.ChannelID)
	}
	if len(result.Trace.Attempts) != 2 || result.Trace.Attempts[0].ChannelID != "key-1" {
		t.Fatalf("attempts = %#v, want key-1 then key-2", result.Trace.Attempts)
	}
	if firstKeyCalls != 1 || secondKeyCalls != 1 {
		t.Fatalf("key calls = %d and %d, want one each", firstKeyCalls, secondKeyCalls)
	}
	if otherUpstreamCalls != 0 {
		t.Fatalf("the lower-priority upstream was called %d times, want none", otherUpstreamCalls)
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
			// Two upstreams, the first one preferred: the request has to reach it
			// for the test to say anything about a non-retryable answer.
			domain.Channel{ID: "first", BaseURL: first.URL, Enabled: true, SiteID: 1, SitePriority: 20, Weight: 1},
			domain.Channel{ID: "second", BaseURL: second.URL, Enabled: true, SiteID: 2, SitePriority: 10, Weight: 1},
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
			// The failing line belongs to the preferred upstream, so which line the
			// request starts on is decided by upstream priority rather than by the
			// draw between two upstreams of one priority.
			domain.Channel{ID: "first", Enabled: true, Weight: 10, SiteID: 1, SitePriority: 20, BaseURL: first.URL, APIKey: "first-key"},
			domain.Channel{ID: "second", Enabled: true, Weight: 10, SiteID: 2, SitePriority: 10, BaseURL: second.URL, APIKey: "second-key"},
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
			// Site 1 is the preferred upstream, so the request always starts on
			// the failing line rather than on whichever site was drawn.
			domain.Channel{ID: "first", Enabled: true, Weight: 10, SiteID: 1, SitePriority: 10, BaseURL: first.URL, APIKey: "first-key"},
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

// A line that keeps failing is held out of rotation, and the circuit that holds
// it out is the one its failures were filed under.
//
// The line is asked for a model of its own, so its circuit is filed under that
// name while the client asks for the mapped one. Selection therefore has to ask
// the breaker about the name the line is asked for: asked about the requested
// one it looks up a circuit that is never written, and a line that answers 503
// to everything is walked on every request forever while its recorded cooldown
// keeps escalating unseen.
func TestEngineHoldsOutAMappedLineThatKeepsFailing(t *testing.T) {
	var deadCalls, liveCalls int
	dead := channelServer(t, http.StatusServiceUnavailable, &deadCalls)
	live := channelServer(t, http.StatusOK, &liveCalls)

	circuit := &breaker.Breaker{
		Policy: breaker.Policy{
			Mode:         breaker.ModeKeyModelCooldown,
			Threshold:    2,
			BaseCooldown: time.Minute,
			MaxCooldown:  time.Minute,
			Multiplier:   1,
		},
		Store: breaker.NewMemoryStore(),
	}
	engine := &Engine{
		Selector: router.NewMemorySelectorWithFilter(routeFor(
			domain.ModelMapping{{Pattern: "gpt-*", Target: "mapped-model"}},
			// The failing line belongs to the preferred upstream, so every request
			// starts on it until its circuit holds it out.
			domain.Channel{
				ID: "dead", Name: "workbuddy", BaseURL: dead.URL, APIKey: "dead-key",
				Enabled: true, Weight: 1, SiteID: 1, SitePriority: 20,
				RoutingStrategy: "round_robin", BreakerMode: "key_model_cooldown",
			},
			domain.Channel{
				ID: "live", Name: "unigate", BaseURL: live.URL, APIKey: "live-key",
				Enabled: true, Weight: 1, SiteID: 2, SitePriority: 10,
				RoutingStrategy: "round_robin", BreakerMode: "key_model_cooldown",
			},
		), circuit),
		Observer: circuit,
		Client:   &http.Client{},
		Policy: domain.RetryPolicy{
			MaxAttempts:           4,
			MaxAttemptsPerChannel: 1,
			RetryStatuses:         map[int]struct{}{http.StatusServiceUnavailable: {}},
		},
		Sleep: noSleep,
	}

	forward := func(requestID string) Result {
		t.Helper()
		result, err := engine.Forward(context.Background(), domain.Request{
			Method:    http.MethodPost,
			Path:      "/v1/chat/completions",
			Body:      []byte(`{"model":"gpt-4.1","messages":[]}`),
			Model:     "gpt-4.1",
			RequestID: requestID,
		})
		if err != nil {
			t.Fatalf("Forward(%s) error = %v", requestID, err)
		}
		result.Response.Body.Close()
		return result
	}

	// Two requests that fail on the line are what its threshold counts, and both
	// of them fall through to the healthy upstream.
	for request := 1; request <= 2; request++ {
		result := forward(fmt.Sprintf("req-%d", request))
		if len(result.Trace.Attempts) != 2 {
			t.Fatalf("request %d attempts = %d, want the failing line and then the healthy one", request, len(result.Trace.Attempts))
		}
		if result.Attempt.ChannelID != "live" {
			t.Fatalf("request %d was served by %q, want the healthy upstream", request, result.Attempt.ChannelID)
		}
	}

	// The third request never reaches the failed line: it is cooling down.
	result := forward("req-3")
	if len(result.Trace.Attempts) != 1 {
		t.Fatalf("attempts after the circuit tripped = %d, want only the healthy upstream", len(result.Trace.Attempts))
	}
	if result.Attempt.ChannelID != "live" {
		t.Fatalf("channel after the circuit tripped = %q, want live", result.Attempt.ChannelID)
	}
	if deadCalls != 2 {
		t.Errorf("the failed upstream was called %d times, want no call once its line is cooling", deadCalls)
	}
	if liveCalls != 3 {
		t.Errorf("the healthy upstream was called %d times, want once per request", liveCalls)
	}

	// The circuit is filed under the model the line was asked for, not the one the
	// client asked for.
	if _, known := circuit.Store.Load(breaker.Scope{KeyID: "dead-key", Model: "mapped-model"}); !known {
		t.Error("the failing line's circuit is not filed under the model it was asked for")
	}
	if !circuit.IsBlocked(domain.Channel{ID: "dead", APIKey: "dead-key", BreakerMode: "key_model_cooldown"}, "mapped-model") {
		t.Error("the failing line is not held out of rotation")
	}
}

// The trace of a request is the path it really walked: only the lines that were
// dispatched to leave an entry, so a line the router skips — held out of rotation
// as a whole, or for the model it would be asked for — is not part of the path and
// not counted among the attempts. A cooling line's own name carrying the failure
// that put it there is what an operator reads the path for, and a line that was
// never asked cannot explain anything.
func TestEngineTracesOnlyTheLinesItAsked(t *testing.T) {
	cooling := []struct {
		name  string
		mode  string
		scope breaker.Scope
	}{
		{"whole line cooling", "key_cooldown", breaker.Scope{KeyID: "dead-key"}},
		{"line cooling for the model it is asked for", "key_model_cooldown", breaker.Scope{KeyID: "dead-key", Model: "mapped-model"}},
	}

	for _, scenario := range cooling {
		t.Run(scenario.name, func(t *testing.T) {
			var deadCalls, liveCalls int
			dead := channelServer(t, http.StatusServiceUnavailable, &deadCalls)
			live := channelServer(t, http.StatusOK, &liveCalls)

			store := breaker.NewMemoryStore()
			store.Update(scenario.scope, func(breaker.State) breaker.State {
				return breaker.State{ConsecutiveFailures: 3, CooldownLevel: 1, BlockedUntil: time.Now().Add(time.Minute)}
			})
			circuit := &breaker.Breaker{
				Policy: breaker.Policy{
					Mode:         breaker.Mode(scenario.mode),
					Threshold:    3,
					BaseCooldown: time.Minute,
					MaxCooldown:  time.Minute,
					Multiplier:   1,
				},
				Store: store,
			}
			engine := &Engine{
				Selector: router.NewMemorySelectorWithFilter(routeFor(
					domain.ModelMapping{{Pattern: "gpt-*", Target: "mapped-model"}},
					domain.Channel{
						ID: "dead", Name: "workbuddy", BaseURL: dead.URL, APIKey: "dead-key",
						Enabled: true, Weight: 1, SiteID: 1, SitePriority: 20,
						RoutingStrategy: "round_robin", BreakerMode: scenario.mode,
					},
					domain.Channel{
						ID: "live", Name: "unigate", BaseURL: live.URL, APIKey: "live-key",
						Enabled: true, Weight: 1, SiteID: 2, SitePriority: 10,
						RoutingStrategy: "round_robin", BreakerMode: scenario.mode,
					},
				), circuit),
				Observer: circuit,
				Client:   &http.Client{},
				Policy: domain.RetryPolicy{
					MaxAttempts:           4,
					MaxAttemptsPerChannel: 1,
					RetryStatuses:         map[int]struct{}{http.StatusServiceUnavailable: {}},
				},
				Sleep: noSleep,
			}

			result, err := engine.Forward(context.Background(), domain.Request{
				Method:    http.MethodPost,
				Path:      "/v1/chat/completions",
				Body:      []byte(`{"model":"gpt-4.1","messages":[]}`),
				Model:     "gpt-4.1",
				RequestID: "req-1",
			})
			if err != nil {
				t.Fatalf("Forward() error = %v", err)
			}
			defer result.Response.Body.Close()

			if len(result.Trace.Attempts) != 1 {
				t.Fatalf("traced attempts = %d, want only the line that answered: %#v", len(result.Trace.Attempts), result.Trace.Attempts)
			}
			attempt := result.Trace.Attempts[0]
			if attempt.ChannelID != "live" || attempt.ChannelName != "unigate" {
				t.Errorf("traced line = %s/%s, want the line that answered", attempt.ChannelID, attempt.ChannelName)
			}
			// Attempts are numbered by what was dispatched, so the first line a
			// request walks is always attempt 1 — a skipped line takes no number.
			if attempt.Number != 1 {
				t.Errorf("traced attempt number = %d, want 1", attempt.Number)
			}
			if deadCalls != 0 {
				t.Errorf("the cooling upstream was called %d times, want none", deadCalls)
			}
			if liveCalls != 1 {
				t.Errorf("the answering upstream was called %d times, want once", liveCalls)
			}
		})
	}
}
