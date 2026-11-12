package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yhw5231/fluxgate/internal/domain"
	"github.com/yhw5231/fluxgate/internal/proxy"
	"github.com/yhw5231/fluxgate/internal/router"
	"github.com/yhw5231/fluxgate/internal/store"
)

// captureRequestLog keeps what the gateway recorded, so a test can read the answer
// the console would show rather than only the one the client received.
type captureRequestLog struct {
	mu      sync.Mutex
	records []domain.RequestRecord
}

func (log *captureRequestLog) AppendRequestRecord(_ context.Context, record domain.RequestRecord, _ int) error {
	log.mu.Lock()
	defer log.mu.Unlock()
	log.records = append(log.records, record)
	return nil
}

func (log *captureRequestLog) ListRequestRecords(context.Context, store.RequestLogFilter) ([]domain.RequestRecord, error) {
	return nil, nil
}

func (log *captureRequestLog) CountRequestRecords(context.Context, store.RequestLogFilter) (int, error) {
	return 0, nil
}

func (log *captureRequestLog) ClearRequestRecords(context.Context) (int64, error) {
	return 0, nil
}

func (log *captureRequestLog) only(t *testing.T) domain.RequestRecord {
	t.Helper()
	log.mu.Lock()
	defer log.mu.Unlock()
	if len(log.records) != 1 {
		t.Fatalf("request records = %d, want 1", len(log.records))
	}
	return log.records[0]
}

// streamServer puts a proxy in front of an upstream that answers server-sent events,
// with the heartbeat set short enough for a test to observe it.
func streamServer(t *testing.T, upstream *httptest.Server, keepAlive time.Duration) *Server {
	t.Helper()
	engine := &proxy.Engine{
		Selector: router.NewMemorySelector(testRoutes(domain.Channel{
			ID: "stream", BaseURL: upstream.URL, Enabled: true, Weight: 1,
		})),
		Policy: domain.RetryPolicy{MaxAttempts: 1, MaxAttemptsPerChannel: 1},
	}
	return &Server{
		Engine:          engine,
		Authenticator:   allowTestAuthentication(),
		StreamKeepAlive: keepAlive,
	}
}

func streamRequestTo(t *testing.T, url string) *http.Response {
	t.Helper()
	var requestBody io.Reader = strings.NewReader(`{"model":"gpt-4.1","stream":true}`)
	request, err := http.NewRequest(http.MethodPost, url+"/v1/chat/completions", requestBody)
	if err != nil {
		t.Fatalf("create POST request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer test-key")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("POST error = %v", err)
	}
	return response
}

// A stream that goes quiet while the model is thinking is kept alive towards the
// client. Without it the caller — or a proxy in front of the gateway, whose own
// read timeout is commonly sixty seconds — decides the request has stalled and hangs
// up, which cancels the upstream's work and leaves it recording a canceled request
// it did nothing to cause.
func TestSilentEventStreamIsKeptAlive(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		// The pause a reasoning model takes before its first token.
		time.Sleep(200 * time.Millisecond)
		_, _ = w.Write([]byte("data: done\n\n"))
		w.(http.Flusher).Flush()
	}))
	defer upstream.Close()

	target := httptest.NewServer(streamServer(t, upstream, 25*time.Millisecond).Handler())
	defer target.Close()

	response := streamRequestTo(t, target.URL)
	defer response.Body.Close()

	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read stream = %v", err)
	}
	text := string(body)
	if !strings.Contains(text, sseHeartbeat) {
		t.Fatalf("stream = %q, want a heartbeat while the upstream was quiet", text)
	}
	if !strings.Contains(text, "data: done") {
		t.Fatalf("stream = %q, want the event that followed the pause", text)
	}
	// The heartbeat is written before the event it covers, never after it: a comment
	// that arrives once the answer is complete keeps nothing alive.
	if strings.Index(text, sseHeartbeat) > strings.Index(text, "data: done") {
		t.Fatalf("stream = %q, want the heartbeat before the event", text)
	}
}

// A streaming request that the upstream answers with a plain JSON body — an error,
// typically — is relayed byte for byte. A comment line inserted there would corrupt
// the answer the client has to parse.
func TestJSONAnswerToAStreamingRequestKeepsItsBytes(t *testing.T) {
	const answer = `{"error":{"message":"rate limited"}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		w.(http.Flusher).Flush()
		time.Sleep(150 * time.Millisecond)
		_, _ = w.Write([]byte(answer))
	}))
	defer upstream.Close()

	target := httptest.NewServer(streamServer(t, upstream, 25*time.Millisecond).Handler())
	defer target.Close()

	response := streamRequestTo(t, target.URL)
	defer response.Body.Close()

	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read body = %v", err)
	}
	if string(body) != answer {
		t.Fatalf("body = %q, want the upstream's own answer %q", body, answer)
	}
	if response.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want the upstream's %d", response.StatusCode, http.StatusTooManyRequests)
	}
}

// A caller that hangs up while the upstream is still working is recorded as the
// cause. The upstream request is canceled with the caller's, and the upstream then
// records a cancellation for work nobody is waiting for — a record that blames the
// upstream would send the operator looking for a fault that is not there.
func TestClientCancellationIsRecordedAsTheCause(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer upstream.Close()

	log := &captureRequestLog{}
	server := &Server{
		Engine: &proxy.Engine{
			Selector: router.NewMemorySelector(testRoutes(domain.Channel{
				ID: "channel", BaseURL: upstream.URL, Enabled: true, Weight: 1,
			})),
			Policy: domain.RetryPolicy{MaxAttempts: 1, MaxAttemptsPerChannel: 1},
		},
		Authenticator: allowTestAuthentication(),
		RequestLog:    log,
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4.1"}`)).WithContext(ctx)
	request.Header.Set("Authorization", "Bearer test-key")
	response := httptest.NewRecorder()

	server.Handler().ServeHTTP(response, request)

	record := log.only(t)
	if record.ErrorCode != "client_canceled" {
		t.Fatalf("record error code = %q, want client_canceled", record.ErrorCode)
	}
	if record.Status != statusClientClosed {
		t.Fatalf("record status = %d, want %d", record.Status, statusClientClosed)
	}
}
