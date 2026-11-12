package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yhw5231/fluxgate/internal/domain"
	"github.com/yhw5231/fluxgate/internal/router"
)

// streamEngine is an engine over one line whose request timeout is short and whose
// silence bound is configurable, so a test can tell the two bounds apart.
func streamEngine(t *testing.T, line domain.Channel, idle time.Duration) *Engine {
	t.Helper()
	return &Engine{
		Selector:          router.NewMemorySelector(routeFor(nil, line)),
		Client:            &http.Client{},
		StreamIdleTimeout: idle,
		Policy:            domain.RetryPolicy{MaxAttempts: 1, MaxAttemptsPerChannel: 1},
		Sleep:             noSleep,
	}
}

func streamRequest() domain.Request {
	return domain.Request{
		Method: http.MethodPost,
		Path:   "/v1/chat/completions",
		Body:   []byte(`{"model":"gpt-5","stream":true}`),
		Model:  "gpt-5",
		Stream: true,
	}
}

// A streamed answer is bounded by silence, not by the channel's request timeout.
// This answer takes longer than that timeout from start to finish while never going
// quiet, which is what a long generation looks like, and it has to be served
// instead of being canceled halfway through.
func TestStreamedAnswerOutlivesTheChannelRequestTimeout(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		for _, event := range []string{"one", "two", "three", "four"} {
			time.Sleep(40 * time.Millisecond)
			_, _ = io.WriteString(w, "data: "+event+"\n\n")
			w.(http.Flusher).Flush()
		}
	}))
	defer upstream.Close()

	// The whole answer takes well over the request timeout, which used to be the
	// bound on the request rather than on the wait for it to begin.
	engine := streamEngine(t, domain.Channel{
		ID: "slow-stream", BaseURL: upstream.URL, Enabled: true, Weight: 1,
		RequestTimeout: 20 * time.Millisecond,
	}, time.Second)

	result, err := engine.Forward(context.Background(), streamRequest())
	if err != nil {
		t.Fatalf("Forward() error = %v, want the long stream to be served", err)
	}
	defer result.Response.Body.Close()

	body, readErr := io.ReadAll(result.Response.Body)
	if readErr != nil {
		t.Fatalf("read stream = %v, want every event", readErr)
	}
	if got := string(body); got != "data: one\n\ndata: two\n\ndata: three\n\ndata: four\n\n" {
		t.Fatalf("stream = %q, want every event in order", got)
	}
}

// An upstream that stops sending is given up on, and the failure names silence as
// the cause rather than reporting a canceled context nobody can attribute.
func TestStreamedAnswerIsGivenUpWhenTheUpstreamGoesSilent(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		<-release
	}))
	defer func() {
		close(release)
		upstream.Close()
	}()

	engine := streamEngine(t, domain.Channel{
		ID: "silent-stream", BaseURL: upstream.URL, Enabled: true, Weight: 1,
		RequestTimeout: 5 * time.Second,
	}, 40*time.Millisecond)

	result, err := engine.Forward(context.Background(), streamRequest())
	if err != nil {
		t.Fatalf("Forward() error = %v, want the attempt to be accepted", err)
	}
	defer result.Response.Body.Close()

	started := time.Now()
	_, readErr := io.ReadAll(result.Response.Body)
	if !errors.Is(readErr, ErrStreamSilent) {
		t.Fatalf("read stream = %v, want ErrStreamSilent", readErr)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("the silent stream was given up after %s, want it bounded by the idle timeout", elapsed)
	}
}

// An upstream that never starts answering is failed on the request timeout, which
// is the one bound that still applies before the first byte.
func TestStreamedAnswerGivenUpWhenTheUpstreamNeverStarts(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		<-request.Context().Done()
		return nil, request.Context().Err()
	})}
	engine := &Engine{
		Selector: router.NewMemorySelector(routeFor(nil, domain.Channel{
			ID: "never-answers", BaseURL: "http://upstream.test", Enabled: true, Weight: 1,
			RequestTimeout: 25 * time.Millisecond,
		})),
		Client:            client,
		StreamIdleTimeout: 5 * time.Second,
		Policy:            domain.RetryPolicy{MaxAttempts: 1, MaxAttemptsPerChannel: 1},
		Sleep:             noSleep,
	}

	_, err := engine.Forward(context.Background(), streamRequest())
	if !errors.Is(err, ErrStreamSilent) {
		t.Fatalf("Forward() error = %v, want ErrStreamSilent", err)
	}
	if !strings.Contains(err.Error(), "did not start answering") {
		t.Fatalf("Forward() error = %v, want it to name the bound that expired", err)
	}
}

// A streamed attempt the caller cancels is not reported as a silent upstream: the
// caller's own context ends the request, and the failure keeps saying so.
func TestStreamedAnswerCanceledByTheCallerIsNotSilence(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-release
	}))
	defer func() {
		close(release)
		upstream.Close()
	}()

	engine := streamEngine(t, domain.Channel{
		ID: "canceled-stream", BaseURL: upstream.URL, Enabled: true, Weight: 1,
		RequestTimeout: 5 * time.Second,
	}, 5*time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	result, err := engine.Forward(ctx, streamRequest())
	if err != nil {
		t.Fatalf("Forward() error = %v, want the attempt to be accepted", err)
	}
	defer result.Response.Body.Close()

	cancel()
	_, readErr := io.ReadAll(result.Response.Body)
	if readErr == nil {
		t.Fatal("read stream = nil, want the canceled request to end it")
	}
	if errors.Is(readErr, ErrStreamSilent) {
		t.Fatalf("read stream = %v, want the caller's cancellation rather than a silent upstream", readErr)
	}
}
