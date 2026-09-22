package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/yhw5231/fluxgate/internal/domain"
	"github.com/yhw5231/fluxgate/internal/router"
)

// The base address of an upstream is the API root the console asks for, and it
// may carry a sub-path: an upstream whose API lives under /openai has to be
// called there. Resolving a request path against it must therefore keep that
// path rather than replace it with the request's own version segment.
func TestUpstreamTargetKeepsTheBasePath(t *testing.T) {
	cases := []struct {
		base     string
		path     string
		expected string
	}{
		{"https://api.example.com", "/v1/chat/completions", "https://api.example.com/v1/chat/completions"},
		{"https://api.example.com/v1", "/v1/chat/completions", "https://api.example.com/v1/chat/completions"},
		{"https://api.example.com/v1/", "/v1/chat/completions", "https://api.example.com/v1/chat/completions"},
		{"https://api.example.com/openai", "/v1/chat/completions", "https://api.example.com/openai/chat/completions"},
		{"https://api.example.com/openai/v1", "/v1/chat/completions", "https://api.example.com/openai/v1/chat/completions"},
		{"https://api.example.com/openai/v1/", "/v1/responses", "https://api.example.com/openai/v1/responses"},
		{"https://api.example.com/openai", "/v1/messages", "https://api.example.com/openai/messages"},
		// A path that carries no version segment is appended as it is.
		{"https://api.example.com/openai/v1", "/anything", "https://api.example.com/openai/v1/anything"},
		// A query the base address carries is part of where the request goes.
		{"https://api.example.com/v1?api-version=1", "/v1/chat/completions", "https://api.example.com/v1/chat/completions?api-version=1"},
	}
	for _, testCase := range cases {
		base, err := url.Parse(testCase.base)
		if err != nil {
			t.Fatalf("parse %q: %v", testCase.base, err)
		}
		if got := upstreamTarget(base, testCase.path).String(); got != testCase.expected {
			t.Errorf("upstreamTarget(%q, %q) = %q, want %q", testCase.base, testCase.path, got, testCase.expected)
		}
	}
}

// The path an upstream actually receives is the one the operator configured,
// which is only observable end to end: a site mounted under a sub-path must see
// its own prefix and the version segment exactly once.
func TestEngineDispatchesToTheSubPathOfAnUpstream(t *testing.T) {
	var receivedPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	selector := router.NewMemorySelector(routeFor(nil, domain.Channel{
		ID:      "line",
		BaseURL: upstream.URL + "/openai/v1",
		Enabled: true,
	}))
	engine := Engine{Selector: selector, Client: &http.Client{}}

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

	if receivedPath != "/openai/v1/chat/completions" {
		t.Errorf("upstream path = %q, want /openai/v1/chat/completions", receivedPath)
	}
}
