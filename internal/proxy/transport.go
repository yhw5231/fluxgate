package proxy

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"golang.org/x/net/proxy"
)

// ProxySource identifies the precedence layer that supplied a proxy.
type ProxySource string

const (
	ProxySourceKey     ProxySource = "key"
	ProxySourceSite    ProxySource = "site"
	ProxySourceDefault ProxySource = "default"
	ProxySourceSystem  ProxySource = "system"
	ProxySourceDirect  ProxySource = "direct"
)

// ProxyConfig contains explicit proxy settings. An empty value falls through
// to the next precedence layer. The value "direct" explicitly disables proxying.
type ProxyConfig struct {
	Default string
	Sites   map[string]string
	Keys    map[string]string
}

// ProxyRequest contains the attributes used to resolve a proxy.
type ProxyRequest struct {
	KeyID     string
	TargetURL *url.URL
}

// ResolvedProxy is a canonical proxy decision suitable for transport pooling.
type ResolvedProxy struct {
	Source ProxySource
	URL    *url.URL
}

// CacheKey identifies one isolated connection pool.
func (r ResolvedProxy) CacheKey() string {
	if r.URL == nil {
		return string(r.Source) + ":direct"
	}
	return string(r.Source) + ":" + r.URL.String()
}

// Resolver applies key, site, default, system, and direct precedence.
//
// Config is replaced by a management-console write while requests are in
// flight, so it is read through a snapshot taken under mu rather than directly.
type Resolver struct {
	SystemProxy func(*http.Request) (*url.URL, error)

	mu     sync.RWMutex
	Config ProxyConfig
}

// SetConfig replaces the proxy configuration in place. The resolver object is
// never swapped, so an in-flight request finishes with the decisions it started
// with.
func (r *Resolver) SetConfig(config ProxyConfig) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Config = config
}

// config returns the current configuration. The maps are shared rather than
// copied, which is safe because a caller only ever reads them and SetConfig
// replaces the whole struct instead of mutating one in place.
func (r *Resolver) config() ProxyConfig {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.Config
}

func (r *Resolver) Resolve(request ProxyRequest) (ResolvedProxy, error) {
	config := r.config()
	if value := strings.TrimSpace(config.Keys[request.KeyID]); value != "" {
		return r.resolveConfiguredProxy(ProxySourceKey, value, request.TargetURL)
	}
	if request.TargetURL != nil {
		host := strings.ToLower(request.TargetURL.Hostname())
		if value := strings.TrimSpace(config.Sites[host]); value != "" {
			return r.resolveConfiguredProxy(ProxySourceSite, value, request.TargetURL)
		}
	}
	if value := strings.TrimSpace(config.Default); value != "" {
		return r.resolveConfiguredProxy(ProxySourceDefault, value, request.TargetURL)
	}
	return r.resolveSystemProxy(request.TargetURL)
}

func (r *Resolver) resolveConfiguredProxy(source ProxySource, value string, targetURL *url.URL) (ResolvedProxy, error) {
	if strings.EqualFold(value, "system") {
		return r.resolveSystemProxy(targetURL)
	}
	return parseResolvedProxy(source, value)
}

func (r *Resolver) resolveSystemProxy(targetURL *url.URL) (ResolvedProxy, error) {
	systemProxy := r.SystemProxy
	if systemProxy == nil {
		systemProxy = http.ProxyFromEnvironment
	}
	if targetURL != nil && systemProxy != nil {
		httpRequest := &http.Request{URL: targetURL}
		proxyURL, err := systemProxy(httpRequest)
		if err != nil {
			return ResolvedProxy{}, fmt.Errorf("resolve system proxy: %w", err)
		}
		if proxyURL != nil {
			return ResolvedProxy{Source: ProxySourceSystem, URL: proxyURL}, nil
		}
	}
	return ResolvedProxy{Source: ProxySourceDirect}, nil
}

func parseResolvedProxy(source ProxySource, value string) (ResolvedProxy, error) {
	if strings.EqualFold(value, "direct") || strings.EqualFold(value, "none") {
		return ResolvedProxy{Source: source}, nil
	}
	proxyURL, err := url.Parse(value)
	if err != nil {
		return ResolvedProxy{}, fmt.Errorf("parse %s proxy: %w", source, err)
	}
	switch strings.ToLower(proxyURL.Scheme) {
	case "http", "https", "socks5", "socks5h":
	default:
		return ResolvedProxy{}, fmt.Errorf("unsupported %s proxy scheme %q", source, proxyURL.Scheme)
	}
	if proxyURL.Host == "" {
		return ResolvedProxy{}, fmt.Errorf("%s proxy requires a host", source)
	}
	return ResolvedProxy{Source: source, URL: proxyURL}, nil
}

// TransportPool owns one http.Transport and connection pool per canonical
// resolved proxy decision.
type TransportPool struct {
	mu         sync.Mutex
	transports map[string]*http.Transport
	base       *http.Transport
}

func NewTransportPool(base *http.Transport) *TransportPool {
	if base == nil {
		base = http.DefaultTransport.(*http.Transport).Clone()
	} else {
		base = base.Clone()
	}
	return &TransportPool{transports: make(map[string]*http.Transport), base: base}
}

func (p *TransportPool) Transport(resolved ResolvedProxy) (*http.Transport, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	key := resolved.CacheKey()
	if transport := p.transports[key]; transport != nil {
		return transport, nil
	}
	transport := p.base.Clone()
	transport.Proxy = nil
	if resolved.URL != nil {
		switch strings.ToLower(resolved.URL.Scheme) {
		case "http", "https":
			transport.Proxy = http.ProxyURL(resolved.URL)
		case "socks5", "socks5h":
			dialer, err := socksDialer(resolved.URL)
			if err != nil {
				return nil, err
			}
			transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
				return dialer.Dial(network, address)
			}
		default:
			return nil, fmt.Errorf("unsupported proxy scheme %q", resolved.URL.Scheme)
		}
	}
	p.transports[key] = transport
	return transport, nil
}

func socksDialer(proxyURL *url.URL) (proxy.Dialer, error) {
	var auth *proxy.Auth
	if proxyURL.User != nil {
		password, _ := proxyURL.User.Password()
		auth = &proxy.Auth{User: proxyURL.User.Username(), Password: password}
	}
	dialer, err := proxy.SOCKS5("tcp", proxyURL.Host, auth, proxy.Direct)
	if err != nil {
		return nil, fmt.Errorf("create SOCKS5 dialer: %w", err)
	}
	return dialer, nil
}

// Client returns a client backed by the isolated pool for the resolved proxy.
func (p *TransportPool) Client(resolved ResolvedProxy) (*http.Client, error) {
	transport, err := p.Transport(resolved)
	if err != nil {
		return nil, err
	}
	return &http.Client{Transport: transport}, nil
}
