package api

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yhw5231/fluxgate/internal/domain"
	"github.com/yhw5231/fluxgate/internal/proxy"
	"github.com/yhw5231/fluxgate/internal/router"
	"github.com/yhw5231/fluxgate/internal/store"
)

type fakeAuthenticator struct {
	wantCredential string
	key            store.DownstreamAPIKey
	err            error
	seen           string
}

// testRoutes wraps channels in a single catch-all route so tests can focus on
// transport behavior rather than route resolution.
func testRoutes(channels ...domain.Channel) []domain.Route {
	return []domain.Route{{
		ID:           1,
		ModelPattern: "*",
		Mode:         domain.RouteModePattern,
		Enabled:      true,
		Channels:     channels,
	}}
}

func (a *fakeAuthenticator) AuthenticateDownstreamKey(_ context.Context, credential string, _ time.Time) (store.DownstreamAPIKey, error) {
	a.seen = credential
	if a.err != nil || credential != a.wantCredential {
		return store.DownstreamAPIKey{}, errors.New("unauthorized")
	}
	return a.key, nil
}

func TestBearerAndAPIKeyAuthentication(t *testing.T) {
	for _, test := range []struct {
		name   string
		header string
		value  string
	}{
		{name: "bearer", header: "Authorization", value: "Bearer downstream-secret"},
		{name: "x api key", header: "X-API-Key", value: "downstream-secret"},
		{name: "api key", header: "Api-Key", value: "downstream-secret"},
	} {
		t.Run(test.name, func(t *testing.T) {
			authenticator := &fakeAuthenticator{
				wantCredential: "downstream-secret",
				key:            store.DownstreamAPIKey{SupportedModels: []string{"gpt-4.1"}},
			}
			server := &Server{Authenticator: authenticator, Models: []string{"gpt-4.1"}}
			request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
			request.Header.Set(test.header, test.value)
			response := httptest.NewRecorder()

			server.Handler().ServeHTTP(response, request)

			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body = %s", response.Code, response.Body.String())
			}
			if authenticator.seen != "downstream-secret" {
				t.Fatalf("authenticated credential = %q", authenticator.seen)
			}
		})
	}
}

func TestAuthenticationFailureDoesNotExposeCredential(t *testing.T) {
	const credential = "credential-must-not-leak"
	authenticator := &fakeAuthenticator{wantCredential: "different"}
	server := &Server{Authenticator: authenticator, Models: []string{"gpt-4.1"}}
	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	request.Header.Set("Authorization", "Bearer "+credential)
	response := httptest.NewRecorder()

	server.Handler().ServeHTTP(response, request)

	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", response.Code)
	}
	if strings.Contains(response.Body.String(), credential) {
		t.Fatalf("response exposed credential: %s", response.Body.String())
	}
}

// supported_models is an exclusion list: a listed model is hidden, everything
// else stays visible.
func TestModelsHidesDeniedModelsForAuthenticatedKey(t *testing.T) {
	authenticator := &fakeAuthenticator{
		wantCredential: "secret",
		key:            store.DownstreamAPIKey{SupportedModels: []string{"claude-sonnet"}},
	}
	server := &Server{
		Authenticator: authenticator,
		Models:        []string{"claude-sonnet", "gpt-4.1"},
	}
	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()

	server.Handler().ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Code)
	}
	body := response.Body.String()
	if !strings.Contains(body, `"id":"gpt-4.1"`) {
		t.Fatalf("body = %s, want the model that is not excluded", body)
	}
	if strings.Contains(body, "claude-sonnet") {
		t.Fatalf("body = %s, contained an excluded model", body)
	}
}

func TestModelsOmitModelsWithoutAnyChannel(t *testing.T) {
	authenticator := &fakeAuthenticator{wantCredential: "secret"}
	server := &Server{
		Authenticator: authenticator,
		Models:        []string{"served-model", "orphan-model"},
		Engine: &proxy.Engine{Selector: router.NewMemorySelector([]domain.Route{{
			ID:           1,
			ModelPattern: "served-model",
			Mode:         domain.RouteModePattern,
			Enabled:      true,
			Channels:     []domain.Channel{{ID: "channel", Enabled: true, Weight: 1}},
		}})},
	}
	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()

	server.Handler().ServeHTTP(response, request)

	body := response.Body.String()
	if !strings.Contains(body, "served-model") {
		t.Fatalf("body = %s, want the routable model", body)
	}
	if strings.Contains(body, "orphan-model") {
		t.Fatalf("body = %s, listed a model with no channel", body)
	}
}

// A model with no usable route or channel never reaches an upstream, so it is
// an availability failure rather than a bad gateway.
func TestProxyReturnsServiceUnavailableWhenNothingCanServeTheModel(t *testing.T) {
	engine := &proxy.Engine{
		Selector: router.NewMemorySelector([]domain.Route{{
			ID:           1,
			ModelPattern: "known-model",
			Mode:         domain.RouteModePattern,
			Enabled:      true,
			Channels:     []domain.Channel{{ID: "channel", Enabled: true, Weight: 1}},
		}}),
		Policy: domain.RetryPolicy{MaxAttempts: 1, MaxAttemptsPerChannel: 1},
	}
	server := &Server{Engine: engine, Authenticator: allowTestAuthentication()}
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"unknown-model"}`))
	request.Header.Set("Authorization", "Bearer test-key")
	response := httptest.NewRecorder()

	server.Handler().ServeHTTP(response, request)

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body = %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"code":"no_available_channel"`) {
		t.Fatalf("body = %s, want no_available_channel", response.Body.String())
	}
}

func TestProxyRejectsDeniedModelWithForbidden(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("upstream was contacted for a denied model")
	}))
	defer upstream.Close()

	engine := &proxy.Engine{
		Selector: router.NewMemorySelector(testRoutes(domain.Channel{
			ID: "channel", BaseURL: upstream.URL, Enabled: true, Weight: 1,
		})),
		Policy: domain.RetryPolicy{MaxAttempts: 1, MaxAttemptsPerChannel: 1},
	}
	authenticator := &fakeAuthenticator{
		wantCredential: "secret",
		key:            store.DownstreamAPIKey{SupportedModels: []string{"blocked-*"}},
	}
	server := &Server{Engine: engine, Authenticator: authenticator}
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"blocked-model"}`))
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()

	server.Handler().ServeHTTP(response, request)

	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body = %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"code":"model_not_allowed"`) {
		t.Fatalf("body = %s, want model_not_allowed", response.Body.String())
	}
}

func TestReadinessAndManagementStatus(t *testing.T) {
	const managementToken = "management-secret"
	server := &Server{
		Engine:          &proxy.Engine{},
		ManagementToken: managementToken,
		Models:          []string{"a", "b"},
		StartedAt:       time.Now().Add(-time.Second),
	}

	notReady := httptest.NewRecorder()
	server.Handler().ServeHTTP(notReady, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if notReady.Code != http.StatusServiceUnavailable {
		t.Fatalf("not-ready status = %d, want 503", notReady.Code)
	}

	server.Ready.Store(true)
	ready := httptest.NewRecorder()
	server.Handler().ServeHTTP(ready, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if ready.Code != http.StatusOK {
		t.Fatalf("ready status = %d, want 200", ready.Code)
	}

	status := httptest.NewRecorder()
	statusRequest := httptest.NewRequest(http.MethodGet, "/management/status", nil)
	statusRequest.Header.Set("Authorization", "Bearer "+managementToken)
	server.Handler().ServeHTTP(status, statusRequest)
	if status.Code != http.StatusOK {
		t.Fatalf("management status = %d, want 200", status.Code)
	}
	if !strings.Contains(status.Body.String(), `"ready":true`) || !strings.Contains(status.Body.String(), `"model_count":2`) {
		t.Fatalf("management body = %s", status.Body.String())
	}
}

func TestProxyRemovesDownstreamCredentialsBeforeForwarding(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer upstream-secret" {
			t.Errorf("upstream Authorization = %q", got)
		}
		if got := r.Header.Get("X-API-Key"); got != "" {
			t.Errorf("upstream X-API-Key = %q, want removed", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	authenticator := &fakeAuthenticator{wantCredential: "downstream-secret"}
	engine := &proxy.Engine{
		Selector: router.NewMemorySelector(testRoutes(domain.Channel{
			ID: "channel", BaseURL: upstream.URL, APIKey: "upstream-secret", Enabled: true, Weight: 1,
		})),
		Policy: domain.RetryPolicy{MaxAttempts: 1, MaxAttemptsPerChannel: 1},
	}
	server := &Server{Engine: engine, Authenticator: authenticator}
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-4.1"}`))
	request.Header.Set("Authorization", "Bearer downstream-secret")
	request.Header.Set("X-API-Key", "downstream-secret")
	response := httptest.NewRecorder()

	server.Handler().ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", response.Code, response.Body.String())
	}
}

func TestSSEIsFlushedIncrementally(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		_, _ = w.Write([]byte("data: first\n\n"))
		flusher.Flush()
		_, _ = w.Write([]byte("data: second\n\n"))
		flusher.Flush()
	}))
	defer upstream.Close()

	engine := &proxy.Engine{
		Selector: router.NewMemorySelector(testRoutes(domain.Channel{ID: "stream", BaseURL: upstream.URL, Enabled: true, Weight: 1})),
		Policy:   domain.RetryPolicy{MaxAttempts: 1, MaxAttemptsPerChannel: 1},
	}
	server := &Server{Engine: engine, Authenticator: allowTestAuthentication()}
	target := httptest.NewServer(server.Handler())
	defer target.Close()

	request, err := http.NewRequest(http.MethodPost, target.URL+"/v1/chat/completions", strings.NewReader(`{"model":"gpt-4.1","stream":true}`))
	if err != nil {
		t.Fatalf("create POST request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer test-key")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("POST error = %v", err)
	}
	defer response.Body.Close()

	buffer := make([]byte, len("data: first\n\n"))
	if _, err := response.Body.Read(buffer); err != nil {
		t.Fatalf("read first event: %v", err)
	}
	if !strings.Contains(string(buffer), "data: first") {
		t.Fatalf("first streamed bytes = %q", buffer)
	}
}

func TestStructuredLoggingRedactsCredentials(t *testing.T) {
	const credential = "credential-must-not-appear"
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, nil))
	server := &Server{
		Authenticator: &fakeAuthenticator{wantCredential: "different"},
		Logger:        logger,
	}
	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	request.Header.Set("Authorization", "Bearer "+credential)
	request.Header.Set("X-API-Key", credential)
	response := httptest.NewRecorder()

	server.Handler().ServeHTTP(response, request)

	logged := output.String()
	if !strings.Contains(logged, `"msg":"http_request"`) {
		t.Fatalf("log = %s, want structured request event", logged)
	}
	if strings.Contains(logged, credential) || strings.Contains(logged, "Authorization") || strings.Contains(logged, "X-API-Key") {
		t.Fatalf("log exposed credential data: %s", logged)
	}
}

func TestClientIPUsesForwardedHeadersAndRemoteAddress(t *testing.T) {
	tests := []struct {
		name          string
		forwardedFor  string
		realIP        string
		remoteAddress string
		expected      string
	}{
		{
			name:          "first valid forwarded address",
			forwardedFor:  "203.0.113.10, 10.0.0.2",
			realIP:        "198.51.100.20",
			remoteAddress: "172.18.0.1:43210",
			expected:      "203.0.113.10",
		},
		{
			name:          "real IP fallback",
			forwardedFor:  "invalid",
			realIP:        "198.51.100.20",
			remoteAddress: "172.18.0.1:43210",
			expected:      "198.51.100.20",
		},
		{
			name:          "remote address fallback",
			forwardedFor:  "invalid",
			realIP:        "invalid",
			remoteAddress: "172.18.0.1:43210",
			expected:      "172.18.0.1",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
			request.RemoteAddr = test.remoteAddress
			request.Header.Set("X-Forwarded-For", test.forwardedFor)
			request.Header.Set("X-Real-IP", test.realIP)

			if actual := clientIP(request); actual != test.expected {
				t.Fatalf("clientIP() = %q, want %q", actual, test.expected)
			}
		})
	}
}

func TestStructuredLoggingIncludesForwardedClientIP(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, nil))
	server := &Server{Logger: logger}
	request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	request.Header.Set("X-Forwarded-For", "203.0.113.25, 172.18.0.1")
	response := httptest.NewRecorder()

	server.Handler().ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Code)
	}
	logged := output.String()
	if !strings.Contains(logged, `"client_ip":"203.0.113.25"`) {
		t.Fatalf("log = %s, want forwarded client IP", logged)
	}
}

func TestConsoleIsServedWithoutManagementToken(t *testing.T) {
	server := &Server{ManagementToken: "management-secret"}
	request := httptest.NewRequest(http.MethodGet, "/console/", nil)
	response := httptest.NewRecorder()

	server.Handler().ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Code)
	}
	if got := response.Header().Get("Content-Type"); !strings.Contains(got, "text/html") {
		t.Fatalf("Content-Type = %q, want text/html", got)
	}
	if !strings.Contains(response.Body.String(), "Fluxgate Console") {
		t.Fatal("console index did not contain the expected title")
	}
}

func TestConsoleRedirectsBarePrefix(t *testing.T) {
	server := &Server{}
	request := httptest.NewRequest(http.MethodGet, "/console", nil)
	response := httptest.NewRecorder()

	server.Handler().ServeHTTP(response, request)

	if response.Code != http.StatusMovedPermanently && response.Code != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d, want a redirect", response.Code)
	}
	if got := response.Header().Get("Location"); got != "console/" {
		t.Fatalf("Location = %q, want %q", got, "console/")
	}
}

// A bare root request is a visitor looking for the console. It must not fall
// through to Go's default plain-text 404, and the redirect target stays relative
// so it resolves against the public URL when a reverse proxy strips a prefix.
func TestRootRedirectsToConsole(t *testing.T) {
	for _, testCase := range []struct {
		name string
		path string
	}{
		{name: "root", path: "/"},
		{name: "console prefix", path: "/console"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			server := &Server{}
			request := httptest.NewRequest(http.MethodGet, testCase.path, nil)
			response := httptest.NewRecorder()

			server.Handler().ServeHTTP(response, request)

			if response.Code != http.StatusTemporaryRedirect {
				t.Fatalf("status = %d, want 307", response.Code)
			}
			location := response.Header().Get("Location")
			if location != "console/" {
				t.Fatalf("Location = %q, want a relative %q", location, "console/")
			}
		})
	}
}

// The console must work when mounted under a path prefix: its assets are
// referenced relatively and its management calls are derived from the page URL.
func TestConsoleAssetsUseRelativePaths(t *testing.T) {
	server := &Server{}
	request := httptest.NewRequest(http.MethodGet, "/console/", nil)
	response := httptest.NewRecorder()

	server.Handler().ServeHTTP(response, request)

	body := response.Body.String()
	for _, unwanted := range []string{`href="/console/`, `src="/console/`} {
		if strings.Contains(body, unwanted) {
			t.Errorf("console index contains root-absolute asset reference %q", unwanted)
		}
	}
	for _, wanted := range []string{`href="styles.css"`, `src="app.js"`} {
		if !strings.Contains(body, wanted) {
			t.Errorf("console index is missing relative asset reference %q", wanted)
		}
	}
}

// A browser navigation the gateway does not serve lands on the console rather
// than a bare 404, so a bare domain and a mistyped page both stay useful.
func TestUnknownPageRedirectsToConsole(t *testing.T) {
	for _, testCase := range []struct {
		name string
		path string
		want string
	}{
		{name: "root", path: "/", want: "console/"},
		{name: "single segment", path: "/admin", want: "console/"},
		{name: "nested", path: "/a/b", want: "../console/"},
		{name: "deeply nested", path: "/a/b/c", want: "../../console/"},
		{name: "directory", path: "/a/b/", want: "../../console/"},
		{name: "index html", path: "/index.html", want: "console/"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			server := &Server{}
			request := httptest.NewRequest(http.MethodGet, testCase.path, nil)
			response := httptest.NewRecorder()

			server.Handler().ServeHTTP(response, request)

			if response.Code != http.StatusTemporaryRedirect {
				t.Fatalf("status = %d, want 307", response.Code)
			}
			if got := response.Header().Get("Location"); got != testCase.want {
				t.Fatalf("Location = %q, want %q", got, testCase.want)
			}
		})
	}
}

// A proxy that strips a path prefix reports it in X-Forwarded-Prefix. Using it
// is the only way to keep the prefix for a visitor who arrived at a slash-less
// path such as https://host/gateway, where the browser would otherwise resolve a
// relative target against the parent directory and drop the prefix.
func TestConsoleRedirectUsesForwardedPrefix(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		prefix string
		want   string
	}{
		{name: "simple", prefix: "/gateway", want: "/gateway/console/"},
		{name: "trailing slash", prefix: "/gateway/", want: "/gateway/console/"},
		{name: "nested", prefix: "/tools/gateway", want: "/tools/gateway/console/"},
		{name: "whitespace", prefix: "  /gateway  ", want: "/gateway/console/"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			server := &Server{}
			request := httptest.NewRequest(http.MethodGet, "/", nil)
			request.Header.Set("X-Forwarded-Prefix", testCase.prefix)
			response := httptest.NewRecorder()

			server.Handler().ServeHTTP(response, request)

			if response.Code != http.StatusTemporaryRedirect {
				t.Fatalf("status = %d, want 307", response.Code)
			}
			if got := response.Header().Get("Location"); got != testCase.want {
				t.Fatalf("Location = %q, want %q", got, testCase.want)
			}
		})
	}
}

// The prefix is echoed into a Location header, so a value that could redirect
// off-origin or smuggle a header must be ignored in favour of the relative form.
func TestConsoleRedirectRejectsUnsafeForwardedPrefix(t *testing.T) {
	for _, prefix := range []string{
		"https://evil.example",  // absolute URL
		"//evil.example",        // protocol-relative
		"/gateway/../admin",     // traversal segment
		"/gateway/./admin",      // current-directory segment
		"/gateway//admin",       // empty segment
		"/gateway\\admin",       // backslash escaping
		"/gateway\r\nX-Evil: 1", // header injection
		"gateway",               // not rooted
		"/",                     // no prefix at all
		"",                      // absent
	} {
		t.Run(prefix, func(t *testing.T) {
			server := &Server{}
			request := httptest.NewRequest(http.MethodGet, "/", nil)
			if prefix != "" {
				request.Header.Set("X-Forwarded-Prefix", prefix)
			}
			response := httptest.NewRecorder()

			server.Handler().ServeHTTP(response, request)

			location := response.Header().Get("Location")
			if location != "console/" {
				t.Fatalf("Location = %q, want the relative fallback %q", location, "console/")
			}
			if strings.Contains(location, "evil.example") || strings.Contains(location, "X-Evil") {
				t.Fatalf("Location = %q, the header was echoed unsafely", location)
			}
		})
	}
}

// A mistyped endpoint must keep answering 404: an API client is not a browser,
// and a login page would hide the real mistake.
func TestUnknownAPIPathStillReturns404(t *testing.T) {
	for _, path := range []string{
		"/v1/typo",
		"/management/typo",
		"/console/missing.js",
	} {
		t.Run(path, func(t *testing.T) {
			server := &Server{}
			request := httptest.NewRequest(http.MethodGet, path, nil)
			response := httptest.NewRecorder()

			server.Handler().ServeHTTP(response, request)

			if response.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404", response.Code)
			}
			if location := response.Header().Get("Location"); location != "" {
				t.Fatalf("Location = %q, want no redirect from an API path", location)
			}
		})
	}
}
