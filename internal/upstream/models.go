// Package upstream talks to an upstream service on the console's behalf. It
// exists so an operator can pick the models an upstream actually serves instead
// of typing model names from memory, which is what keeps a route from pointing
// at a model the upstream would reject.
package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/yhw5231/fluxgate/internal/proxy"
)

// ConfigError reports a probe that did not produce a model list. Every failure
// of this kind wraps it, so a caller can tell "the gateway could not make this
// request" from a programming fault.
var ConfigError = errors.New("upstream probe failed")

// Reasons a probe can fail for. They are stable codes so a client can phrase the
// failure in its own language instead of parsing an English sentence.
const (
	// ReasonInvalidRequest means the gateway would not make the request: no
	// address, an address that is not an HTTP URL, or nothing to authenticate
	// with.
	ReasonInvalidRequest = "invalid_request"
	// ReasonNoModelEndpoint means the upstream serves no model listing at either
	// candidate path.
	ReasonNoModelEndpoint = "no_model_endpoint"
	// ReasonCredentialRejected means the upstream refused the credential.
	ReasonCredentialRejected = "credential_rejected"
	// ReasonUpstreamStatus means the upstream answered with an error status.
	ReasonUpstreamStatus = "upstream_status"
	// ReasonUnreadable means the answer was not a model listing the gateway
	// understands.
	ReasonUnreadable = "unreadable_response"
	// ReasonUnreachable means the request never reached the upstream.
	ReasonUnreachable = "unreachable"
	// ReasonInvalidProxy means the proxy the probe was told to use cannot be
	// used, so no request was made.
	ReasonInvalidProxy = "invalid_proxy"
)

// Failure explains why a probe did not produce a model list.
type Failure struct {
	// Reason is one of the Reason constants.
	Reason string
	// Status is the upstream's HTTP status when it answered.
	Status  int
	Message string
	// Params carries what the console's message template needs to phrase the
	// failure: which address was tried, and how the request left the gateway.
	// They travel as data rather than prose so the console renders them in its
	// own language.
	Params map[string]any
}

func (f Failure) Error() string { return f.Message }

// Unwrap makes errors.Is(err, ConfigError) true for every probe failure.
func (f Failure) Unwrap() error { return ConfigError }

func failure(reason string, status int, message string) error {
	return Failure{Reason: reason, Status: status, Message: message}
}

// describedFailure adds the context every outbound failure has in common: the
// address that was tried and the proxy, if any, the request left through. Both
// are what an operator has to check, so both travel with the failure.
func describedFailure(err error, endpoint string, route outbound) error {
	var failed Failure
	if !errors.As(err, &failed) {
		return err
	}
	params := map[string]any{"endpoint": endpoint, "proxy_source": route.Source}
	if route.URL != "" {
		params["proxy_url"] = route.URL
	}
	for name, value := range failed.Params {
		params[name] = value
	}
	failed.Params = params
	return failed
}

// outbound records how a probe leaves the gateway, so a failure can say which
// proxy carried it instead of leaving the operator to guess.
type outbound struct {
	// Source is the proxy decision: "direct", "system", or the configuration
	// layer that supplied the proxy.
	Source string
	// URL is the proxy address when one is in use.
	URL string
}

// maxBodyBytes bounds how much of a model listing is read, so a hostile or
// broken upstream cannot make the gateway buffer an unbounded response.
const maxBodyBytes = int64(1 << 20)

// Request describes one upstream to ask for its model list.
type Request struct {
	// BaseURL is the upstream's root address, the same value a site stores. A
	// trailing slash and a version segment are both optional: the spellings an
	// operator may type of one address are probed the same way.
	BaseURL string
	// APIKey is the credential to present. Both the bearer scheme and the
	// x-api-key header are sent, because upstreams differ in which one they read
	// and neither is sensitive to the other being present.
	APIKey string
	// Headers are extra request headers, which is how a platform that needs a
	// vendor-specific header or a different scheme is reached.
	Headers map[string]string
	// ProxyURL is the outbound proxy to probe through: a proxy address, or the
	// keywords "system" and "direct" (also "none"). An empty value follows the
	// system proxy settings, which is also what the routing engine falls back to
	// when no proxy is configured for an upstream.
	ProxyURL string
}

// Result is what an upstream answered.
type Result struct {
	Models []string
	// Endpoint is the URL that answered, so an operator can see which of the
	// candidate paths the upstream actually serves.
	Endpoint string
}

// Client fetches model listings.
type Client struct {
	// HTTP overrides how a probe connects. When set it is used as given, which
	// is how a test drives the probe without a network.
	HTTP *http.Client
	// Timeout bounds one probe. A console request should not hang on an upstream
	// that never answers.
	Timeout time.Duration
}

// Models asks an upstream which models it serves. The OpenAI listing path is
// tried first, then the bare one, so an upstream that mounts its API at the
// root works without the operator having to say which shape it is.
func (c *Client) Models(ctx context.Context, request Request) (Result, error) {
	base, err := parseBaseURL(request.BaseURL)
	if err != nil {
		return Result{}, err
	}
	if strings.TrimSpace(request.APIKey) == "" && len(request.Headers) == 0 {
		return Result{}, failure(ReasonInvalidRequest, 0, "a key or a request header is needed to ask for the model list")
	}

	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	probeContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	client, route, release, err := c.dispatch(request, base)
	if err != nil {
		return Result{}, err
	}
	defer release()

	endpoints := candidateEndpoints(base)
	var lastErr error
	for _, endpoint := range endpoints {
		models, err := c.fetch(probeContext, client, endpoint, request)
		if err == nil {
			return Result{Models: models, Endpoint: endpoint}, nil
		}
		if errors.Is(err, errNotFound) {
			lastErr = err
			continue
		}
		return Result{}, describedFailure(err, endpoint, route)
	}
	if lastErr != nil {
		params := map[string]any{"endpoints": endpoints, "proxy_source": route.Source}
		if route.URL != "" {
			params["proxy_url"] = route.URL
		}
		return Result{}, Failure{
			Reason:  ReasonNoModelEndpoint,
			Status:  http.StatusNotFound,
			Message: fmt.Sprintf("the upstream serves no model listing at %s", strings.Join(endpoints, " or ")),
			Params:  params,
		}
	}
	params := map[string]any{"proxy_source": route.Source}
	if route.URL != "" {
		params["proxy_url"] = route.URL
	}
	return Result{}, Failure{
		Reason:  ReasonUnreachable,
		Status:  0,
		Message: "the upstream did not answer",
		Params:  params,
	}
}

// dispatch builds the client a probe leaves through. The proxy the form carries
// wins; with none configured the request follows the system proxy settings,
// which is what the routing engine does for an upstream without one.
func (c *Client) dispatch(request Request, target *url.URL) (*http.Client, outbound, func(), error) {
	if c.HTTP != nil {
		// An injected client is a test seam; how it connects is not this
		// package's business.
		return c.HTTP, outbound{}, func() {}, nil
	}
	resolver := &proxy.Resolver{Config: proxy.ProxyConfig{Default: strings.TrimSpace(request.ProxyURL)}}
	resolved, err := resolver.Resolve(proxy.ProxyRequest{TargetURL: target})
	if err != nil {
		return nil, outbound{}, func() {}, failure(ReasonInvalidProxy, 0, err.Error())
	}
	route := outbound{Source: string(resolved.Source)}
	if resolved.URL != nil {
		route.URL = resolved.URL.String()
	}
	pool := proxy.NewTransportPool(nil)
	transport, err := pool.Transport(resolved)
	if err != nil {
		return nil, route, func() {}, failure(ReasonInvalidProxy, 0, err.Error())
	}
	return &http.Client{Transport: transport}, route, transport.CloseIdleConnections, nil
}

// errNotFound marks an endpoint the upstream does not serve.
var errNotFound = errors.New("endpoint not found")

func (c *Client) fetch(ctx context.Context, client *http.Client, endpoint string, request Request) ([]string, error) {
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("build model list request: %w", err)
	}
	httpRequest.Header.Set("Accept", "application/json")
	if key := strings.TrimSpace(request.APIKey); key != "" {
		httpRequest.Header.Set("Authorization", "Bearer "+key)
		httpRequest.Header.Set("X-Api-Key", key)
	}
	for name, value := range request.Headers {
		httpRequest.Header.Set(name, value)
	}

	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(httpRequest)
	if err != nil {
		return nil, failure(ReasonUnreachable, 0, err.Error())
	}
	defer response.Body.Close()

	body, err := io.ReadAll(io.LimitReader(response.Body, maxBodyBytes))
	if err != nil {
		return nil, failure(ReasonUnreachable, response.StatusCode, "reading the model list failed: "+err.Error())
	}
	switch {
	case response.StatusCode == http.StatusNotFound || response.StatusCode == http.StatusMethodNotAllowed:
		return nil, errNotFound
	case response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden:
		return nil, failure(ReasonCredentialRejected, response.StatusCode,
			fmt.Sprintf("the upstream rejected the credential (HTTP %d)", response.StatusCode))
	case response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices:
		return nil, failure(ReasonUpstreamStatus, response.StatusCode,
			fmt.Sprintf("the upstream answered HTTP %d", response.StatusCode))
	}

	models, err := parseModels(body)
	if err != nil {
		return nil, failure(ReasonUnreadable, response.StatusCode, err.Error())
	}
	return models, nil
}

// candidateEndpoints lists the model-listing URLs to try, most likely first.
//
// The address decides the order. One that names no version is probed at the
// versioned path and then at the bare one, which is what lets an upstream be
// written as https://host or https://host/; one that already names a version is
// probed there first and then at its parent, because a listing also sits
// directly under the API root — /openai/v1 names the same API as /openai, and an
// upstream mounted at the root may serve its listing at /models.
func candidateEndpoints(base *url.URL) []string {
	root := strings.TrimRight(base.String(), "/")
	path := strings.TrimRight(base.Path, "/")
	version, versioned := versionSegment(path)
	if !versioned {
		return []string{root + "/v1/models", root + "/models"}
	}
	parent := strings.TrimSuffix(root, "/"+version)
	return []string{root + "/models", parent + "/models"}
}

// versionSegment reports whether a path's last segment is an API version such as
// v1 or v1beta1, which is what makes the parent path worth probing.
func versionSegment(path string) (string, bool) {
	segment := path[strings.LastIndex(path, "/")+1:]
	if len(segment) < 2 || (segment[0] != 'v' && segment[0] != 'V') {
		return "", false
	}
	digits := false
	for index := 1; index < len(segment); index++ {
		character := segment[index]
		switch {
		case character >= '0' && character <= '9':
			digits = true
		case character >= 'a' && character <= 'z', character >= 'A' && character <= 'Z',
			character == '.', character == '-', character == '_':
		default:
			return "", false
		}
	}
	if !digits {
		return "", false
	}
	return segment, true
}

func parseBaseURL(value string) (*url.URL, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return nil, failure(ReasonInvalidRequest, 0, "an API address is required")
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return nil, failure(ReasonInvalidRequest, 0, "the API address is not a valid URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, failure(ReasonInvalidRequest, 0, "the API address must start with http:// or https://")
	}
	if parsed.Host == "" {
		return nil, failure(ReasonInvalidRequest, 0, "the API address must include a host")
	}
	return parsed, nil
}

// parseModels reads a model listing. Providers agree on the OpenAI shape, but
// the id also appears as a plain string and under a few other keys, so the
// shapes that carry the same information are all accepted rather than making an
// operator rename a working upstream's output.
func parseModels(body []byte) ([]string, error) {
	var decoded any
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, errors.New("the upstream did not answer a JSON model list")
	}
	names := map[string]struct{}{}
	collectModels(decoded, names)
	if len(names) == 0 {
		return nil, errors.New("the upstream answered with no model names")
	}
	models := make([]string, 0, len(names))
	for name := range names {
		models = append(models, name)
	}
	sort.Strings(models)
	return models, nil
}

// collectModels walks a decoded listing for model names. It descends into the
// containers a listing is known to use instead of walking the whole document,
// so an unrelated "id" field cannot be mistaken for a model.
//
// One entry names one model: the first of the identifier keys that carries a
// value is the name, and the others are not further models. An entry that
// carries a display label beside its identifier is a common shape — a platform
// that serves a model from a channel group may list
// {"id":"cn:deepseek-v4.1-flash","name":"Deepseek-V4.1-Flash"} — and that label
// is a name the upstream would not accept, and the same model under another
// spelling to everything downstream. Reading it as a model in its own right is
// what offers an operator a choice that cannot be routed and lets the label
// answer for the model: the two reduce to one identity, so the lowercase model
// ends up named by the label.
//
// The identifier keys are tried in the order the shapes put them in, so a
// listing of plain strings, of "id"-keyed entries, and of "name"-keyed entries
// all still read.
func collectModels(value any, names map[string]struct{}) {
	switch typed := value.(type) {
	case []any:
		for _, entry := range typed {
			collectModels(entry, names)
		}
	case map[string]any:
		for _, key := range []string{"data", "models", "result"} {
			if nested, present := typed[key]; present {
				collectModels(nested, names)
			}
		}
		for _, key := range []string{"id", "model", "name"} {
			name, ok := typed[key].(string)
			if !ok {
				continue
			}
			trimmed := strings.TrimSpace(name)
			if trimmed == "" {
				continue
			}
			names[trimmed] = struct{}{}
			break
		}
	case string:
		if trimmed := strings.TrimSpace(typed); trimmed != "" {
			names[trimmed] = struct{}{}
		}
	}
}
