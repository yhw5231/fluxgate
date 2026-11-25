// Package probe verifies that an upstream credential still works, by using it
// the way a client would: it sends one small chat completion and checks the
// answer.
//
// A key can fail without the gateway noticing. A quota expires, an account is
// suspended, a subscription lapses; the next request to that line discovers it,
// and until then the line is still selected. Probing turns that discovery into
// something an operator can see before their users do: a short arithmetic
// question is cheap, it needs no model to be healthy beyond answering at all,
// and a wrong answer tells "the account is reachable" apart from "the account is
// working".
//
// The probe is deliberately observational. It records what it found and never
// changes the circuits: a line that is cooling down is cooled because a real
// request failed on it, and bringing it back is an operator's decision (恢复) or
// its cooldown expiring — not something a probe should do on their behalf.
package probe

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/yhw5231/fluxgate/internal/clientprofile"
	"github.com/yhw5231/fluxgate/internal/domain"
	"github.com/yhw5231/fluxgate/internal/proxy"
	"github.com/yhw5231/fluxgate/internal/transform"
)

// Kind names why a probe ran, which is what makes its record readable after the
// fact: an idle probe that failed means something different from a startup sweep
// that failed.
type Kind string

const (
	// KindStartup is the sweep a gateway runs once after it starts, because a
	// restart is exactly when nobody knows which credentials still work.
	KindStartup Kind = "startup"
	// KindIdle is the periodic check of a line that has not carried traffic for
	// longer than the configured idle interval.
	KindIdle Kind = "idle"
	// KindRecovery is the check of a line that was held out of rotation and no
	// longer is, which answers whether the account really came back.
	KindRecovery Kind = "recovery"
	// KindManual is a check an operator asked for.
	KindManual Kind = "manual"
)

// Reason classifies the outcome of one probe. The console renders the reason in
// its own language; Detail carries the underlying error for diagnosis.
const (
	ReasonOK            = "ok"
	ReasonUnreachable   = "unreachable"
	ReasonInvalidProxy  = "invalid_proxy"
	ReasonAuthRejected  = "auth_rejected"
	ReasonRateLimited   = "rate_limited"
	ReasonUpstreamError = "upstream_error"
	ReasonBadStatus     = "bad_status"
	ReasonWrongAnswer   = "wrong_answer"
)

// Line is one upstream line as the probe needs it: where to send the request,
// which credential to present, and which model to ask about.
type Line struct {
	ChannelID     string
	ChannelName   string
	BaseURL       string
	APIKey        string
	Model         string
	BreakerMode   string
	ClientProfile string
	Transform     domain.TransformRules
	// ProxyURL and UseSystemProxy are the line's own outbound decision. They are
	// resolved through the gateway's own resolver, so a probe leaves the way a
	// real request from that line would.
	ProxyURL       string
	UseSystemProxy bool
}

// Result is one probe as the log keeps it and the console reads it.
type Result struct {
	// At is when the probe started.
	At time.Time `json:"at"`
	// Kind is why it ran.
	Kind Kind `json:"kind"`
	// ChannelID and ChannelName identify the line.
	ChannelID   string `json:"channel_id"`
	ChannelName string `json:"channel_name"`
	// Model is the model the question was asked about.
	Model string `json:"model"`
	// Status is the HTTP status the upstream answered with, or zero when it did
	// not answer.
	Status int `json:"status"`
	// LatencyMS is how long the probe took end to end.
	LatencyMS int64 `json:"latency_ms"`
	// OK reports whether the account answered correctly.
	OK bool `json:"ok"`
	// Reason is the stable classification, and Detail the text that explains it.
	Reason string `json:"reason"`
	Detail string `json:"detail,omitempty"`
}

// Prober sends one probe request. It holds the transport and the proxy resolver
// the gateway itself uses, so a probe reaches an upstream through the same path a
// client request would.
type Prober struct {
	// Client is used when no proxy resolution is needed, which is also what a
	// test injects.
	Client *http.Client
	// Resolver and Pool decide the outbound route. When either is nil the probe
	// leaves directly.
	Resolver *proxy.Resolver
	Pool     *proxy.TransportPool
	// Timeout bounds one probe request. A probe is a small question: a line that
	// cannot answer it promptly is worth reporting as slow rather than waited on
	// for minutes.
	Timeout time.Duration
	Now     func() time.Time
	// MaxBodyBytes bounds how much of the answer is read.
	MaxBodyBytes int64
}

const (
	defaultProbeTimeout = 45 * time.Second
	defaultMaxBodyBytes = 64 << 10
)

// maxReadBodyBytes is the hard bound on an answer the probe keeps.
func maxReadBodyBytes(configured int64) int64 {
	if configured <= 0 {
		return defaultMaxBodyBytes
	}
	return configured
}

// Probe asks one line one arithmetic question and classifies the answer.
func (p *Prober) Probe(ctx context.Context, line Line, kind Kind) Result {
	now := p.Now
	if now == nil {
		now = time.Now
	}
	result := Result{
		At:          now().UTC(),
		Kind:        kind,
		ChannelID:   line.ChannelID,
		ChannelName: line.ChannelName,
		Model:       line.Model,
	}
	started := now()

	asked, target, request, err := p.question(line)
	if err != nil {
		result.Reason = ReasonBadStatus
		result.Detail = err.Error()
		return result
	}

	timeout := p.Timeout
	if timeout <= 0 {
		timeout = defaultProbeTimeout
	}
	// The caller's cancellation still applies, but a probe is bounded on its own
	// too: an upstream that trickles bytes would otherwise hold the slot open.
	requestContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	request = request.WithContext(requestContext)

	client, err := p.client(line, target)
	if err != nil {
		result.Reason = ReasonInvalidProxy
		result.Detail = err.Error()
		return result
	}

	response, err := client.Do(request)
	result.LatencyMS = now().Sub(started).Milliseconds()
	if err != nil {
		result.Reason = ReasonUnreachable
		result.Detail = err.Error()
		return result
	}
	defer response.Body.Close()
	result.Status = response.StatusCode
	body, _ := io.ReadAll(io.LimitReader(response.Body, maxReadBodyBytes(p.MaxBodyBytes)))

	switch {
	case response.StatusCode >= 200 && response.StatusCode < 300:
		p.verify(&result, body, asked.Sum())
		return result
	case response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden:
		result.Reason = ReasonAuthRejected
		result.Detail = fmt.Sprintf("the upstream rejected the credential (HTTP %d)", response.StatusCode)
	case response.StatusCode == http.StatusTooManyRequests:
		result.Reason = ReasonRateLimited
		result.Detail = "the upstream rate limited the probe"
		if retry := strings.TrimSpace(response.Header.Get("Retry-After")); retry != "" {
			result.Detail += " (Retry-After " + retry + ")"
		}
	case response.StatusCode >= 500:
		result.Reason = ReasonUpstreamError
		result.Detail = fmt.Sprintf("the upstream answered HTTP %d: %s", response.StatusCode, snippet(body))
	default:
		result.Reason = ReasonBadStatus
		result.Detail = fmt.Sprintf("the upstream answered HTTP %d: %s", response.StatusCode, snippet(body))
	}
	return result
}

// verify checks the answer of a served probe. A reply that carries no readable
// answer is left alone — the account answered a real request, which is what the
// probe is for — while a reply that states a number and states the wrong one is
// reported, because that is an account that is reachable but not working.
func (p *Prober) verify(result *Result, body []byte, want int) {
	answer, found := extractAnswer(body)
	if !found {
		result.OK = true
		result.Reason = ReasonOK
		return
	}
	numbers := numbersIn(answer)
	if len(numbers) == 0 {
		result.OK = true
		result.Reason = ReasonOK
		return
	}
	for _, number := range numbers {
		if number == want {
			result.OK = true
			result.Reason = ReasonOK
			return
		}
	}
	result.Reason = ReasonWrongAnswer
	result.Detail = fmt.Sprintf("the answer %q does not contain %d", truncate(answer, 128), want)
}

// question builds the probe request for one line. It returns the question that
// was asked together with the request, because the answer is checked against the
// arithmetic the request carried.
func (p *Prober) question(line Line) (question, *url.URL, *http.Request, error) {
	if strings.TrimSpace(line.APIKey) == "" {
		return question{}, nil, nil, errors.New("the line carries no credential")
	}
	if strings.TrimSpace(line.Model) == "" {
		return question{}, nil, nil, errors.New("the line names no model to ask about")
	}
	base, err := url.Parse(strings.TrimRight(strings.TrimSpace(line.BaseURL), "/"))
	if err != nil {
		return question{}, nil, nil, fmt.Errorf("parse the upstream address: %w", err)
	}
	target := *base
	target.Path = strings.TrimRight(base.Path, "/") + "/chat/completions"
	asked := newQuestion()

	body, err := json.Marshal(map[string]any{
		"model":       line.Model,
		"messages":    []map[string]string{{"role": "user", "content": asked.Text()}},
		"stream":      false,
		"max_tokens":  32,
		"temperature": 0,
	})
	if err != nil {
		return question{}, nil, nil, fmt.Errorf("encode the probe request: %w", err)
	}
	request, err := http.NewRequest(http.MethodPost, target.String(), bytes.NewReader(body))
	if err != nil {
		return question{}, nil, nil, fmt.Errorf("build the probe request: %w", err)
	}
	// The probe presents exactly what a dispatched request presents: the client
	// fingerprint, the upstream's own configured headers, and its credential.
	request.Header = transform.ApplyHeadersWithProfile(
		nil,
		clientprofile.Headers(line.ClientProfile),
		line.Transform,
		line.APIKey,
		"application/json",
	)
	return asked, &target, request, nil
}

// client resolves the outbound route for one probe.
func (p *Prober) client(line Line, target *url.URL) (*http.Client, error) {
	if p.Client != nil {
		return p.Client, nil
	}
	resolver := p.Resolver
	if resolver == nil {
		// A prober assembled without the gateway's resolver still honors the line's
		// own outbound decision, so a probe through a proxy works on its own.
		resolver = &proxy.Resolver{Config: proxy.ProxyConfig{
			Keys: map[string]string{line.APIKey: outboundValue(line)},
		}}
	}
	resolved, err := resolver.Resolve(proxy.ProxyRequest{
		KeyID:     strings.TrimSpace(line.APIKey),
		TargetURL: target,
	})
	if err != nil {
		return nil, err
	}
	pool := p.Pool
	if pool == nil {
		pool = proxy.NewTransportPool(nil)
	}
	return pool.Client(resolved)
}

// outboundValue renders the line's own proxy decision in the form the resolver
// reads: an address, "system", or "direct" when it leaves as the gateway does.
func outboundValue(line Line) string {
	if address := strings.TrimSpace(line.ProxyURL); address != "" {
		return address
	}
	if line.UseSystemProxy {
		return "system"
	}
	return "direct"
}

var numberPattern = regexp.MustCompile(`-?\d+`)

// extractAnswer reads the assistant's reply out of an upstream body: the chat
// shape first, then the Responses API's shape, and finally the whole body when it
// is not JSON at all — some providers answer a plain question with plain text.
func extractAnswer(body []byte) (string, bool) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return "", false
	}
	var raw json.RawMessage
	if json.Unmarshal(trimmed, &raw) != nil {
		return string(trimmed), true
	}
	if len(raw) > 0 && raw[0] == '"' {
		var scalar string
		if json.Unmarshal(raw, &scalar) == nil && strings.TrimSpace(scalar) != "" {
			return scalar, true
		}
	}
	if answer, ok := answerInJSON(raw); ok {
		return answer, true
	}
	// A gateway may wrap the OpenAI shape in a data envelope.
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(raw, &envelope) == nil && len(envelope.Data) > 0 {
		if answer, ok := answerInJSON(envelope.Data); ok {
			return answer, true
		}
	}
	return "", false
}

// answerInJSON reads the reply text from the chat or Responses shape.
func answerInJSON(raw []byte) (string, bool) {
	var chat struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &chat); err == nil && len(chat.Choices) > 0 {
		if content := strings.TrimSpace(chat.Choices[0].Message.Content); content != "" {
			return content, true
		}
	}
	var responses struct {
		OutputText string `json:"output_text"`
		Output     []struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	if err := json.Unmarshal(raw, &responses); err == nil {
		if text := strings.TrimSpace(responses.OutputText); text != "" {
			return text, true
		}
		for _, output := range responses.Output {
			for _, content := range output.Content {
				if text := strings.TrimSpace(content.Text); text != "" {
					return text, true
				}
			}
		}
	}
	return "", false
}

// numbersIn lists every whole number in a reply, tolerating thousands separators
// and the prose a model puts around an answer. A number too long to parse is
// ignored rather than guessed at.
func numbersIn(text string) []int {
	cleaned := strings.NewReplacer(",", "", "，", "").Replace(text)
	matches := numberPattern.FindAllString(cleaned, -1)
	numbers := make([]int, 0, len(matches))
	for _, match := range matches {
		if number, err := strconv.Atoi(match); err == nil {
			numbers = append(numbers, number)
		}
	}
	return numbers
}

// snippet renders the beginning of an upstream body as one line a record can
// hold.
func snippet(body []byte) string {
	text := strings.TrimSpace(string(body))
	text = strings.Join(strings.Fields(text), " ")
	return truncate(text, 256)
}

func truncate(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return text[:limit] + "…"
}

// question is one arithmetic question. Three five-digit operands keep the sum
// inside an integer and make a lucky guess implausible, while the arithmetic is
// still trivial for any working model.
type question struct {
	a, b, c int
}

func newQuestion() question {
	return question{a: randomOperand(), b: randomOperand(), c: randomOperand()}
}

// Text renders the question as the prompt a client would send.
func (q question) Text() string {
	return fmt.Sprintf("What is %d + %d + %d? Reply with the number only.", q.a, q.b, q.c)
}

// Sum is the answer the probe expects.
func (q question) Sum() int { return q.a + q.b + q.c }

func randomOperand() int {
	var buffer [4]byte
	if _, err := rand.Read(buffer[:]); err != nil {
		return 10000 + int(time.Now().UnixNano()%90000)
	}
	value := int(buffer[0])<<24 | int(buffer[1])<<16 | int(buffer[2])<<8 | int(buffer[3])
	if value < 0 {
		value = -value
	}
	return 10000 + value%90000
}
