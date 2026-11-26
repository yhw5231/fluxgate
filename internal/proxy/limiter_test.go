package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/yhw5231/fluxgate/internal/concurrency"
	"github.com/yhw5231/fluxgate/internal/domain"
	"github.com/yhw5231/fluxgate/internal/router"
)

// limitedRequest is one chat completion, which is what every test here sends.
func limitedRequest() domain.Request {
	return domain.Request{
		Method: http.MethodPost,
		Path:   "/v1/chat/completions",
		Body:   []byte(`{"model":"gpt-4.1","messages":[]}`),
		Model:  "gpt-4.1",
	}
}

// answeringServer answers every request with a JSON body after a short while, and
// reports how many requests it was serving at once.
type answeringServer struct {
	*httptest.Server
	mu       sync.Mutex
	busy     int
	maxBusy  int
	requests int
}

func newAnsweringServer(t *testing.T, delay time.Duration) *answeringServer {
	t.Helper()
	server := &answeringServer{}
	server.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		server.mu.Lock()
		server.requests++
		server.busy++
		if server.busy > server.maxBusy {
			server.maxBusy = server.busy
		}
		server.mu.Unlock()

		time.Sleep(delay)

		server.mu.Lock()
		server.busy--
		server.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(server.Close)
	return server
}

func (s *answeringServer) peak() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.maxBusy
}

// A key configured with a concurrency of one serves one request at a time, even
// when several callers ask at once: the rest wait for the slot rather than failing
// or reaching the upstream beside it.
func TestEngineServesOneRequestAtATimeThroughAKey(t *testing.T) {
	upstream := newAnsweringServer(t, 20*time.Millisecond)
	limiter := concurrency.NewLimiter()
	selector := router.NewMemorySelector(routeFor(nil, domain.Channel{
		ID: "line", BaseURL: upstream.URL, APIKey: "key", Enabled: true,
		SiteID: 1, Weight: 1, KeyConcurrency: 1,
	}))
	selector.SetKeyLimiter(limiter)
	engine := Engine{Selector: selector, Client: &http.Client{}, Limiter: limiter}

	var wait sync.WaitGroup
	for index := 0; index < 4; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			result, err := engine.Forward(context.Background(), limitedRequest())
			if err != nil {
				t.Errorf("Forward() error = %v", err)
				return
			}
			_, _ = io.Copy(io.Discard, result.Response.Body)
			_ = result.Response.Body.Close()
		}()
	}
	wait.Wait()

	if got := upstream.peak(); got != 1 {
		t.Fatalf("upstream served %d requests at once, want 1 for a key of concurrency 1", got)
	}
	if upstream.requests != 4 {
		t.Fatalf("upstream served %d requests, want all 4", upstream.requests)
	}
}

// The slot belongs to the key for as long as the answer lasts, not until its
// headers arrive: a streamed answer still has the upstream working.
func TestEngineHoldsTheKeyUntilItsAnswerIsClosed(t *testing.T) {
	upstream := newAnsweringServer(t, 0)
	limiter := concurrency.NewLimiter()
	channel := domain.Channel{
		ID: "line", BaseURL: upstream.URL, APIKey: "key", Enabled: true,
		SiteID: 1, Weight: 1, KeyConcurrency: 1,
	}
	selector := router.NewMemorySelector(routeFor(nil, channel))
	selector.SetKeyLimiter(limiter)
	engine := Engine{Selector: selector, Client: &http.Client{}, Limiter: limiter}

	first, err := engine.Forward(context.Background(), limitedRequest())
	if err != nil {
		t.Fatalf("Forward() error = %v", err)
	}
	if got := limiter.InFlight(channel); got != 1 {
		t.Fatalf("InFlight() = %d while the first answer is open, want 1", got)
	}

	// A second request is not served while the first answer is still open. Its own
	// context ends the wait, which is the only bound a queued request has.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := engine.Forward(ctx, limitedRequest()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Forward() error = %v, want the wait bounded by its context", err)
	}
	if upstream.requests != 1 {
		t.Fatalf("upstream served %d requests while the key was taken, want 1", upstream.requests)
	}

	// Closing the first answer hands the slot back, and the waiting work proceeds.
	_ = first.Response.Body.Close()
	if got := limiter.InFlight(channel); got != 0 {
		t.Fatalf("InFlight() = %d after the answer was closed, want 0", got)
	}
	second, err := engine.Forward(context.Background(), limitedRequest())
	if err != nil {
		t.Fatalf("Forward() after the key was released error = %v", err)
	}
	_ = second.Response.Body.Close()
}

// A retry on the same key needs the slot back first, or a key of concurrency one
// would wait for itself.
func TestEngineHandsTheSlotBackBeforeRetryingTheSameKey(t *testing.T) {
	var (
		mu    sync.Mutex
		calls int
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		calls++
		attempt := calls
		mu.Unlock()
		if attempt == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"temporary"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	limiter := concurrency.NewLimiter()
	channel := domain.Channel{
		ID: "line", BaseURL: upstream.URL, APIKey: "key", Enabled: true,
		SiteID: 1, Weight: 1, KeyConcurrency: 1,
	}
	selector := router.NewMemorySelector(routeFor(nil, channel))
	selector.SetKeyLimiter(limiter)
	engine := Engine{
		Selector: selector,
		Client:   &http.Client{},
		Limiter:  limiter,
		Policy: domain.RetryPolicy{
			MaxAttempts:           2,
			MaxAttemptsPerChannel: 2,
			RetryStatuses:         map[int]struct{}{http.StatusServiceUnavailable: {}},
		},
		Sleep: noSleep,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	result, err := engine.Forward(ctx, limitedRequest())
	if err != nil {
		t.Fatalf("Forward() error = %v, want the retry to be served", err)
	}
	defer result.Response.Body.Close()
	if result.Attempt.Number != 2 {
		t.Fatalf("successful attempt number = %d, want 2", result.Attempt.Number)
	}
	if got := limiter.InFlight(channel); got != 1 {
		t.Fatalf("InFlight() = %d while the answer is open, want the one request", got)
	}
}

// A request waiting for a slot belongs to its caller: when the caller goes away,
// the wait ends with it instead of being served for nobody.
func TestEngineStopsWaitingForASlotWhenTheCallerGoesAway(t *testing.T) {
	upstream := newAnsweringServer(t, 0)
	limiter := concurrency.NewLimiter()
	channel := domain.Channel{
		ID: "line", BaseURL: upstream.URL, APIKey: "key", Enabled: true,
		SiteID: 1, Weight: 1, KeyConcurrency: 1,
	}
	selector := router.NewMemorySelector(routeFor(nil, channel))
	selector.SetKeyLimiter(limiter)
	engine := Engine{Selector: selector, Client: &http.Client{}, Limiter: limiter}

	held, err := engine.Forward(context.Background(), limitedRequest())
	if err != nil {
		t.Fatalf("Forward() error = %v", err)
	}
	defer held.Response.Body.Close()

	ctx, cancel := context.WithCancel(context.Background())
	canceled := make(chan error, 1)
	go func() {
		_, err := engine.Forward(ctx, limitedRequest())
		canceled <- err
	}()
	// The wait has to be in progress before it is canceled, or the test proves
	// nothing about the wait.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-canceled:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Forward() error = %v, want the caller's cancellation", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Forward() kept waiting after its caller went away")
	}
	if upstream.requests != 1 {
		t.Fatalf("upstream served %d requests, want only the one that held the key", upstream.requests)
	}
}

// Two keys of one upstream are two allowances: a request is placed on the key that
// has room instead of queueing behind the busy one.
func TestEnginePlacesARequestOnAKeyWithRoom(t *testing.T) {
	busy := newAnsweringServer(t, 0)
	free := newAnsweringServer(t, 0)

	limiter := concurrency.NewLimiter()
	busyLine := domain.Channel{
		ID: "busy", BaseURL: busy.URL, APIKey: "first-key", Enabled: true,
		SiteID: 1, Weight: 1, KeyConcurrency: 1,
	}
	freeLine := domain.Channel{
		ID: "free", BaseURL: free.URL, APIKey: "second-key", Enabled: true,
		SiteID: 1, Weight: 1, KeyConcurrency: 1,
	}
	selector := router.NewMemorySelector(routeFor(nil, busyLine, freeLine))
	selector.SetKeyLimiter(limiter)
	engine := Engine{Selector: selector, Client: &http.Client{}, Limiter: limiter}

	// The first key is taken by work already in flight.
	release, ok := limiter.Acquire(busyLine)
	if !ok {
		t.Fatal("Acquire() = false on the free first key")
	}
	defer release()

	result, err := engine.Forward(context.Background(), limitedRequest())
	if err != nil {
		t.Fatalf("Forward() error = %v", err)
	}
	defer result.Response.Body.Close()
	if result.Attempt.ChannelID != "free" {
		t.Fatalf("attempt ran on line %q, want the key with room", result.Attempt.ChannelID)
	}
	if free.requests != 1 || busy.requests != 0 {
		t.Fatalf("upstreams served %d/%d requests, want the request on the free key only", busy.requests, free.requests)
	}
}
