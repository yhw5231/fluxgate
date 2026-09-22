package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/yhw5231/fluxgate/internal/domain"
	"github.com/yhw5231/fluxgate/internal/transform"
)

const (
	maxBufferedErrorBody = int64(1 << 20)
	// errorSnippetBytes bounds how much of a failed upstream response is kept as
	// the explanation of the failure. An upstream names its cause in the first few
	// hundred bytes; keeping more would only fill the request log with text nobody
	// reads.
	errorSnippetBytes = 512
)

// Result contains the final upstream response and the successful attempt.
type Result struct {
	Response *http.Response
	Attempt  domain.Attempt
	// Trace lists every upstream attempt the request made, in order, including the
	// attempts that failed. It is filled in on the error path as well, so a caller
	// can record what happened to a request that could not be served.
	Trace domain.RequestTrace
}

// DispatchPolicy is everything the engine consults while a request is being
// retried: how often it may try, and how far it may move away from the channel
// it started on.
type DispatchPolicy struct {
	Retry    domain.RetryPolicy
	Failover domain.FailoverPolicy
}

// Engine performs bounded same-channel retries and channel failover before a
// response is exposed to the downstream client.
type Engine struct {
	Selector      domain.Selector
	Observer      domain.FailureObserver
	Client        *http.Client
	ProxyResolver *Resolver
	TransportPool *TransportPool
	Policy        domain.RetryPolicy
	// Failover is the static failover configuration. A nil value means the
	// engine's own default: a failed channel may be replaced by any other
	// eligible one.
	Failover *domain.FailoverPolicy
	Sleep    func(context.Context, time.Duration) error
	Now      func() time.Time

	// installed holds the policy a console write installed, so a retry or
	// failover change takes effect for the next request instead of at the next
	// restart. It is read once per request, which is what keeps one request
	// governed by one policy.
	installed atomic.Pointer[DispatchPolicy]
}

// SetPolicy replaces the retry and failover policy. It is safe to call while
// requests are in flight.
func (e *Engine) SetPolicy(policy DispatchPolicy) {
	if e == nil {
		return
	}
	e.installed.Store(&policy)
}

// currentPolicy returns the installed policy, falling back to the static fields
// so an engine assembled without a console keeps its configured behavior.
func (e *Engine) currentPolicy() DispatchPolicy {
	if stored := e.installed.Load(); stored != nil {
		return *stored
	}
	failover := domain.FailoverPolicy{Enabled: true, CrossUpstream: true}
	if e.Failover != nil {
		failover = *e.Failover
	}
	return DispatchPolicy{Retry: e.Policy, Failover: failover}
}

// Forward transforms and dispatches an OpenAI-compatible request. Failed
// retryable responses are closed internally and are never returned to callers.
func (e *Engine) Forward(ctx context.Context, input domain.Request) (Result, error) {
	if e.Selector == nil {
		return Result{}, errors.New("proxy selector is required")
	}
	client := e.Client
	if client == nil {
		client = http.DefaultClient
	}
	policy := e.currentPolicy()
	retry := normalizePolicy(policy.Retry)
	sleep := e.Sleep
	if sleep == nil {
		sleep = sleepContext
	}
	now := e.Now
	if now == nil {
		now = time.Now
	}

	excluded := make(map[string]struct{})
	attemptsByChannel := make(map[string]int)
	var lastFailure domain.Failure
	// trace is what the request log keeps: every attempt this request made, in
	// order, whichever way the request ends.
	var trace domain.RequestTrace
	// pinned records the channel the request started on, which is what a request
	// with failover switched off, or with cross-upstream failover switched off,
	// has to stay with.
	pinned := domain.Selection{}

	for attemptNumber := 1; attemptNumber <= retry.MaxAttempts; attemptNumber++ {
		selection, err := e.Selector.Select(e.selectionRequest(input, excluded, pinned, policy.Failover))
		if err != nil {
			if lastFailure.Err != nil || lastFailure.StatusCode != 0 {
				return Result{Trace: trace}, finalError(lastFailure)
			}
			return Result{Trace: trace}, err
		}
		if pinned.Channel.ID == "" {
			pinned = selection
		}

		channel := selection.Channel
		channelAttempt := attemptsByChannel[channel.ID] + 1
		attemptsByChannel[channel.ID] = channelAttempt
		attempt := domain.Attempt{
			Number:       attemptNumber,
			ChannelID:    channel.ID,
			ChannelIndex: channelAttempt,
			KeyID:        channel.APIKey,
			Model:        selection.Model,
			BreakerMode:  channel.BreakerMode,
			StartedAt:    now(),
			RequestID:    input.RequestID,
		}
		traced := domain.AttemptTrace{
			Number:      attemptNumber,
			ChannelID:   channel.ID,
			ChannelName: channel.Name,
			Model:       selection.Model,
			StartedAt:   attempt.StartedAt,
		}

		body, err := transform.ApplyJSON(input.Body, selection.Model, channel.Transform)
		if err != nil {
			return Result{Trace: trace}, err
		}
		request, cancelRequest, err := buildRequest(ctx, input, channel, body)
		if err != nil {
			return Result{Trace: trace}, err
		}

		dispatchClient := client
		if e.ProxyResolver != nil {
			resolved, resolveErr := e.ProxyResolver.Resolve(ProxyRequest{KeyID: channel.APIKey, TargetURL: request.URL})
			if resolveErr != nil {
				cancelRequest()
				return Result{Trace: trace}, resolveErr
			}
			pool := e.TransportPool
			if pool == nil {
				pool = NewTransportPool(nil)
			}
			dispatchClient, err = pool.Client(resolved)
			if err != nil {
				cancelRequest()
				return Result{Trace: trace}, err
			}
		}

		response, dispatchErr := dispatchClient.Do(request)
		if response != nil {
			response.Body = &cancelReadCloser{ReadCloser: response.Body, cancel: cancelRequest}
		} else {
			cancelRequest()
		}
		traced.DurationMS = now().Sub(attempt.StartedAt).Milliseconds()
		if dispatchErr == nil && response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices {
			traced.StatusCode = response.StatusCode
			trace.Attempts = append(trace.Attempts, traced)
			if e.Observer != nil {
				e.Observer.RecordSuccess(attempt)
			}
			return Result{Response: response, Attempt: attempt, Trace: trace}, nil
		}

		statusCode := 0
		if response != nil {
			statusCode = response.StatusCode
		}
		retryable := dispatchErr != nil || isRetryableStatus(retry, statusCode)
		lastFailure = domain.Failure{
			Attempt:    attempt,
			StatusCode: statusCode,
			Err:        dispatchErr,
			Retryable:  retryable,
		}
		if e.Observer != nil {
			e.Observer.RecordFailure(lastFailure)
		}

		traced.StatusCode = statusCode
		traced.Retryable = retryable
		if dispatchErr != nil {
			traced.Error = dispatchErr.Error()
		}
		// The upstream's own words are the explanation of the failure, so they are
		// read before the response is either passed on or dropped.
		if response != nil && statusCode >= http.StatusBadRequest {
			traced.Response = recordErrorBody(response, channel.APIKey)
		}
		trace.Attempts = append(trace.Attempts, traced)

		if !retryable {
			return Result{Response: response, Attempt: attempt, Trace: trace}, nil
		}
		if response != nil {
			drainAndClose(response.Body)
		}
		// A channel is given up only when the request is allowed to leave it, so
		// switching failover off retries the same channel instead of moving on.
		if policy.Failover.Enabled && channelAttempt >= retry.MaxAttemptsPerChannel {
			excluded[channel.ID] = struct{}{}
		}
		if attemptNumber >= retry.MaxAttempts {
			break
		}
		if err := sleep(ctx, backoff(retry, attemptNumber)); err != nil {
			return Result{Trace: trace}, err
		}
	}

	return Result{Trace: trace}, finalError(lastFailure)
}

// selectionRequest builds one lookup, restricted to the channel or the upstream
// the request has to stay with while failover is limited.
func (e *Engine) selectionRequest(input domain.Request, excluded map[string]struct{}, pinned domain.Selection, failover domain.FailoverPolicy) domain.SelectionRequest {
	request := domain.SelectionRequest{
		Model:    input.Model,
		Policy:   input.Policy,
		Excluded: excluded,
	}
	if pinned.Channel.ID == "" {
		return request
	}
	switch {
	case !failover.Enabled:
		request.OnlyChannel = pinned.Channel.ID
	case !failover.CrossUpstream:
		request.OnlySiteID = pinned.Channel.SiteID
	}
	return request
}

func buildRequest(ctx context.Context, input domain.Request, channel domain.Channel, body []byte) (*http.Request, context.CancelFunc, error) {
	baseURL, err := url.Parse(strings.TrimRight(channel.BaseURL, "/"))
	if err != nil {
		return nil, nil, fmt.Errorf("parse channel base URL: %w", err)
	}
	target := upstreamTarget(baseURL, input.Path)

	requestContext := ctx
	cancel := func() {}
	if channel.RequestTimeout > 0 {
		requestContext, cancel = context.WithTimeout(ctx, channel.RequestTimeout)
	}
	request, err := http.NewRequestWithContext(requestContext, input.Method, target.String(), bytes.NewReader(body))
	if err != nil {
		cancel()
		return nil, nil, fmt.Errorf("build upstream request: %w", err)
	}
	request.Header = transform.ApplyHeaders(input.Headers, channel.Transform, channel.APIKey)
	request.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}
	return request, cancel, nil
}

// upstreamTarget resolves the path of one downstream request against a channel's
// base address.
//
// The base address is the upstream's API root — the value an OpenAI client would
// be configured with, such as "https://host/v1", or "https://host/openai/v1" for
// an upstream mounted under a sub-path. That path is part of where the request
// has to go and is never dropped: a site whose API lives under /openai must not
// be called at its host root.
//
// The gateway's own routes carry the version segment, so the base address
// supplies it once: a request for /v1/chat/completions reaches
// "<base path>/chat/completions". A base address without a path keeps the
// request path as it arrived, because the upstream schema itself stores a site
// as a bare host and the version segment is then the only thing naming the API.
func upstreamTarget(baseURL *url.URL, requestPath string) *url.URL {
	requested := "/" + strings.TrimLeft(requestPath, "/")
	target := *baseURL
	basePath := strings.TrimRight(target.Path, "/")
	if basePath == "" {
		target.Path = requested
	} else {
		target.Path = basePath + strings.TrimPrefix(requested, "/v1")
	}
	// The path is rebuilt from decoded segments, so a stale escape of the base
	// address would describe a path this URL no longer has.
	target.RawPath = ""
	return &target
}

func normalizePolicy(policy domain.RetryPolicy) domain.RetryPolicy {
	if policy.MaxAttempts <= 0 {
		policy.MaxAttempts = 1
	}
	if policy.MaxAttemptsPerChannel <= 0 {
		policy.MaxAttemptsPerChannel = 1
	}
	if policy.BaseBackoff < 0 {
		policy.BaseBackoff = 0
	}
	if policy.MaxBackoff <= 0 {
		policy.MaxBackoff = policy.BaseBackoff
	}
	return policy
}

func isRetryableStatus(policy domain.RetryPolicy, statusCode int) bool {
	_, ok := policy.RetryStatuses[statusCode]
	return ok
}

func backoff(policy domain.RetryPolicy, attempt int) time.Duration {
	if policy.BaseBackoff <= 0 {
		return 0
	}
	delay := policy.BaseBackoff
	for index := 1; index < attempt; index++ {
		if delay >= policy.MaxBackoff/2 {
			return policy.MaxBackoff
		}
		delay *= 2
	}
	if delay > policy.MaxBackoff {
		return policy.MaxBackoff
	}
	return delay
}

func sleepContext(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type cancelReadCloser struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (body *cancelReadCloser) Close() error {
	err := body.ReadCloser.Close()
	body.cancel()
	return err
}

// prefixedBody re-emits bytes that were read ahead and put back, so a response
// whose beginning was read for the request log still reaches the client whole.
type prefixedBody struct {
	io.Reader
	io.Closer
}

// recordErrorBody reads the beginning of a failed upstream response for the
// request log and puts it back, so the upstream's own explanation of the failure
// can be recorded while the response still reaches the client unchanged. The
// response's body is replaced by one that yields the same bytes.
//
// The credential the attempt presented is redacted from the text: an upstream
// that echoes the key it rejected would otherwise put it into the record, and
// into the error the client is answered with.
func recordErrorBody(response *http.Response, credential string) string {
	original := response.Body
	if original == nil {
		return ""
	}
	prefix := make([]byte, errorSnippetBytes)
	count, _ := io.ReadFull(original, prefix)
	prefix = prefix[:count]
	if count == 0 {
		return ""
	}
	response.Body = &prefixedBody{
		Reader: io.MultiReader(bytes.NewReader(prefix), original),
		Closer: original,
	}
	snippet := sanitizeSnippet(prefix)
	if key := strings.TrimSpace(credential); key != "" {
		snippet = strings.ReplaceAll(snippet, key, "[redacted]")
	}
	return snippet
}

// sanitizeSnippet renders the beginning of an upstream body as text a record can
// hold: whitespace runs collapse to one space, so a JSON error stays on one line,
// and anything that is not printable text is replaced, so a compressed or binary
// body cannot put control characters into the log. A body with nothing readable
// in it at all is dropped rather than recorded as a row of replacement
// characters.
func sanitizeSnippet(raw []byte) string {
	decoded := strings.ToValidUTF8(string(raw), "\uFFFD")
	builder := strings.Builder{}
	builder.Grow(len(decoded))
	spaced := false
	for _, character := range decoded {
		if unicode.IsSpace(character) || character < 0x20 || character == 0x7f {
			spaced = true
			continue
		}
		if spaced && builder.Len() > 0 {
			builder.WriteRune(' ')
		}
		spaced = false
		builder.WriteRune(character)
	}
	text := builder.String()
	for _, character := range text {
		if character != '\uFFFD' && unicode.IsGraphic(character) {
			return text
		}
	}
	return ""
}

func drainAndClose(body io.ReadCloser) {
	if body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(body, maxBufferedErrorBody))
	_ = body.Close()
}

func finalError(failure domain.Failure) error {
	if failure.Err != nil {
		return fmt.Errorf("upstream request failed after attempt %d: %w", failure.Attempt.Number, failure.Err)
	}
	if failure.StatusCode != 0 {
		return fmt.Errorf("upstream request failed after attempt %d with status %d", failure.Attempt.Number, failure.StatusCode)
	}
	return errors.New("upstream request failed")
}
