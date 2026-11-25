package probe

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/yhw5231/fluxgate/internal/domain"
)

var questionPattern = regexp.MustCompile(`What is (-?\d+) ([+\-*/]) (-?\d+)\?`)

// questionAnswer evaluates the arithmetic in a probe prompt, so the stub upstream
// can answer it the way a working model would.
func questionAnswer(prompt string) (int64, bool) {
	match := questionPattern.FindStringSubmatch(prompt)
	if match == nil {
		return 0, false
	}
	a, errA := strconv.ParseInt(match[1], 10, 64)
	b, errB := strconv.ParseInt(match[3], 10, 64)
	if errA != nil || errB != nil {
		return 0, false
	}
	switch match[2] {
	case "+":
		return a + b, true
	case "-":
		return a - b, true
	case "*":
		return a * b, true
	case "/":
		if b == 0 {
			return 0, false
		}
		return a / b, true
	}
	return 0, false
}

// promptOfBody reads the question out of a probe request, whichever protocol shape
// it was written in.
func promptOfBody(raw []byte) string {
	var payload struct {
		Messages []struct {
			Content string `json:"content"`
		} `json:"messages"`
		Input []struct {
			Content string `json:"content"`
		} `json:"input"`
	}
	if json.Unmarshal(raw, &payload) != nil {
		return ""
	}
	if len(payload.Messages) > 0 {
		return payload.Messages[0].Content
	}
	if len(payload.Input) > 0 {
		return payload.Input[0].Content
	}
	return ""
}

// chatAnswer renders an OpenAI chat completion whose content is the given text.
func chatAnswer(content string) string {
	encoded, _ := json.Marshal(map[string]any{
		"choices": []map[string]any{{"message": map[string]string{"role": "assistant", "content": content}}},
	})
	return string(encoded)
}

// workingUpstream answers every probe correctly and counts what it saw.
func workingUpstream(t *testing.T, seen *[]http.Header) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if seen != nil {
			mu.Lock()
			*seen = append(*seen, r.Header.Clone())
			mu.Unlock()
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read probe request: %v", err)
		}
		answer, ok := questionAnswer(promptOfBody(body))
		if !ok {
			t.Errorf("probe sent no arithmetic question: %s", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(chatAnswer(fmt.Sprintf("The answer is %d.", answer))))
	}))
}

func lineFor(server *httptest.Server) Line {
	return Line{
		ChannelID:   "line-1",
		ChannelName: "relay",
		BaseURL:     server.URL + "/v1",
		APIKey:      "sk-upstream",
		Model:       "gpt-4.1",
	}
}

func TestProbeAcceptsACorrectAnswer(t *testing.T) {
	server := workingUpstream(t, nil)
	defer server.Close()

	result := (&Prober{Client: server.Client()}).Probe(context.Background(), lineFor(server), KindManual)
	if !result.OK || result.Reason != ReasonOK {
		t.Fatalf("result = %#v, want a healthy line", result)
	}
	if result.Status != http.StatusOK {
		t.Fatalf("status = %d, want 200", result.Status)
	}
	if result.ChannelID != "line-1" || result.ChannelName != "relay" || result.Model != "gpt-4.1" {
		t.Fatalf("result identifies the wrong line: %#v", result)
	}
	if result.At.IsZero() || result.LatencyMS < 0 {
		t.Fatalf("result has no time or latency: %#v", result)
	}
}

// A line that only serves the Responses API is asked in that protocol, so a
// working account is not reported as broken for answering the chat-completions
// path with a 404.
func TestProbeAsksInTheProtocolTheLineSpeaks(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Errorf("probe path = %q, want /v1/responses", r.URL.Path)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read probe request: %v", err)
		}
		var payload struct {
			Input []struct {
				Content string `json:"content"`
			} `json:"input"`
			Messages []any `json:"messages"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatalf("decode probe request: %v", err)
		}
		if len(payload.Messages) > 0 {
			t.Fatalf("probe body still carries chat messages: %s", body)
		}
		if len(payload.Input) == 0 {
			t.Fatalf("probe body carries no input: %s", body)
		}
		answer, ok := questionAnswer(payload.Input[0].Content)
		if !ok {
			t.Fatalf("probe sent no arithmetic question: %s", body)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"output":[{"type":"message","content":[{"type":"output_text","text":"The answer is %d."}]}]}`, answer)
	}))
	defer server.Close()

	line := lineFor(server)
	line.UpstreamAPI = "responses"
	result := (&Prober{Client: server.Client()}).Probe(context.Background(), line, KindManual)
	if !result.OK || result.Reason != ReasonOK {
		t.Fatalf("result = %#v, want a healthy line asked in its own protocol", result)
	}
}

func TestProbeReportsAWrongAnswer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		answer, _ := questionAnswer(promptOfBody(body))
		w.Header().Set("Content-Type", "application/json")
		// A stub that answers the question with a number that is not the result.
		_, _ = w.Write([]byte(chatAnswer(fmt.Sprintf("The answer is %d.", answer+1))))
	}))
	defer server.Close()

	result := (&Prober{Client: server.Client()}).Probe(context.Background(), lineFor(server), KindManual)
	if result.OK {
		t.Fatalf("result = %#v, want a wrong answer reported", result)
	}
	if result.Reason != ReasonWrongAnswer {
		t.Fatalf("reason = %q, want %q", result.Reason, ReasonWrongAnswer)
	}
}

// The question is multi-digit arithmetic in all four operations, so a reply that
// merely echoes a number out of the prompt is not accepted as an answer and a
// model that cannot compute is told apart from an account that cannot answer.
func TestQuestionsAreCheckableArithmetic(t *testing.T) {
	drawn := make(map[rune]bool)
	for draw := 0; draw < 500; draw++ {
		asked := newQuestion()
		drawn[asked.op] = true

		text := asked.Text()
		if !questionPattern.MatchString(text) {
			t.Fatalf("question %q is not a two-operand arithmetic question", text)
		}
		if digits(asked.a) < 2 || digits(asked.b) < 2 {
			t.Fatalf("question %q carries a single-digit operand", text)
		}
		if asked.op == '/' && (asked.b == 0 || asked.a%asked.b != 0) {
			t.Fatalf("division %q is not exact", text)
		}
		answer := asked.Answer()
		if answer == asked.a || answer == asked.b {
			t.Fatalf("question %q answers with one of its own operands", text)
		}
		if parsed, ok := questionAnswer(text); !ok || parsed != answer {
			t.Fatalf("question %q evaluates to %d, %v; want %d", text, parsed, ok, answer)
		}
	}
	for _, operator := range []rune{'+', '-', '*', '/'} {
		if !drawn[operator] {
			t.Errorf("500 questions never drew %q", string(operator))
		}
	}
}

// digits counts the decimal digits of a positive number.
func digits(value int64) int {
	count := 1
	for value >= 10 {
		value /= 10
		count++
	}
	return count
}

// The probe asks with the smallest body a chat completion can carry. Naming an
// output bound or a temperature is what makes a newer model reject the probe — a
// 400 about the probe's own request — and a working account would then be reported
// broken over a parameter the gateway chose.
func TestProbeSendsTheSmallestRequestBody(t *testing.T) {
	var raw []byte
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		raw = body
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(chatAnswer("done")))
	}))
	defer server.Close()

	(&Prober{Client: server.Client()}).Probe(context.Background(), lineFor(server), KindManual)

	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("decode probe request: %v", err)
	}
	for _, name := range []string{"max_tokens", "max_completion_tokens", "max_output_tokens", "temperature", "top_p"} {
		if _, present := payload[name]; present {
			t.Errorf("probe body carries %q; a model that spells or defaults it differently would answer 400", name)
		}
	}
	if payload["model"] != "gpt-4.1" {
		t.Errorf("model = %v, want the line's model", payload["model"])
	}
	if stream, present := payload["stream"]; !present || stream != false {
		t.Errorf("stream = %v, present = %v; want an explicit false", stream, present)
	}
}

// A channel that rewrites its request body needs those rules applied to the probe
// too: a probe that skipped them would be rejected for whatever the rule supplies,
// and a working account would be reported broken.
func TestProbeCarriesTheLinesBodyRules(t *testing.T) {
	var raw []byte
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		raw = body
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(chatAnswer("done")))
	}))
	defer server.Close()

	line := lineFor(server)
	line.Transform = domain.TransformRules{
		OverrideJSON: map[string]any{"service_tier": "flex"},
	}
	result := (&Prober{Client: server.Client()}).Probe(context.Background(), line, KindManual)
	if !result.OK {
		t.Fatalf("result = %#v, want the served probe to be healthy", result)
	}

	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("decode probe request: %v", err)
	}
	if payload["service_tier"] != "flex" {
		t.Fatalf("probe body = %s, want the line's own override applied", raw)
	}
}

func TestProbeClassifiesUpstreamFailures(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		retryAfter string
		reason     string
	}{
		{"credential rejected", http.StatusUnauthorized, "", ReasonAuthRejected},
		{"forbidden", http.StatusForbidden, "", ReasonAuthRejected},
		{"rate limited", http.StatusTooManyRequests, "120", ReasonRateLimited},
		{"upstream broken", http.StatusBadGateway, "", ReasonUpstreamError},
		{"bad request", http.StatusNotFound, "", ReasonBadStatus},
	}
	for _, testcase := range cases {
		t.Run(testcase.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if testcase.retryAfter != "" {
					w.Header().Set("Retry-After", testcase.retryAfter)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(testcase.status)
				_, _ = w.Write([]byte(`{"error":{"message":"nope"}}`))
			}))
			defer server.Close()

			result := (&Prober{Client: server.Client()}).Probe(context.Background(), lineFor(server), KindManual)
			if result.OK || result.Reason != testcase.reason {
				t.Fatalf("result = %#v, want reason %q", result, testcase.reason)
			}
			if result.Status != testcase.status {
				t.Fatalf("status = %d, want %d", result.Status, testcase.status)
			}
			if testcase.retryAfter != "" && !strings.Contains(result.Detail, testcase.retryAfter) {
				t.Fatalf("detail = %q, want the Retry-After value in it", result.Detail)
			}
		})
	}
}

func TestProbeReportsAnUnreachableUpstream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	client := server.Client()
	endpoint := lineFor(server)
	server.Close()

	result := (&Prober{Client: client}).Probe(context.Background(), endpoint, KindManual)
	if result.OK || result.Reason != ReasonUnreachable {
		t.Fatalf("result = %#v, want an unreachable upstream", result)
	}
	if result.Status != 0 {
		t.Fatalf("status = %d, want zero when the upstream never answered", result.Status)
	}
}

func TestProbePresentsTheFingerprintAndCredential(t *testing.T) {
	var seen []http.Header
	server := workingUpstream(t, &seen)
	defer server.Close()

	line := lineFor(server)
	line.ClientProfile = "claude-code"
	(&Prober{Client: server.Client()}).Probe(context.Background(), line, KindManual)

	if len(seen) != 1 {
		t.Fatalf("upstream saw %d requests, want 1", len(seen))
	}
	headers := seen[0]
	if got := headers.Get("Authorization"); got != "Bearer sk-upstream" {
		t.Fatalf("Authorization = %q, want the line credential", got)
	}
	if got := headers.Get("User-Agent"); !strings.HasPrefix(got, "claude-cli/") {
		t.Fatalf("User-Agent = %q, want the selected client fingerprint", got)
	}
	if headers.Get("X-Claude-Code-Session-Id") == "" {
		t.Fatal("the fingerprint's dynamic header was not sent")
	}
}

func TestProbeLeavesAnUnverifiableAnswerAlone(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// A served request whose reply carries no text at all (a tool call, say).
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","tool_calls":[]}}]}`))
	}))
	defer server.Close()

	result := (&Prober{Client: server.Client()}).Probe(context.Background(), lineFor(server), KindManual)
	if !result.OK || result.Reason != ReasonOK {
		t.Fatalf("result = %#v, want a served request treated as healthy", result)
	}
}

func TestProbeRequiresACredentialAndAModel(t *testing.T) {
	server := workingUpstream(t, nil)
	defer server.Close()

	withoutKey := lineFor(server)
	withoutKey.APIKey = ""
	result := (&Prober{Client: server.Client()}).Probe(context.Background(), withoutKey, KindManual)
	if result.OK || result.Reason == "" {
		t.Fatalf("result = %#v, want a refusal", result)
	}

	withoutModel := lineFor(server)
	withoutModel.Model = ""
	result = (&Prober{Client: server.Client()}).Probe(context.Background(), withoutModel, KindManual)
	if result.OK || result.Reason == "" {
		t.Fatalf("result = %#v, want a refusal", result)
	}
}

func TestExtractAnswerReadsTheShapesUpstreamsUse(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
		ok   bool
	}{
		{"chat", `{"choices":[{"message":{"content":"hello"}}]}`, "hello", true},
		{"responses", `{"output_text":"hello"}`, "hello", true},
		{"responses output", `{"output":[{"content":[{"text":"hello"}]}]}`, "hello", true},
		{"data envelope", `{"data":{"choices":[{"message":{"content":"hello"}}]}}`, "hello", true},
		{"plain text", `hello`, "hello", true},
		{"string scalar", `"hello"`, "hello", true},
		{"no answer", `{"choices":[]}`, "", false},
		{"empty", ``, "", false},
	}
	for _, testcase := range cases {
		got, ok := extractAnswer([]byte(testcase.body))
		if got != testcase.want || ok != testcase.ok {
			t.Fatalf("%s: extractAnswer = %q, %v; want %q, %v", testcase.name, got, ok, testcase.want, testcase.ok)
		}
	}
}

func TestNumbersInToleratesProseAndSeparators(t *testing.T) {
	numbers := numbersIn("Sure — 12,345 + 67 = 12,412.")
	want := []int64{12345, 67, 12412}
	if len(numbers) != len(want) {
		t.Fatalf("numbers = %v, want %v", numbers, want)
	}
	for index := range want {
		if numbers[index] != want[index] {
			t.Fatalf("numbers = %v, want %v", numbers, want)
		}
	}
}
