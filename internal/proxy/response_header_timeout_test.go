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

	"github.com/yhw5231/fluxgate/internal/config"
	"github.com/yhw5231/fluxgate/internal/domain"
	"github.com/yhw5231/fluxgate/internal/router"
)

// Use real HTTP/1.1 and HTTP/2 with delayed headers, through the proxy pool.
func TestConfiguredResponseHeaderTimeout(t *testing.T) {
	for _, http2 := range []bool{false, true} {
		protocol := "http1"
		if http2 {
			protocol = "http2"
		}
		t.Run(protocol, func(t *testing.T) {
			for _, stream := range []bool{false, true} {
				name := "body"
				if stream {
					name = "stream"
				}
				t.Run(name, func(t *testing.T) {
					for _, test := range []struct {
						name    string
						header  string
						request string
					}{
						{"disabled", "0s", "2s"},
						{"header_bound", "50ms", "2s"},
						{"request_bound", "0s", "50ms"},
					} {
						t.Run(test.name, func(t *testing.T) {
							t.Setenv("FLUXGATE_RESPONSE_HEADER_TIMEOUT", test.header)
							t.Setenv("FLUXGATE_REQUEST_TIMEOUT", test.request)
							cfg, err := config.Load()
							if err != nil {
								t.Fatal(err)
							}
							seen := make(chan int, 1)
							payload := `{"choices":[]}`
							if stream {
								payload = "data:[DONE]\n\n"
							}
							upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
								seen <- r.ProtoMajor
								timer := time.NewTimer(150 * time.Millisecond)
								defer timer.Stop()
								select {
								case <-r.Context().Done():
									return
								case <-timer.C:
								}
								if stream {
									w.Header().Set("Content-Type", "text/event-stream")
								}
								_, _ = io.WriteString(w, payload)
							}))
							upstream.EnableHTTP2 = http2
							upstream.StartTLS()
							defer upstream.Close()
							base := upstream.Client().Transport.(*http.Transport).Clone()
							base.ResponseHeaderTimeout = cfg.ResponseHeaderTimeout
							defer base.CloseIdleConnections()
							pool := NewTransportPool(base)
							pooled, err := pool.Transport(ResolvedProxy{Source: ProxySourceKey})
							if err != nil {
								t.Fatal(err)
							}
							defer pooled.CloseIdleConnections()
							if pooled.ResponseHeaderTimeout != cfg.ResponseHeaderTimeout {
								t.Fatalf("pooled header timeout=%s,want=%s", pooled.ResponseHeaderTimeout, cfg.ResponseHeaderTimeout)
							}
							engine := Engine{
								Selector: router.NewMemorySelector(routeFor(nil, domain.Channel{
									ID: "slow-headers", BaseURL: upstream.URL, Enabled: true, APIKey: "test-key", RequestTimeout: cfg.RequestTimeout,
								})),
								ProxyResolver: &Resolver{Config: ProxyConfig{Keys: map[string]string{"test-key": "direct"}}},
								TransportPool: pool, ResponseHeaderTimeout: cfg.ResponseHeaderTimeout, StreamIdleTimeout: cfg.StreamIdleTimeout,
								Policy: domain.RetryPolicy{MaxAttempts: 1, MaxAttemptsPerChannel: 1},
							}
							ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
							defer cancel()
							result, err := engine.Forward(ctx, domain.Request{
								Method: http.MethodPost, Path: "/v1/chat/completions", ContentType: "application/json", Model: "m", Stream: stream, Body: []byte(`{"model":"m"}`),
							})
							if result.Response != nil {
								defer result.Response.Body.Close()
							}
							switch test.name {
							case "header_bound":
								if err == nil || !strings.Contains(err.Error(), "awaiting") {
									t.Fatalf("Forward()=%v,want transport header timeout", err)
								}
							case "request_bound":
								want := context.DeadlineExceeded
								if stream {
									want = ErrStreamSilent
								}
								if !errors.Is(err, want) {
									t.Fatalf("Forward()=%v,want=%v", err, want)
								}
							default:
								if err != nil {
									t.Fatalf("Forward()=%v,want delayed headers to succeed", err)
								}
								body, readErr := io.ReadAll(result.Response.Body)
								if readErr != nil || string(body) != payload {
									t.Fatalf("body=%q,error=%v,want=%q", body, readErr, payload)
								}
							}
							wantMajor := 1
							if http2 {
								wantMajor = 2
							}
							select {
							case major := <-seen:
								if major != wantMajor {
									t.Fatalf("HTTP major=%d,want=%d", major, wantMajor)
								}
							default:
								t.Fatal("upstream never received the request")
							}
						})
					}
				})
			}
		})
	}
}
