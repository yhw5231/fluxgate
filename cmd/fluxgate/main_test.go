package main

import (
	"net/http"
	"testing"

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
