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

// A listing entry names one model. A platform that serves models from channel
// groups labels them beside the id — {"id":"cn:deepseek-v4.1-flash",
// "name":"Deepseek-V4.1-Flash"} — and that label is what its own console shows,
// not a name it would accept. Offering it as a model is what let an operator's
// lowercase model be answered by the label.
func TestModelsReadsTheIdentifierAndNotTheDisplayLabel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"object":"list","data":[` +
			`{"id":"cn:deepseek-v4.1-flash","object":"model","name":"Deepseek-V4.1-Flash"},` +
			`{"id":"global:deepseek-v4.1-flash","object":"model","name":"Deepseek-V4.1-Flash"},` +
			// An entry whose identifier is blank still contributes the name it
			// does carry, so a shape that is only ever named keeps working.
			`{"id":"","name":"bare-name"}]}`))
	}))
	defer server.Close()

	result, err := (&Client{}).Models(context.Background(), Request{BaseURL: server.URL, APIKey: "secret"})
	if err != nil {
		t.Fatalf("Models() error = %v", err)
	}
	want := []string{"bare-name", "cn:deepseek-v4.1-flash", "global:deepseek-v4.1-flash"}
	if len(result.Models) != len(want) {
		t.Fatalf("models = %v, want %v", result.Models, want)
	}
	for index, name := range want {
		if result.Models[index] != name {
			t.Errorf("models = %v, want %v", result.Models, want)
			break
		}
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

// One upstream has several spellings — with or without a trailing slash, with or
// without the version segment, mounted under a subpath — and all of them have to
// find its model listing. An operator types whatever the upstream's own
// documentation shows.
func TestModelsAdaptsToTheSpellingOfTheAddress(t *testing.T) {
	versioned := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/v1/models") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"data":["versioned-model"]}`))
	}))
	defer versioned.Close()

	rooted := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/v1/models") || !strings.HasSuffix(r.URL.Path, "/models") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"data":["root-model"]}`))
	}))
	defer rooted.Close()

	mounted := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/openai/v1/models" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"data":["mounted-model"]}`))
	}))
	defer mounted.Close()

	cases := []struct {
		name    string
		base    string
		want    string
		model   string
		missing string
	}{
		{name: "bare host", base: versioned.URL, want: versioned.URL + "/v1/models", model: "versioned-model"},
		{name: "trailing slash", base: versioned.URL + "/", want: versioned.URL + "/v1/models", model: "versioned-model"},
		{name: "version", base: versioned.URL + "/v1", want: versioned.URL + "/v1/models", model: "versioned-model"},
		{name: "version with trailing slash", base: versioned.URL + "/v1/", want: versioned.URL + "/v1/models", model: "versioned-model"},
		// The same address written without the version segment still finds a
		// listing that sits at /models, because a versioned address is also
		// probed one level up.
		{name: "unversioned", base: rooted.URL, want: rooted.URL + "/models", model: "root-model"},
		{name: "version printed anyway", base: rooted.URL + "/v1", want: rooted.URL + "/models", model: "root-model"},
		{name: "mounted under a subpath", base: mounted.URL + "/openai/v1", want: mounted.URL + "/openai/v1/models", model: "mounted-model"},
		{name: "subpath without the version", base: mounted.URL + "/openai", want: mounted.URL + "/openai/v1/models", model: "mounted-model"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			result, err := (&Client{}).Models(context.Background(), Request{BaseURL: testCase.base, APIKey: "secret"})
			if err != nil {
				t.Fatalf("Models(%q) error = %v", testCase.base, err)
			}
			if len(result.Models) != 1 || result.Models[0] != testCase.model {
				t.Errorf("models = %v, want %s", result.Models, testCase.model)
			}
			if result.Endpoint != testCase.want {
				t.Errorf("endpoint = %q, want %q", result.Endpoint, testCase.want)
			}
		})
	}
}

// A probe that finds no listing says which addresses it tried, because that is
// what tells an operator whether the address or the upstream is wrong.
func TestModelsReportsEveryPathItTried(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	_, err := (&Client{}).Models(context.Background(), Request{BaseURL: server.URL, APIKey: "secret"})
	var failed Failure
	if !errors.As(err, &failed) || failed.Reason != ReasonNoModelEndpoint {
		t.Fatalf("error = %v, want a %s failure", err, ReasonNoModelEndpoint)
	}
	endpoints, ok := failed.Params["endpoints"].([]string)
	if !ok || len(endpoints) != 2 {
		t.Fatalf("failure params = %v, want the endpoints that were tried", failed.Params)
	}
	if endpoints[0] != server.URL+"/v1/models" || endpoints[1] != server.URL+"/models" {
		t.Errorf("endpoints = %v, want the versioned path first", endpoints)
	}
	if !strings.Contains(failed.Error(), endpoints[0]) {
		t.Errorf("error = %q, want it to name the address it tried", failed.Error())
	}
}

// The proxy the form carries is what the probe goes out through: an upstream
// that is only reachable through one cannot be listed without it.
func TestModelsProbesThroughTheConfiguredProxy(t *testing.T) {
	var proxied int
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":["direct-model"]}`))
	}))
	defer upstreamServer.Close()

	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxied++
		_, _ = w.Write([]byte(`{"data":["proxied-model"]}`))
	}))
	defer proxyServer.Close()

	result, err := (&Client{}).Models(context.Background(), Request{
		BaseURL: upstreamServer.URL, APIKey: "secret", ProxyURL: proxyServer.URL,
	})
	if err != nil {
		t.Fatalf("Models() error = %v", err)
	}
	if proxied != 1 {
		t.Fatalf("the proxy saw %d requests, want 1", proxied)
	}
	if len(result.Models) != 1 || result.Models[0] != "proxied-model" {
		t.Errorf("models = %v, want the answer the proxy relayed", result.Models)
	}
}

// An upstream that cannot be reached through the proxy it was given fails, and
// the failure says which proxy carried it: an operator cannot fix a proxy the
// message does not name.
func TestModelsReportsTheProxyItUsedWhenItCannotConnect(t *testing.T) {
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":["direct-model"]}`))
	}))
	defer upstreamServer.Close()

	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	proxyAddress := closed.URL
	closed.Close()

	_, err := (&Client{}).Models(context.Background(), Request{
		BaseURL: upstreamServer.URL, APIKey: "secret", ProxyURL: proxyAddress,
	})
	var failed Failure
	if !errors.As(err, &failed) || failed.Reason != ReasonUnreachable {
		t.Fatalf("error = %v, want a %s failure", err, ReasonUnreachable)
	}
	if failed.Params["proxy_url"] != proxyAddress {
		t.Errorf("failure params = %v, want the proxy address that was used", failed.Params)
	}
	if failed.Params["endpoint"] != upstreamServer.URL+"/v1/models" {
		t.Errorf("failure params = %v, want the endpoint that was tried", failed.Params)
	}
}

// An explicit direct connection means the address is used as given, with no
// proxy in front of it — which is what makes "direct" a setting rather than a
// synonym for an empty field.
func TestModelsHonoursAnExplicitDirectConnection(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":["local-model"]}`))
	}))
	defer server.Close()

	if _, err := (&Client{}).Models(context.Background(), Request{
		BaseURL: server.URL, APIKey: "secret", ProxyURL: "direct",
	}); err != nil {
		t.Fatalf("Models(direct) error = %v", err)
	}

	// The same upstream through a proxy that is not there fails, so the field
	// really decides the route instead of being ignored.
	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	proxyAddress := closed.URL
	closed.Close()

	_, err := (&Client{}).Models(context.Background(), Request{
		BaseURL: server.URL, APIKey: "secret", ProxyURL: proxyAddress,
	})
	var failed Failure
	if !errors.As(err, &failed) {
		t.Fatalf("error = %v, want a described failure", err)
	}
	if failed.Reason != ReasonUnreachable {
		t.Errorf("reason = %q, want %s", failed.Reason, ReasonUnreachable)
	}
	if failed.Params["proxy_url"] != proxyAddress {
		t.Errorf("failure params = %v, want the proxy that was used", failed.Params)
	}
}

// A proxy that cannot be used is a configuration mistake, not an unreachable
// upstream, so it is reported as one.
func TestModelsRejectsAnUnusableProxy(t *testing.T) {
	_, err := (&Client{}).Models(context.Background(), Request{
		BaseURL: "https://api.example.com", APIKey: "secret", ProxyURL: "socks4://127.0.0.1:1080",
	})
	var failed Failure
	if !errors.As(err, &failed) || failed.Reason != ReasonInvalidProxy {
		t.Fatalf("error = %v, want a %s failure", err, ReasonInvalidProxy)
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
