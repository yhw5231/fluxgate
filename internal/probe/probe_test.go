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
)

var questionNumbers = regexp.MustCompile(`\d+`)

// sumOfRequest reads the arithmetic out of the question a probe sent, so the stub
// upstream can answer it the way a working model would.
func sumOfRequest(t *testing.T, r *http.Request) int {
	t.Helper()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read probe request: %v", err)
	}
	var payload struct {
		Messages []struct {
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode probe request: %v", err)
	}
	if len(payload.Messages) == 0 {
		t.Fatalf("probe sent no message: %s", body)
	}
	matches := questionNumbers.FindAllString(payload.Messages[0].Content, -1)
	if len(matches) != 3 {
		t.Fatalf("question %q does not carry three operands", payload.Messages[0].Content)
	}
	total := 0
	for _, match := range matches {
		number, err := strconv.Atoi(match)
		if err != nil {
			t.Fatalf("operand %q: %v", match, err)
		}
		total += number
	}
	return total
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
		total := sumOfRequest(t, r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(chatAnswer(fmt.Sprintf("The answer is %d.", total))))
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
		matches := questionNumbers.FindAllString(payload.Input[0].Content, -1)
		total := 0
		for _, match := range matches {
			number, _ := strconv.Atoi(match)
			total += number
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"output":[{"type":"message","content":[{"type":"output_text","text":"The answer is %d."}]}]}`, total)
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
		w.Header().Set("Content-Type", "application/json")
		// A stub that answers the question with a number that is not the sum.
		_, _ = w.Write([]byte(chatAnswer("The answer is 1.")))
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
	want := []int{12345, 67, 12412}
	if len(numbers) != len(want) {
		t.Fatalf("numbers = %v, want %v", numbers, want)
	}
	for index := range want {
		if numbers[index] != want[index] {
			t.Fatalf("numbers = %v, want %v", numbers, want)
		}
	}
}
