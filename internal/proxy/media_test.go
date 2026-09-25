package proxy

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/yhw5231/fluxgate/internal/domain"
	"github.com/yhw5231/fluxgate/internal/router"
)

// A generation dispatched through a configured proxy still goes through the media
// pool, because the pool is what the connection settings come from: sending it
// down the ordinary pool would restore the response-header timeout the media pool
// exists to lift.
func TestMediaRequestsUseTheMediaTransportPool(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer upstream.Close()

	// The ordinary pool cannot reach anything, so a request that used it would fail
	// rather than quietly look like a success.
	unusable := http.DefaultTransport.(*http.Transport).Clone()
	unusable.DialContext = func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("the ordinary pool carried a generation")
	}
	resolver := &Resolver{SystemProxy: func(*http.Request) (*url.URL, error) { return nil, nil }}
	engine := &Engine{
		Selector: router.NewMemorySelector(testRouteWithChannel(domain.Channel{
			ID: "channel-one", BaseURL: upstream.URL, Enabled: true, SiteID: 1,
		})),
		Policy:             domain.RetryPolicy{MaxAttempts: 1, MaxAttemptsPerChannel: 1},
		Sleep:              func(context.Context, time.Duration) error { return nil },
		ProxyResolver:      resolver,
		TransportPool:      NewTransportPool(unusable),
		MediaTransportPool: NewTransportPool(nil),
	}

	media, err := engine.Forward(context.Background(), domain.Request{
		Method: http.MethodPost, Path: "/v1/images/generations",
		ContentType: "application/json", Body: []byte(`{"model":"gpt-image-1"}`),
		Model: "gpt-image-1", Media: true,
	})
	if err != nil {
		t.Fatalf("a generation did not use the media pool: %v", err)
	}
	defer media.Response.Body.Close()
	if media.Response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", media.Response.StatusCode)
	}

	// The other direction: an ordinary request stays on the ordinary pool.
	if _, err := engine.Forward(context.Background(), domain.Request{
		Method: http.MethodPost, Path: "/v1/chat/completions",
		ContentType: "application/json", Body: []byte(`{"model":"gpt-image-1"}`),
		Model: "gpt-image-1",
	}); err == nil {
		t.Fatal("an ordinary request did not use the ordinary pool")
	}
}

// testRouteWithChannel wraps one channel in a catch-all route.
func testRouteWithChannel(channel domain.Channel) []domain.Route {
	return []domain.Route{{
		ID:           1,
		ModelPattern: "*",
		Mode:         domain.RouteModePattern,
		Enabled:      true,
		Channels:     []domain.Channel{channel},
	}}
}
