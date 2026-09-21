package proxy

import (
	"net/http"
	"net/url"
	"testing"
)

func mustURL(t *testing.T, value string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(value)
	if err != nil {
		t.Fatalf("url.Parse(%q) error = %v", value, err)
	}
	return parsed
}

func TestResolverPrecedence(t *testing.T) {
	target := mustURL(t, "https://api.example.com/v1/responses")
	systemURL := mustURL(t, "http://system-proxy.example:8080")
	// A resolver guards its configuration with a lock, so every case builds its
	// own instead of copying a shared one.
	newResolver := func() *Resolver {
		return &Resolver{
			Config: ProxyConfig{
				Default: "http://default-proxy.example:8080",
				Sites: map[string]string{
					"api.example.com": "https://site-proxy.example:8443",
				},
				Keys: map[string]string{
					"key-a": "socks5://key-proxy.example:1080",
				},
			},
			SystemProxy: func(*http.Request) (*url.URL, error) {
				return systemURL, nil
			},
		}
	}

	tests := []struct {
		name       string
		request    ProxyRequest
		mutate     func(*Resolver)
		wantSource ProxySource
		wantURL    string
	}{
		{
			name:       "key specific wins",
			request:    ProxyRequest{KeyID: "key-a", TargetURL: target},
			wantSource: ProxySourceKey,
			wantURL:    "socks5://key-proxy.example:1080",
		},
		{
			name:       "site specific wins without key",
			request:    ProxyRequest{KeyID: "unknown", TargetURL: target},
			wantSource: ProxySourceSite,
			wantURL:    "https://site-proxy.example:8443",
		},
		{
			name:       "default wins without key or site",
			request:    ProxyRequest{KeyID: "unknown", TargetURL: mustURL(t, "https://other.example/v1")},
			wantSource: ProxySourceDefault,
			wantURL:    "http://default-proxy.example:8080",
		},
		{
			name:    "system wins without explicit configuration",
			request: ProxyRequest{KeyID: "unknown", TargetURL: target},
			mutate: func(value *Resolver) {
				value.Config = ProxyConfig{}
			},
			wantSource: ProxySourceSystem,
			wantURL:    systemURL.String(),
		},
		{
			name:    "direct is final fallback",
			request: ProxyRequest{KeyID: "unknown", TargetURL: target},
			mutate: func(value *Resolver) {
				value.Config = ProxyConfig{}
				value.SystemProxy = func(*http.Request) (*url.URL, error) { return nil, nil }
			},
			wantSource: ProxySourceDirect,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := newResolver()
			if test.mutate != nil {
				test.mutate(candidate)
			}
			resolved, err := candidate.Resolve(test.request)
			if err != nil {
				t.Fatalf("Resolve() error = %v", err)
			}
			if resolved.Source != test.wantSource {
				t.Fatalf("source = %q, want %q", resolved.Source, test.wantSource)
			}
			gotURL := ""
			if resolved.URL != nil {
				gotURL = resolved.URL.String()
			}
			if gotURL != test.wantURL {
				t.Fatalf("URL = %q, want %q", gotURL, test.wantURL)
			}
		})
	}
}

func TestResolverExplicitDirectStopsFallback(t *testing.T) {
	resolver := Resolver{
		Config: ProxyConfig{
			Default: "http://default-proxy.example:8080",
			Keys:    map[string]string{"key-a": "direct"},
		},
		SystemProxy: func(*http.Request) (*url.URL, error) {
			return mustURL(t, "http://system-proxy.example:8080"), nil
		},
	}
	resolved, err := resolver.Resolve(ProxyRequest{
		KeyID:     "key-a",
		TargetURL: mustURL(t, "https://api.example.com/v1"),
	})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if resolved.Source != ProxySourceKey || resolved.URL != nil {
		t.Fatalf("resolved = %#v, want key-specific direct decision", resolved)
	}
}

func TestResolverConfiguredSystemUsesSystemProxyWithoutFallingThrough(t *testing.T) {
	target := mustURL(t, "https://api.example.com/v1")
	systemURL := mustURL(t, "http://system-proxy.example:8080")
	resolver := Resolver{
		Config: ProxyConfig{
			Default: "http://default-proxy.example:8080",
			Keys:    map[string]string{"key-a": "system"},
		},
		SystemProxy: func(request *http.Request) (*url.URL, error) {
			if request.URL.Hostname() != target.Hostname() {
				t.Fatalf("system proxy target host = %q, want %q", request.URL.Hostname(), target.Hostname())
			}
			return systemURL, nil
		},
	}

	resolved, err := resolver.Resolve(ProxyRequest{KeyID: "key-a", TargetURL: target})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if resolved.Source != ProxySourceSystem {
		t.Fatalf("source = %q, want %q", resolved.Source, ProxySourceSystem)
	}
	if resolved.URL == nil || resolved.URL.String() != systemURL.String() {
		t.Fatalf("URL = %v, want %q", resolved.URL, systemURL)
	}
}

func TestResolverSupportsHTTPHTTPSAndSOCKS5(t *testing.T) {
	for _, value := range []string{
		"http://proxy.example:8080",
		"https://proxy.example:8443",
		"socks5://proxy.example:1080",
		"socks5h://proxy.example:1080",
	} {
		t.Run(value, func(t *testing.T) {
			resolver := Resolver{
				Config:      ProxyConfig{Default: value},
				SystemProxy: func(*http.Request) (*url.URL, error) { return nil, nil },
			}
			resolved, err := resolver.Resolve(ProxyRequest{TargetURL: mustURL(t, "https://api.example.com")})
			if err != nil {
				t.Fatalf("Resolve() error = %v", err)
			}
			if resolved.URL == nil || resolved.URL.String() != value {
				t.Fatalf("resolved URL = %v, want %q", resolved.URL, value)
			}
		})
	}
}

func TestTransportPoolIsolatesResolvedProxyConfigurations(t *testing.T) {
	pool := NewTransportPool(nil)
	first := ResolvedProxy{Source: ProxySourceKey, URL: mustURL(t, "http://proxy-a.example:8080")}
	second := ResolvedProxy{Source: ProxySourceKey, URL: mustURL(t, "http://proxy-b.example:8080")}
	direct := ResolvedProxy{Source: ProxySourceDirect}

	firstTransport, err := pool.Transport(first)
	if err != nil {
		t.Fatalf("first Transport() error = %v", err)
	}
	firstAgain, err := pool.Transport(first)
	if err != nil {
		t.Fatalf("repeated Transport() error = %v", err)
	}
	secondTransport, err := pool.Transport(second)
	if err != nil {
		t.Fatalf("second Transport() error = %v", err)
	}
	directTransport, err := pool.Transport(direct)
	if err != nil {
		t.Fatalf("direct Transport() error = %v", err)
	}

	if firstTransport != firstAgain {
		t.Fatal("same resolved proxy did not reuse its transport and connection pool")
	}
	if firstTransport == secondTransport {
		t.Fatal("different proxy configurations shared a transport")
	}
	if firstTransport == directTransport || secondTransport == directTransport {
		t.Fatal("proxied and direct configurations shared a transport")
	}

	firstProxy, err := firstTransport.Proxy(&http.Request{URL: mustURL(t, "https://api.example.com")})
	if err != nil {
		t.Fatalf("first transport Proxy() error = %v", err)
	}
	secondProxy, err := secondTransport.Proxy(&http.Request{URL: mustURL(t, "https://api.example.com")})
	if err != nil {
		t.Fatalf("second transport Proxy() error = %v", err)
	}
	if firstProxy.String() != first.URL.String() || secondProxy.String() != second.URL.String() {
		t.Fatalf("transport proxies = %v and %v, want %v and %v", firstProxy, secondProxy, first.URL, second.URL)
	}
	if directTransport.Proxy != nil {
		t.Fatal("direct transport retained a proxy function")
	}
}

func TestTransportPoolConfiguresSOCKS5Dialer(t *testing.T) {
	pool := NewTransportPool(nil)
	transport, err := pool.Transport(ResolvedProxy{
		Source: ProxySourceDefault,
		URL:    mustURL(t, "socks5://user:password@proxy.example:1080"),
	})
	if err != nil {
		t.Fatalf("Transport() error = %v", err)
	}
	if transport.Proxy != nil {
		t.Fatal("SOCKS5 transport unexpectedly configured HTTP Proxy function")
	}
	if transport.DialContext == nil {
		t.Fatal("SOCKS5 transport did not configure DialContext")
	}
}
