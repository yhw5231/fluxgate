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
	"time"

	"github.com/yhw5231/fluxgate/internal/domain"
	"github.com/yhw5231/fluxgate/internal/transform"
)

const maxBufferedErrorBody = int64(1 << 20)

// Result contains the final upstream response and the successful attempt.
type Result struct {
	Response *http.Response
	Attempt  domain.Attempt
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
	Sleep         func(context.Context, time.Duration) error
	Now           func() time.Time
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
	policy := normalizePolicy(e.Policy)
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

	for attemptNumber := 1; attemptNumber <= policy.MaxAttempts; attemptNumber++ {
		selection, err := e.Selector.Select(input.Model, excluded)
		if err != nil {
			if lastFailure.Err != nil || lastFailure.StatusCode != 0 {
				return Result{}, finalError(lastFailure)
			}
			return Result{}, err
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
		}

		body, err := transform.ApplyJSON(input.Body, selection.Model, channel.Transform)
		if err != nil {
			return Result{}, err
		}
		request, cancelRequest, err := buildRequest(ctx, input, channel, body)
		if err != nil {
			return Result{}, err
		}

		dispatchClient := client
		if e.ProxyResolver != nil {
			resolved, resolveErr := e.ProxyResolver.Resolve(ProxyRequest{KeyID: channel.APIKey, TargetURL: request.URL})
			if resolveErr != nil {
				cancelRequest()
				return Result{}, resolveErr
			}
			pool := e.TransportPool
			if pool == nil {
				pool = NewTransportPool(nil)
			}
			dispatchClient, err = pool.Client(resolved)
			if err != nil {
				cancelRequest()
				return Result{}, err
			}
		}

		response, dispatchErr := dispatchClient.Do(request)
		if response != nil {
			response.Body = &cancelReadCloser{ReadCloser: response.Body, cancel: cancelRequest}
		} else {
			cancelRequest()
		}
		if dispatchErr == nil && response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices {
			if e.Observer != nil {
				e.Observer.RecordSuccess(attempt)
			}
			return Result{Response: response, Attempt: attempt}, nil
		}

		statusCode := 0
		if response != nil {
			statusCode = response.StatusCode
		}
		retryable := dispatchErr != nil || isRetryableStatus(policy, statusCode)
		lastFailure = domain.Failure{
			Attempt:    attempt,
			StatusCode: statusCode,
			Err:        dispatchErr,
			Retryable:  retryable,
		}
		if e.Observer != nil {
			e.Observer.RecordFailure(lastFailure)
		}

		if !retryable {
			return Result{Response: response, Attempt: attempt}, nil
		}
		if response != nil {
			drainAndClose(response.Body)
		}
		if channelAttempt >= policy.MaxAttemptsPerChannel {
			excluded[channel.ID] = struct{}{}
		}
		if attemptNumber >= policy.MaxAttempts {
			break
		}
		if err := sleep(ctx, backoff(policy, attemptNumber)); err != nil {
			return Result{}, err
		}
	}

	return Result{}, finalError(lastFailure)
}

func buildRequest(ctx context.Context, input domain.Request, channel domain.Channel, body []byte) (*http.Request, context.CancelFunc, error) {
	baseURL, err := url.Parse(strings.TrimRight(channel.BaseURL, "/"))
	if err != nil {
		return nil, nil, fmt.Errorf("parse channel base URL: %w", err)
	}
	pathURL, err := url.Parse("/" + strings.TrimLeft(input.Path, "/"))
	if err != nil {
		return nil, nil, fmt.Errorf("parse upstream path: %w", err)
	}
	target := baseURL.ResolveReference(pathURL)

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
