// Package probe verifies that an upstream credential still works, by using it
// the way a client would: it sends one small chat completion and checks the
// answer.
//
// A key can fail without the gateway noticing. A quota expires, an account is
// suspended, a subscription lapses; the next request to that line discovers it,
// and until then the line is still selected. Probing turns that discovery into
// something an operator can see before their users do: a short arithmetic
// question — two multi-digit operands and one of + - * / — is cheap, any working
// model can answer it, and checking the result tells "the account is reachable"
// apart from "the account is working". The request carries nothing but the model
// and the question, so a parameter the gateway picked is never what an upstream
// rejects.
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
	"encoding/binary"
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
	"github.com/yhw5231/fluxgate/internal/protocol"
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
	// UpstreamAPI is the conversational protocol the line speaks, when the
	// operator has named one. The probe asks its question in that protocol, so a
	// line that only serves the Responses API is not reported as broken for
	// answering a chat-completions path with a 404.
	UpstreamAPI string
	Transform   domain.TransformRules
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
		p.verify(&result, body, asked.Answer())
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
func (p *Prober) verify(result *Result, body []byte, want int64) {
	detail, matched := matchAnswer(body, want)
	if !matched {
		result.Reason = ReasonWrongAnswer
		result.Detail = detail
		return
	}
	result.OK = true
	result.Reason = ReasonOK
}

// matchAnswer reports whether a served reply states the result the question asked
// for, returning what to tell an operator when it does not. A reply with no
// readable number at all is treated as matched: the account answered a real
// request, and inventing a failure out of an answer that carries no number would
// be worse than saying nothing.
func matchAnswer(body []byte, want int64) (string, bool) {
	answer, found := extractAnswer(body)
	if !found {
		return "", true
	}
	numbers := numbersIn(answer)
	if len(numbers) == 0 {
		return "", true
	}
	for _, number := range numbers {
		if number == want {
			return "", true
		}
	}
	return fmt.Sprintf("the answer %q does not contain %d", truncate(answer, 128), want), false
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
	asked := newQuestion()

	// The probe asks with the smallest body a chat completion can carry: the model,
	// the question, and nothing streamed. An output bound and a temperature are
	// left out on purpose. Newer models reject `max_tokens` in favour of
	// `max_completion_tokens`, and some accept no temperature but their own, so a
	// probe that named them would be answered with a 400 about the probe's own
	// request — and a working account would be reported broken over a parameter the
	// gateway chose. The probe's job is to test the account, not the model's
	// parameter support.
	body, err := json.Marshal(map[string]any{
		"model":    line.Model,
		"messages": []map[string]string{{"role": "user", "content": asked.Text()}},
		"stream":   false,
	})
	if err != nil {
		return question{}, nil, nil, fmt.Errorf("encode the probe request: %w", err)
	}
	// The question is written as a chat completion and translated when the line
	// speaks the Responses API, so the probe asks exactly the shape a real request
	// on that line would.
	api := protocol.APIChatCompletions
	if protocol.NormalizeAPI(line.UpstreamAPI) == protocol.APIResponses {
		converted, convertErr := protocol.ConvertRequest(protocol.Conversion{
			From: protocol.APIChatCompletions,
			To:   protocol.APIResponses,
		}, body)
		if convertErr != nil {
			return question{}, nil, nil, fmt.Errorf("encode the probe request: %w", convertErr)
		}
		body = converted
		api = protocol.APIResponses
	}
	// A channel that rewrites its request body needs those rules applied to the
	// probe too. They are what a line's own upstream requires — a parameter it only
	// accepts under another name, say — so a probe that skipped them would be
	// rejected for whatever the rule supplies and report a working line as broken.
	if needsBodyRules(line.Transform) {
		rewritten, rewriteErr := transform.ApplyJSON(body, line.Model, line.Transform)
		if rewriteErr != nil {
			return question{}, nil, nil, fmt.Errorf("apply the line's body rules to the probe: %w", rewriteErr)
		}
		body = rewritten
	}
	target.Path = probeTargetPath(base, api)
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

// probeTargetPath is the path a probe is sent to, joined the way the proxy joins
// an upstream address: a base address that carries a path supplies it, and one
// that carries none keeps the protocol's version segment.
func probeTargetPath(base *url.URL, api string) string {
	path := protocol.PathForAPI(api)
	basePath := strings.TrimRight(base.Path, "/")
	if basePath == "" {
		return path
	}
	return basePath + strings.TrimPrefix(path, "/v1")
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
func numbersIn(text string) []int64 {
	cleaned := strings.NewReplacer(",", "", "，", "").Replace(text)
	matches := numberPattern.FindAllString(cleaned, -1)
	numbers := make([]int64, 0, len(matches))
	for _, match := range matches {
		if number, err := strconv.ParseInt(match, 10, 64); err == nil {
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

// question is one arithmetic question: two multi-digit operands and one of the
// four basic operations. Several digits make a lucky guess implausible, and taking
// the operator from + - * / rather than always adding keeps a reply from being
// accepted for repeating a number the prompt already carried. The answer is still
// trivial for any working model — a health probe is meant to test the account, not
// the model's arithmetic stamina — so multiplication keeps two-digit operands and
// division keeps a two-digit divisor with a two-digit quotient, where a working
// model computes the exact result, while addition and subtraction use five-digit
// ones.
type question struct {
	a, b int64
	op   rune
}

// newQuestion draws one question, arranged so the expected answer is a whole number
// that appears nowhere in the question: subtraction keeps the larger operand first
// and division is exact, so the probe can tell a computed answer from an echo of the
// prompt.
func newQuestion() question {
	switch randomBelow(4) {
	case 0:
		return question{a: randomBetween(10000, 99999), b: randomBetween(10000, 99999), op: '+'}
	case 1:
		a, b := randomBetween(10000, 99999), randomBetween(10000, 99999)
		if a < b {
			a, b = b, a
		}
		if a == 2*b {
			// The result would then be one of the operands, which an echo of the
			// question would contain without computing anything.
			b++
		}
		return question{a: a, b: b, op: '-'}
	case 2:
		return question{a: randomBetween(11, 99), b: randomBetween(11, 99), op: '*'}
	default:
		divisor := randomBetween(11, 99)
		quotient := randomBetween(11, 99)
		if quotient == divisor {
			// A result equal to the divisor would be carried by the question itself,
			// so an echo could pass for an answer: step the quotient aside, staying at
			// two digits.
			if divisor < 99 {
				quotient = divisor + 1
			} else {
				quotient = divisor - 1
			}
		}
		return question{a: divisor * quotient, b: divisor, op: '/'}
	}
}

// Text renders the question as the prompt a client would send.
func (q question) Text() string {
	return fmt.Sprintf("What is %d %c %d? Reply with the number only.", q.a, q.op, q.b)
}

// Answer is the result the probe expects.
func (q question) Answer() int64 {
	switch q.op {
	case '+':
		return q.a + q.b
	case '-':
		return q.a - q.b
	case '*':
		return q.a * q.b
	case '/':
		if q.b == 0 {
			return 0
		}
		return q.a / q.b
	}
	return 0
}

// randomBelow returns a random number in [0, limit).
func randomBelow(limit int64) int64 {
	if limit <= 0 {
		return 0
	}
	var buffer [8]byte
	if _, err := rand.Read(buffer[:]); err != nil {
		return time.Now().UnixNano() % limit
	}
	return int64(binary.BigEndian.Uint64(buffer[:]) % uint64(limit))
}

// randomBetween returns a random number in [low, high].
func randomBetween(low, high int64) int64 {
	if high <= low {
		return low
	}
	return low + randomBelow(high-low+1)
}

// needsBodyRules reports whether a line carries JSON body mutations worth
// applying to the probe request.
func needsBodyRules(rules domain.TransformRules) bool {
	return len(rules.DeleteJSON) > 0 || len(rules.OverrideJSON) > 0
}
