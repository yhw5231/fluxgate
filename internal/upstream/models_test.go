package upstream

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// An OpenAI-shaped listing is the common case and the whole reason the probe
// exists: the operator picks from the names the upstream reports.
func TestModelsReadsAnOpenAIListing(t *testing.T) {
	var seenAuth, seenKey string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("path = %q, want /v1/models", r.URL.Path)
		}
		seenAuth = r.Header.Get("Authorization")
		seenKey = r.Header.Get("X-Api-Key")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"gpt-4.1"},{"id":"gpt-4.1-mini"},{"id":"gpt-4.1"}]}`))
	}))
	defer server.Close()

	result, err := (&Client{}).Models(context.Background(), Request{BaseURL: server.URL, APIKey: "secret"})
	if err != nil {
		t.Fatalf("Models() error = %v", err)
	}
	if len(result.Models) != 2 {
		t.Errorf("models = %v, want two unique names", result.Models)
	}
	if result.Models[0] != "gpt-4.1" || result.Models[1] != "gpt-4.1-mini" {
		t.Errorf("models = %v, want them sorted", result.Models)
	}
	if seenAuth != "Bearer secret" || seenKey != "secret" {
		t.Errorf("credentials presented = %q / %q, want both schemes", seenAuth, seenKey)
	}
	if result.Endpoint != server.URL+"/v1/models" {
		t.Errorf("endpoint = %q, want the path that answered", result.Endpoint)
	}
}

// Upstreams that mount their API at the root, or answer with a different shape,
// still work: the probe tries the other path and reads the names it finds.
func TestModelsFallsBackToTheRootPathAndOtherShapes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"models":[{"name":"local-model"},{"name":"other-model"}]}`))
	}))
	defer server.Close()

	result, err := (&Client{}).Models(context.Background(), Request{BaseURL: server.URL, APIKey: "secret"})
	if err != nil {
		t.Fatalf("Models() error = %v", err)
	}
	if len(result.Models) != 2 || result.Models[0] != "local-model" {
		t.Errorf("models = %v, want the names from the alternate shape", result.Models)
	}
	if !strings.HasSuffix(result.Endpoint, "/models") || strings.HasSuffix(result.Endpoint, "/v1/models") {
		t.Errorf("endpoint = %q, want the root path", result.Endpoint)
	}
}

// An address that already carries a version segment is not doubled up.
func TestModelsUsesTheVersionedAddressAsGiven(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"data":["a-model"]}`))
	}))
	defer server.Close()

	result, err := (&Client{}).Models(context.Background(), Request{BaseURL: server.URL + "/v1", APIKey: "secret"})
	if err != nil {
		t.Fatalf("Models() error = %v", err)
	}
	if len(result.Models) != 1 || result.Models[0] != "a-model" {
		t.Errorf("models = %v, want a-model", result.Models)
	}
}

func TestModelsReportsWhatWentWrong(t *testing.T) {
	unauthorized := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer unauthorized.Close()
	_, err := (&Client{}).Models(context.Background(), Request{BaseURL: unauthorized.URL, APIKey: "wrong"})
	if !errors.Is(err, ConfigError) {
		t.Fatalf("error = %v, want a ConfigError", err)
	}
	if !strings.Contains(err.Error(), "credential") {
		t.Errorf("error = %q, want it to name the credential", err.Error())
	}

	notJSON := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html>hello</html>"))
	}))
	defer notJSON.Close()
	if _, err := (&Client{}).Models(context.Background(), Request{BaseURL: notJSON.URL, APIKey: "key"}); !errors.Is(err, ConfigError) {
		t.Errorf("error = %v, want a ConfigError for a non-JSON answer", err)
	}

	// A request the gateway will not even make is a configuration error, not an
	// upstream one.
	for _, request := range []Request{
		{BaseURL: "", APIKey: "key"},
		{BaseURL: "ftp://example.test", APIKey: "key"},
		{BaseURL: "https://example.test", APIKey: "", Headers: nil},
	} {
		if _, err := (&Client{}).Models(context.Background(), request); !errors.Is(err, ConfigError) {
			t.Errorf("Models(%+v) error = %v, want a ConfigError", request, err)
		}
	}
}

// A probe of an address nobody answers is reported as unreachable rather than
// hanging or panicking.
func TestModelsReportsAnUnreachableUpstream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := server.URL
	server.Close()

	if _, err := (&Client{}).Models(context.Background(), Request{BaseURL: url, APIKey: "key"}); !errors.Is(err, ConfigError) {
		t.Fatalf("error = %v, want a ConfigError", err)
	}
}

// Custom headers are how a platform that needs a vendor-specific header is
// reached, so they have to reach the request.
func TestModelsSendsCustomHeaders(t *testing.T) {
	var version string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		version = r.Header.Get("anthropic-version")
		_, _ = w.Write([]byte(`{"data":["claude-3-5-sonnet"]}`))
	}))
	defer server.Close()

	if _, err := (&Client{}).Models(context.Background(), Request{
		BaseURL: server.URL, Headers: map[string]string{"anthropic-version": "2023-06-01"},
	}); err != nil {
		t.Fatalf("Models() error = %v", err)
	}
	if version != "2023-06-01" {
		t.Errorf("custom header = %q, want it sent", version)
	}
}
