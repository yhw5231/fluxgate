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
	"sync"
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

// DefaultStreamIdleTimeout bounds how long a streamed answer may go without a byte
// from the upstream. A stream is bounded by silence, not by duration: a reasoning
// model may think for minutes before its first token, and a long answer may take
// longer still, while an upstream that has stopped sending is never going to
// resume. Holding a stream to a whole-request deadline instead would cut healthy
// answers off, which the upstream records as a request it was asked to cancel.
const DefaultStreamIdleTimeout = 2 * time.Minute

// ErrStreamSilent reports a streamed attempt given up because the upstream stopped
// sending: it either never started answering, or went quiet for longer than the
// idle bound.
var ErrStreamSilent = errors.New("upstream stream went silent")

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
	// MediaClient and MediaTransportPool dispatch a generation request, which is
	// one whose upstream holds the connection open until it has produced the
	// artifact. They are what a synchronous image generation needs: the ordinary
	// client's response-header timeout is shorter than a generation takes, so a
	// request that legitimately takes a minute would otherwise fail as if the
	// upstream had gone silent. When either is nil the ordinary client is used.
	MediaClient        *http.Client
	MediaTransportPool *TransportPool
	// StreamIdleTimeout bounds how long a streamed answer may be silent before the
	// attempt is given up. A stream is not held to a whole-request deadline,
	// because a long answer would be cut off as if the upstream had failed. When
	// zero, DefaultStreamIdleTimeout is used.
	StreamIdleTimeout time.Duration
	Policy            domain.RetryPolicy
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
	if input.Media && e.MediaClient != nil {
		client = e.MediaClient
	}
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
	streamIdle := e.StreamIdleTimeout
	if streamIdle <= 0 {
		streamIdle = DefaultStreamIdleTimeout
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

		body := transform.NewBody(input.ContentType, input.Body)
		body, err = body.Rewrite(selection.Model, channel.Transform)
		if err != nil {
			return Result{Trace: trace}, err
		}
		call, err := buildRequest(ctx, input, channel, body, streamIdle)
		if err != nil {
			return Result{Trace: trace}, err
		}
		request := call.request

		dispatchClient := client
		if e.ProxyResolver != nil {
			resolved, resolveErr := e.ProxyResolver.Resolve(ProxyRequest{KeyID: channel.APIKey, TargetURL: request.URL})
			if resolveErr != nil {
				call.cancel()
				return Result{Trace: trace}, resolveErr
			}
			pool := e.TransportPool
			if input.Media && e.MediaTransportPool != nil {
				pool = e.MediaTransportPool
			}
			if pool == nil {
				pool = NewTransportPool(nil)
			}
			dispatchClient, err = pool.Client(resolved)
			if err != nil {
				call.cancel()
				return Result{Trace: trace}, err
			}
		}

		response, dispatchErr := dispatchClient.Do(request)
		if dispatchErr != nil && call.silent() {
			// The attempt failed because the upstream never started answering. The
			// transport reports that as a canceled context, which says nothing about
			// who canceled it, so the bound that actually expired is named here.
			dispatchErr = fmt.Errorf("%w: the upstream did not start answering within %s", ErrStreamSilent, call.firstByte)
		}
		call.answer(response, streamIdle)
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
// the request has to stay with while failover is limited, and to the one line a
// request that names no model is pinned to.
func (e *Engine) selectionRequest(input domain.Request, excluded map[string]struct{}, pinned domain.Selection, failover domain.FailoverPolicy) domain.SelectionRequest {
	request := domain.SelectionRequest{
		Model:    input.Model,
		Policy:   input.Policy,
		Excluded: excluded,
	}
	// A pinned request is not a routing question at all: the line that holds the
	// upstream-side work is the only one that can answer, so it is asked for on
	// every attempt and failover never moves the request away from it.
	if pinnedChannel := strings.TrimSpace(input.OnlyChannel); pinnedChannel != "" {
		request.OnlyChannel = pinnedChannel
		return request
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

// upstreamCall is one prepared attempt: the request, the cancel that ends it, and
// the watchdog that bounds a streamed answer by silence rather than by duration.
type upstreamCall struct {
	request *http.Request
	cancel  context.CancelFunc
	// guard is nil for an attempt that is read as one body, which its context
	// deadline bounds instead.
	guard *silenceGuard
	// firstByte is how long the upstream was given to start answering, kept so a
	// failure to do so can say which bound it missed.
	firstByte time.Duration
}

// silent reports whether the attempt was given up because the upstream stopped
// sending, as opposed to being canceled by the caller or by the transport.
func (c upstreamCall) silent() bool {
	return c.guard != nil && c.guard.expired()
}

// answer puts the bounds of the attempt on the response that came back: a stream's
// watchdog is retuned from "must start answering" to "may not go silent", and the
// body is released from the watchdog once it is closed, which also ends the
// upstream request.
func (c upstreamCall) answer(response *http.Response, idle time.Duration) {
	if response == nil {
		if c.guard != nil {
			c.guard.stop()
		}
		c.cancel()
		return
	}
	if c.guard == nil {
		response.Body = &cancelReadCloser{ReadCloser: response.Body, cancel: c.cancel}
		return
	}
	c.guard.retune(idle)
	response.Body = &watchedBody{ReadCloser: response.Body, guard: c.guard, cancel: c.cancel, idle: idle}
}

// buildRequest prepares one upstream attempt and the bounds that apply to it.
//
// A request answered as a single body is bounded by the channel's request timeout
// from dispatch to the last byte. A streamed answer is not: what bounds it is
// silence, not duration. The upstream must still start answering within that
// timeout — a request that never begins has failed — but once it has started it may
// take as long as the answer needs, provided it does not go quiet for longer than
// idle. A whole-request deadline on a stream would cut a long answer off and leave
// the upstream recording a canceled request it had done nothing to deserve.
func buildRequest(ctx context.Context, input domain.Request, channel domain.Channel, body transform.Body, idle time.Duration) (upstreamCall, error) {
	baseURL, err := url.Parse(strings.TrimRight(channel.BaseURL, "/"))
	if err != nil {
		return upstreamCall{}, fmt.Errorf("parse channel base URL: %w", err)
	}
	target := upstreamTarget(baseURL, input.Path)

	// A generation is bounded by the gateway's media timeout, which is longer than
	// the per-channel default because producing an image or a video takes longer
	// than answering a chat completion.
	timeout := channel.RequestTimeout
	if input.Timeout > 0 {
		timeout = input.Timeout
	}
	requestContext := ctx
	var cancel context.CancelFunc = func() {}
	call := upstreamCall{firstByte: timeout}
	if input.Stream {
		// The request outlives its deadline by design, so the context it is sent
		// with carries no deadline of its own; the watchdog is what ends it, either
		// because the upstream never started answering or because it went quiet.
		requestContext, cancel = context.WithCancel(ctx)
		firstByte := timeout
		if firstByte <= 0 {
			firstByte = idle
		}
		call.firstByte = firstByte
		call.guard = newSilenceGuard(cancel, firstByte)
	} else if timeout > 0 {
		requestContext, cancel = context.WithTimeout(ctx, timeout)
	}
	call.cancel = cancel
	request, err := http.NewRequestWithContext(requestContext, input.Method, target.String(), bytes.NewReader(body.Raw))
	if err != nil {
		cancel()
		return upstreamCall{}, fmt.Errorf("build upstream request: %w", err)
	}
	request.Header = transform.ApplyHeaders(input.Headers, channel.Transform, channel.APIKey, body.ContentType)
	request.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body.Raw)), nil
	}
	call.request = request
	return call, nil
}

// silenceGuard ends an attempt whose upstream has gone quiet. It measures the time
// between bytes rather than the time the request takes, so a slow answer is served
// while a stalled one is given up instead of holding the downstream connection
// open until the client gives up on it.
type silenceGuard struct {
	cancel context.CancelFunc

	mu      sync.Mutex
	timeout time.Duration
	timer   *time.Timer
	fired   bool
	stopped bool
}

func newSilenceGuard(cancel context.CancelFunc, timeout time.Duration) *silenceGuard {
	guard := &silenceGuard{cancel: cancel, timeout: timeout}
	if timeout > 0 {
		guard.timer = time.AfterFunc(timeout, guard.fire)
	}
	return guard
}

// fire ends the attempt. It is the timer callback, so it must not hold mu while
// canceling: the cancel wakes the read that is blocked on the request context.
func (g *silenceGuard) fire() {
	g.mu.Lock()
	alreadyStopped := g.stopped
	if !alreadyStopped {
		g.fired = true
	}
	g.mu.Unlock()
	if !alreadyStopped {
		g.cancel()
	}
}

// expired reports whether the guard gave up on the attempt.
func (g *silenceGuard) expired() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.fired
}

// retune replaces the bound the guard measures. It is what moves a stream from
// "the upstream has this long to start answering" to "it may not stay silent for
// longer than this".
func (g *silenceGuard) retune(timeout time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.stopped || g.fired {
		// The attempt has already been given up; re-arming the timer would only
		// cancel a request whose context is already done.
		return
	}
	g.timeout = timeout
	if g.timer != nil {
		g.timer.Stop()
		g.timer = nil
	}
	if timeout > 0 {
		g.timer = time.AfterFunc(timeout, g.fire)
	}
}

// kick restarts the silence window from now, which every byte read from the
// upstream does.
func (g *silenceGuard) kick() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.stopped || g.fired || g.timer == nil {
		return
	}
	g.timer.Reset(g.timeout)
}

// pause stops the window while the bytes just read are on their way to the client:
// a client that reads slowly is not an upstream that has gone quiet.
func (g *silenceGuard) pause() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.timer != nil {
		g.timer.Stop()
	}
}

// stop retires the guard, so a response that has been fully read is not canceled
// after the fact.
func (g *silenceGuard) stop() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.stopped = true
	if g.timer != nil {
		g.timer.Stop()
		g.timer = nil
	}
}

// watchedBody is the body of a streamed answer as the gateway reads it: every byte
// pushes the silence window forward, and the response is released from the guard
// when it is closed.
type watchedBody struct {
	io.ReadCloser
	guard  *silenceGuard
	cancel context.CancelFunc
	idle   time.Duration
}

func (body *watchedBody) Read(buffer []byte) (int, error) {
	body.guard.kick()
	count, err := body.ReadCloser.Read(buffer)
	body.guard.pause()
	if err != nil && err != io.EOF && body.guard.expired() {
		return count, fmt.Errorf("%w: the upstream sent nothing for %s", ErrStreamSilent, body.idle)
	}
	return count, err
}

func (body *watchedBody) Close() error {
	body.guard.stop()
	err := body.ReadCloser.Close()
	body.cancel()
	return err
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
