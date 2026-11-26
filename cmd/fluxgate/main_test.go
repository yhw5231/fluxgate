package main

import (
	"bytes"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/yhw5231/fluxgate/internal/config"
	"github.com/yhw5231/fluxgate/internal/domain"
	"github.com/yhw5231/fluxgate/internal/store"
)

func TestBuildProxyResolverUsesEnabledDefaultProfile(t *testing.T) {
	configuration := store.Configuration{
		ProxyProfiles: []store.ProxyProfile{
			{ID: 1, Name: "disabled-default", URL: "http://disabled.example:8080", IsDefault: true, Enabled: false},
			{ID: 2, Name: "enabled-default", URL: "http://proxy.example:8080", IsDefault: true, Enabled: true},
			{ID: 3, Name: "later-default", URL: "http://later.example:8080", IsDefault: true, Enabled: true},
		},
	}

	resolver := buildProxyResolver(configuration)
	if resolver == nil {
		t.Fatal("buildProxyResolver() returned nil")
	}
	if resolver.Config.Default != "http://proxy.example:8080" {
		t.Fatalf("default proxy = %q, want first enabled default profile", resolver.Config.Default)
	}
	if resolver.Config.Sites == nil {
		t.Fatal("site proxy map is nil")
	}
	if resolver.Config.Keys == nil {
		t.Fatal("key proxy map is nil")
	}
	if resolver.SystemProxy == nil {
		t.Fatal("system proxy resolver is nil")
	}
}

func TestBuildProxyResolverAllowsNoDefaultProfile(t *testing.T) {
	configuration := store.Configuration{
		ProxyProfiles: []store.ProxyProfile{
			{ID: 1, Name: "non-default", URL: "http://proxy.example:8080", IsDefault: false, Enabled: true},
			{ID: 2, Name: "disabled-default", URL: "http://disabled.example:8080", IsDefault: true, Enabled: false},
		},
	}

	resolver := buildProxyResolver(configuration)
	if resolver.Config.Default != "" {
		t.Fatalf("default proxy = %q, want empty", resolver.Config.Default)
	}
}

func TestBuildProxyResolverUsesEnvironmentProxyFunction(t *testing.T) {
	resolver := buildProxyResolver(store.Configuration{})
	if resolver.SystemProxy == nil {
		t.Fatal("system proxy resolver is nil")
	}

	request, err := http.NewRequest(http.MethodGet, "https://example.com", nil)
	if err != nil {
		t.Fatalf("http.NewRequest() error = %v", err)
	}
	if _, err := resolver.SystemProxy(request); err != nil {
		t.Fatalf("system proxy resolver error = %v", err)
	}
}

func TestBuildProxyResolverModelsChannelAndSiteProxyDecisions(t *testing.T) {
	configuration := store.Configuration{
		Channels: []domain.Channel{
			{
				BaseURL:            "https://API.Example.com/v1",
				APIKey:             "token-key",
				ProxyURL:           "http://token-proxy.example:8080",
				SiteProxyURL:       "http://site-proxy.example:8080",
				UseSystemProxy:     true,
				SiteUseSystemProxy: true,
			},
			{
				BaseURL:            "https://system-site.example/v1",
				APIKey:             "system-key",
				UseSystemProxy:     true,
				SiteUseSystemProxy: true,
			},
		},
	}

	resolver := buildProxyResolver(configuration)
	if got := resolver.Config.Keys["token-key"]; got != "http://token-proxy.example:8080" {
		t.Fatalf("token key proxy = %q, want explicit token proxy", got)
	}
	if got := resolver.Config.Sites["api.example.com"]; got != "http://site-proxy.example:8080" {
		t.Fatalf("site proxy = %q, want explicit site proxy", got)
	}
	if got := resolver.Config.Keys["system-key"]; got != "system" {
		t.Fatalf("system key proxy decision = %q, want system", got)
	}
	if got := resolver.Config.Sites["system-site.example"]; got != "system" {
		t.Fatalf("system site proxy decision = %q, want system", got)
	}
}

// A response-header bound shorter than the request timeout decides the same phase
// first, so a model that is still thinking is recorded as a failed upstream. The
// setting comes from the deployment's environment, so it is named at startup; the
// documented default stays quiet.
func TestWarnResponseHeaderTimeoutBelowTheRequestTimeout(t *testing.T) {
	var logged bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logged, nil))

	warnResponseHeaderTimeout(logger, config.Config{
		RequestTimeout:        5 * time.Minute,
		ResponseHeaderTimeout: 30 * time.Second,
	})
	warning := logged.String()
	if !strings.Contains(warning, "response_header_timeout_below_request_timeout") {
		t.Fatalf("log = %q, want the header bound named", warning)
	}
	if !strings.Contains(warning, "30s") || !strings.Contains(warning, "5m0s") {
		t.Errorf("log = %q, want both bounds", warning)
	}

	logged.Reset()
	warnResponseHeaderTimeout(logger, config.Config{RequestTimeout: 5 * time.Minute})
	if logged.Len() != 0 {
		t.Errorf("log = %q, want silence when the header bound is off", logged.String())
	}

	logged.Reset()
	warnResponseHeaderTimeout(logger, config.Config{
		RequestTimeout:        5 * time.Minute,
		ResponseHeaderTimeout: 10 * time.Minute,
	})
	if logged.Len() != 0 {
		t.Errorf("log = %q, want silence when the header bound is the longer one", logged.String())
	}
}
