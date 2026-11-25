package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yhw5231/fluxgate/internal/domain"
	"github.com/yhw5231/fluxgate/internal/protocol"
	"github.com/yhw5231/fluxgate/internal/router"
)

// conversionEngine assembles an engine that dispatches one request to one line.
func conversionEngine(t *testing.T, channel domain.Channel) *Engine {
	t.Helper()
	return &Engine{
		Selector: router.NewMemorySelector(routeFor(nil, channel)),
		Client:   &http.Client{},
		Sleep:    noSleep,
	}
}

// A caller that speaks chat completions reaches a line that only serves the
// Responses API: the body and the path are translated on the way out, and the
// answer is translated back.
func TestEngineTranslatesChatRequestForAResponsesLine(t *testing.T) {
	var receivedPath string
	var receivedBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&receivedBody); err != nil {
			t.Errorf("decode upstream request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": "resp_1", "created_at": 1700000000, "model": "gpt-4.1", "status": "completed",
			"output": [{"type": "message", "content": [{"type": "output_text", "text": "translated"}]}],
			"usage": {"input_tokens": 3, "output_tokens": 2, "total_tokens": 5}
		}`))
	}))
	defer upstream.Close()

	engine := conversionEngine(t, domain.Channel{
		ID: "line", BaseURL: upstream.URL, APIKey: "key", Enabled: true,
		SiteID: 1, Weight: 1, UpstreamAPI: protocol.APIResponses,
	})
	result, err := engine.Forward(context.Background(), domain.Request{
		Method:      http.MethodPost,
		Path:        protocol.PathChatCompletions,
		Headers:     http.Header{"Content-Type": []string{"application/json"}},
		Body:        []byte(`{"model":"gpt-4.1","messages":[{"role":"user","content":"hi"}]}`),
		ContentType: "application/json",
		Model:       "gpt-4.1",
	})
	if err != nil {
		t.Fatalf("Forward() error = %v", err)
	}
	defer result.Response.Body.Close()

	if receivedPath != protocol.PathResponses {
		t.Errorf("upstream path = %q, want %q", receivedPath, protocol.PathResponses)
	}
	if _, present := receivedBody["messages"]; present {
		t.Errorf("upstream body still carries messages: %v", receivedBody)
	}
	input, _ := receivedBody["input"].([]any)
	if len(input) != 1 {
		t.Errorf("upstream input = %#v, want one item", receivedBody["input"])
	}
	if receivedBody["model"] != "gpt-4.1" {
		t.Errorf("upstream model = %v, want the mapped model", receivedBody["model"])
	}

	answer, err := io.ReadAll(result.Response.Body)
	if err != nil {
		t.Fatalf("read answer: %v", err)
	}
	var chat map[string]any
	if err := json.Unmarshal(answer, &chat); err != nil {
		t.Fatalf("decode answer: %v", err)
	}
	if chat["object"] != "chat.completion" {
		t.Errorf("answer object = %v, want chat.completion", chat["object"])
	}
	choices, _ := chat["choices"].([]any)
	if len(choices) != 1 {
		t.Fatalf("choices = %#v", chat["choices"])
	}
	choice, _ := choices[0].(map[string]any)
	message, _ := choice["message"].(map[string]any)
	if message["content"] != "translated" {
		t.Errorf("message content = %v, want translated", message["content"])
	}
	if got := result.Response.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
}

// The reverse: a Responses caller reaches a chat-completions line.
func TestEngineTranslatesResponsesRequestForAChatLine(t *testing.T) {
	var receivedPath string
	var receivedBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&receivedBody); err != nil {
			t.Errorf("decode upstream request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": "chatcmpl-1", "created": 1700000000, "model": "gpt-4.1",
			"choices": [{"index": 0, "message": {"role": "assistant", "content": "translated back"}, "finish_reason": "stop"}],
			"usage": {"prompt_tokens": 3, "completion_tokens": 2, "total_tokens": 5}
		}`))
	}))
	defer upstream.Close()

	engine := conversionEngine(t, domain.Channel{
		ID: "line", BaseURL: upstream.URL, APIKey: "key", Enabled: true,
		SiteID: 1, Weight: 1, UpstreamAPI: protocol.APIChatCompletions,
	})
	result, err := engine.Forward(context.Background(), domain.Request{
		Method:      http.MethodPost,
		Path:        protocol.PathResponses,
		Headers:     http.Header{"Content-Type": []string{"application/json"}},
		Body:        []byte(`{"model":"gpt-4.1","input":"hi"}`),
		ContentType: "application/json",
		Model:       "gpt-4.1",
	})
	if err != nil {
		t.Fatalf("Forward() error = %v", err)
	}
	defer result.Response.Body.Close()

	if receivedPath != protocol.PathChatCompletions {
		t.Errorf("upstream path = %q, want %q", receivedPath, protocol.PathChatCompletions)
	}
	if _, present := receivedBody["input"]; present {
		t.Errorf("upstream body still carries input: %v", receivedBody)
	}
	messages, _ := receivedBody["messages"].([]any)
	if len(messages) != 1 {
		t.Errorf("upstream messages = %#v, want one message", receivedBody["messages"])
	}

	answer, err := io.ReadAll(result.Response.Body)
	if err != nil {
		t.Fatalf("read answer: %v", err)
	}
	var responses map[string]any
	if err := json.Unmarshal(answer, &responses); err != nil {
		t.Fatalf("decode answer: %v", err)
	}
	if responses["object"] != "response" || responses["output_text"] != "translated back" {
		t.Errorf("answer = %v, want a Responses object with the text", responses)
	}
}

// A streamed answer is translated event by event, so the caller receives its
// protocol's chunks rather than the upstream's events.
func TestEngineTranslatesAStreamedAnswer(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.created\n"+
			`data: {"type":"response.created","response":{"id":"resp_1","created_at":1700000000,"model":"gpt-4.1","status":"in_progress"}}`+"\n\n"+
			"event: response.output_text.delta\n"+
			`data: {"type":"response.output_text.delta","delta":"hello"}`+"\n\n"+
			"event: response.completed\n"+
			`data: {"type":"response.completed","response":{"id":"resp_1","created_at":1700000000,"model":"gpt-4.1","status":"completed"}}`+"\n\n")
	}))
	defer upstream.Close()

	engine := conversionEngine(t, domain.Channel{
		ID: "line", BaseURL: upstream.URL, APIKey: "key", Enabled: true,
		SiteID: 1, Weight: 1, UpstreamAPI: protocol.APIResponses,
	})
	result, err := engine.Forward(context.Background(), domain.Request{
		Method:      http.MethodPost,
		Path:        protocol.PathChatCompletions,
		Headers:     http.Header{"Content-Type": []string{"application/json"}},
		Body:        []byte(`{"model":"gpt-4.1","messages":[{"role":"user","content":"hi"}],"stream":true}`),
		ContentType: "application/json",
		Model:       "gpt-4.1",
		Stream:      true,
	})
	if err != nil {
		t.Fatalf("Forward() error = %v", err)
	}
	defer result.Response.Body.Close()

	answer, err := io.ReadAll(result.Response.Body)
	if err != nil {
		t.Fatalf("read streamed answer: %v", err)
	}
	stream := string(answer)
	if !strings.Contains(stream, `"object":"chat.completion.chunk"`) {
		t.Errorf("stream = %s, want chat completion chunks", stream)
	}
	if !strings.Contains(stream, `"content":"hello"`) {
		t.Errorf("stream = %s, want the translated content", stream)
	}
	if !strings.HasSuffix(stream, "data: [DONE]\n\n") {
		t.Errorf("stream does not end with [DONE]: %q", stream)
	}
	if got := result.Response.Header.Get("Content-Length"); got != "" {
		t.Errorf("Content-Length = %q, want none on a translated stream", got)
	}
}

// A request one line cannot express is tried on the next line rather than
// failed outright: the first line is skipped, not blamed, and a Responses line
// behind it answers the same request unchanged.
func TestEngineSkipsALineThatCannotExpressTheRequest(t *testing.T) {
	var chatCalls int
	chatLine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chatCalls++
		w.WriteHeader(http.StatusOK)
	}))
	defer chatLine.Close()

	responsesLine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != protocol.PathResponses {
			t.Errorf("upstream path = %q, want %q", r.URL.Path, protocol.PathResponses)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_1","created_at":1,"model":"m","status":"completed","output":[]}`))
	}))
	defer responsesLine.Close()

	engine := &Engine{
		Selector: router.NewMemorySelector(routeFor(nil,
			domain.Channel{
				ID: "chat", BaseURL: chatLine.URL, Enabled: true,
				SiteID: 1, SitePriority: 20, Weight: 1, UpstreamAPI: protocol.APIChatCompletions,
			},
			domain.Channel{
				ID: "responses", BaseURL: responsesLine.URL, Enabled: true,
				SiteID: 2, SitePriority: 10, Weight: 1, UpstreamAPI: protocol.APIResponses,
			},
		)),
		Client: &http.Client{},
		Policy: domain.RetryPolicy{MaxAttempts: 2, MaxAttemptsPerChannel: 1},
		Sleep:  noSleep,
	}
	result, err := engine.Forward(context.Background(), domain.Request{
		Method:      http.MethodPost,
		Path:        protocol.PathResponses,
		Body:        []byte(`{"model":"m","previous_response_id":"resp_0","input":"again"}`),
		ContentType: "application/json",
		Model:       "m",
	})
	if err != nil {
		t.Fatalf("Forward() error = %v, want the second line to answer", err)
	}
	defer result.Response.Body.Close()
	if result.Attempt.ChannelID != "responses" {
		t.Fatalf("successful channel = %q, want the Responses line", result.Attempt.ChannelID)
	}
	if chatCalls != 0 {
		t.Errorf("chat line calls = %d, want none: it could not express the request", chatCalls)
	}
	if len(result.Trace.Attempts) != 2 || result.Trace.Attempts[0].Error == "" {
		t.Errorf("trace = %+v, want the skipped line recorded with its reason", result.Trace.Attempts)
	}
}

// A caller's request the line cannot express is reported before anything is
// sent, as a conversion failure the API layer answers as a request error.
func TestEngineReportsAnUnconvertibleRequestBeforeDispatch(t *testing.T) {
	var calls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
	}))
	defer upstream.Close()

	engine := conversionEngine(t, domain.Channel{
		ID: "line", BaseURL: upstream.URL, APIKey: "key", Enabled: true,
		SiteID: 1, Weight: 1, UpstreamAPI: protocol.APIResponses,
	})
	_, err := engine.Forward(context.Background(), domain.Request{
		Method:      http.MethodPost,
		Path:        protocol.PathChatCompletions,
		Body:        []byte(`{"model":"gpt-4.1","messages":[{"role":"banana","content":"hi"}]}`),
		ContentType: "application/json",
		Model:       "gpt-4.1",
	})
	if err == nil || !errors.Is(err, protocol.ErrRequestConversion) {
		t.Fatalf("Forward() error = %v, want ErrRequestConversion", err)
	}
	if calls != 0 {
		t.Errorf("upstream calls = %d, want none: the request was never expressible", calls)
	}
}
