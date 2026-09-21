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
)

// Failure explains why a probe did not produce a model list.
type Failure struct {
	// Reason is one of the Reason constants.
	Reason string
	// Status is the upstream's HTTP status when it answered.
	Status  int
	Message string
}

func (f Failure) Error() string { return f.Message }

// Unwrap makes errors.Is(err, ConfigError) true for every probe failure.
func (f Failure) Unwrap() error { return ConfigError }

func failure(reason string, status int, message string) error {
	return Failure{Reason: reason, Status: status, Message: message}
}

// maxBodyBytes bounds how much of a model listing is read, so a hostile or
// broken upstream cannot make the gateway buffer an unbounded response.
const maxBodyBytes = int64(1 << 20)

// Request describes one upstream to ask for its model list.
type Request struct {
	// BaseURL is the upstream's root address, the same value a site stores.
	BaseURL string
	// APIKey is the credential to present. Both the bearer scheme and the
	// x-api-key header are sent, because upstreams differ in which one they read
	// and neither is sensitive to the other being present.
	APIKey string
	// Headers are extra request headers, which is how a platform that needs a
	// vendor-specific header or a different scheme is reached.
	Headers map[string]string
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

	var lastErr error
	for _, endpoint := range candidateEndpoints(base) {
		models, err := c.fetch(probeContext, endpoint, request)
		if err == nil {
			return Result{Models: models, Endpoint: endpoint}, nil
		}
		if errors.Is(err, errNotFound) {
			lastErr = err
			continue
		}
		return Result{}, err
	}
	if lastErr != nil {
		return Result{}, failure(ReasonNoModelEndpoint, http.StatusNotFound,
			fmt.Sprintf("the upstream answered 404 for %s and %s", base.String()+"/v1/models", base.String()+"/models"))
	}
	return Result{}, failure(ReasonUnreachable, 0, "the upstream did not answer")
}

// errNotFound marks an endpoint the upstream does not serve.
var errNotFound = errors.New("endpoint not found")

func (c *Client) fetch(ctx context.Context, endpoint string, request Request) ([]string, error) {
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

	client := c.HTTP
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

// candidateEndpoints lists the URLs to try, OpenAI's path first unless the
// address already carries a version segment.
func candidateEndpoints(base *url.URL) []string {
	root := strings.TrimRight(base.String(), "/")
	if strings.HasSuffix(base.Path, "/v1") || strings.Contains(base.Path, "/v1/") {
		return []string{root + "/models", root + "/v1/models"}
	}
	return []string{root + "/v1/models", root + "/models"}
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
		for _, key := range []string{"id", "name", "model"} {
			if name, ok := typed[key].(string); ok {
				if trimmed := strings.TrimSpace(name); trimmed != "" {
					names[trimmed] = struct{}{}
				}
			}
		}
	case string:
		if trimmed := strings.TrimSpace(typed); trimmed != "" {
			names[trimmed] = struct{}{}
		}
	}
}
