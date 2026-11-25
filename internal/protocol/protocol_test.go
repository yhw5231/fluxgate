package protocol

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// document decodes a translated body so a test can state what it expects field
// by field instead of comparing one brittle string.
func document(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return decoded
}

func nested(t *testing.T, value any, path ...string) any {
	t.Helper()
	current := value
	for _, name := range path {
		object, ok := current.(map[string]any)
		if !ok {
			t.Fatalf("%v is not an object", current)
		}
		current = object[name]
	}
	return current
}

func TestResolveNamesTheTranslationOneAttemptNeeds(t *testing.T) {
	cases := []struct {
		name   string
		path   string
		stored string
		want   Conversion
	}{
		{"chat caller, chat line", PathChatCompletions, APIChatCompletions, Conversion{}},
		{"chat caller, no line protocol", PathChatCompletions, "", Conversion{}},
		{"chat caller, responses line", PathChatCompletions, APIResponses, Conversion{From: APIChatCompletions, To: APIResponses}},
		{"responses caller, chat line", PathResponses, APIChatCompletions, Conversion{From: APIResponses, To: APIChatCompletions}},
		{"responses caller, responses line", PathResponses, APIResponses, Conversion{}},
		{"other endpoint", "/v1/embeddings", APIResponses, Conversion{}},
		{"stored synonym", PathChatCompletions, "responses", Conversion{From: APIChatCompletions, To: APIResponses}},
		{"unknown stored value", PathChatCompletions, "grpc", Conversion{}},
	}
	for _, testcase := range cases {
		t.Run(testcase.name, func(t *testing.T) {
			if got := Resolve(testcase.path, testcase.stored); got != testcase.want {
				t.Fatalf("Resolve(%q, %q) = %+v, want %+v", testcase.path, testcase.stored, got, testcase.want)
			}
		})
	}
}

func TestChatRequestBecomesResponsesRequest(t *testing.T) {
	request := []byte(`{
		"model": "gpt-4.1",
		"messages": [
			{"role": "system", "content": "be brief"},
			{"role": "user", "content": [
				{"type": "text", "text": "what is this"},
				{"type": "image_url", "image_url": {"url": "https://example.com/a.png", "detail": "low"}}
			]},
			{"role": "assistant", "content": null, "tool_calls": [
				{"id": "call_1", "type": "function", "function": {"name": "lookup", "arguments": "{\"q\":\"x\"}"}}
			]},
			{"role": "tool", "tool_call_id": "call_1", "content": "the result"}
		],
		"tools": [
			{"type": "function", "function": {"name": "lookup", "description": "look it up", "parameters": {"type": "object"}, "strict": true}}
		],
		"tool_choice": {"type": "function", "function": {"name": "lookup"}},
		"response_format": {"type": "json_schema", "json_schema": {"name": "answer", "schema": {"type": "object"}, "strict": true}},
		"max_tokens": 100,
		"temperature": 0.2,
		"reasoning_effort": "low",
		"stream": true
	}`)

	converted, err := ConvertRequest(Conversion{From: APIChatCompletions, To: APIResponses}, request)
	if err != nil {
		t.Fatalf("ConvertRequest() error = %v", err)
	}
	body := document(t, converted)
	if got := body["model"]; got != "gpt-4.1" {
		t.Errorf("model = %v, want gpt-4.1", got)
	}
	if got := body["instructions"]; got != "be brief" {
		t.Errorf("instructions = %v, want the system message", got)
	}
	input, ok := body["input"].([]any)
	if !ok || len(input) != 3 {
		t.Fatalf("input = %#v, want a user message, a function call, and its output", body["input"])
	}
	user, _ := input[0].(map[string]any)
	if user["role"] != "user" {
		t.Errorf("first input item = %v, want a user message", user)
	}
	parts, _ := user["content"].([]any)
	if len(parts) != 2 {
		t.Fatalf("user content = %#v, want text and image parts", user["content"])
	}
	image, _ := parts[1].(map[string]any)
	if image["type"] != "input_image" || image["image_url"] != "https://example.com/a.png" || image["detail"] != "low" {
		t.Errorf("image part = %v, want an input_image with its address and detail", image)
	}
	call, _ := input[1].(map[string]any)
	if call["type"] != "function_call" || call["call_id"] != "call_1" || call["name"] != "lookup" {
		t.Errorf("function call item = %v", call)
	}
	result, _ := input[2].(map[string]any)
	if result["type"] != "function_call_output" || result["call_id"] != "call_1" || result["output"] != "the result" {
		t.Errorf("function output item = %v", result)
	}
	if got := body["max_output_tokens"]; got != float64(100) {
		t.Errorf("max_output_tokens = %v, want the chat completion's budget", got)
	}
	if got := body["stream"]; got != true {
		t.Errorf("stream = %v, want true", got)
	}
	if got := nested(t, body, "reasoning", "effort"); got != "low" {
		t.Errorf("reasoning.effort = %v, want low", got)
	}
	if got := nested(t, body, "text", "format", "type"); got != "json_schema" {
		t.Errorf("text.format.type = %v, want json_schema", got)
	}
	if got := nested(t, body, "text", "format", "name"); got != "answer" {
		t.Errorf("text.format.name = %v, want answer", got)
	}
	tools, _ := body["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %#v, want one function", body["tools"])
	}
	tool, _ := tools[0].(map[string]any)
	if tool["type"] != "function" || tool["name"] != "lookup" || tool["strict"] != true {
		t.Errorf("tool = %v, want the function at the top level", tool)
	}
	if got := nested(t, body, "tool_choice", "name"); got != "lookup" {
		t.Errorf("tool_choice.name = %v, want lookup", got)
	}
}

func TestResponsesRequestBecomesChatRequest(t *testing.T) {
	request := []byte(`{
		"model": "gpt-4.1",
		"instructions": "be brief",
		"input": [
			{"role": "user", "content": [
				{"type": "input_text", "text": "what is this"},
				{"type": "input_image", "image_url": "https://example.com/a.png"}
			]},
			{"type": "function_call", "call_id": "call_1", "name": "lookup", "arguments": "{\"q\":\"x\"}"},
			{"type": "function_call", "call_id": "call_2", "name": "other", "arguments": "{}"},
			{"type": "function_call_output", "call_id": "call_1", "output": "the result"}
		],
		"tools": [{"type": "function", "name": "lookup", "description": "look it up", "parameters": {"type": "object"}}],
		"tool_choice": {"type": "function", "name": "lookup"},
		"max_output_tokens": 50,
		"text": {"format": {"type": "json_object"}},
		"reasoning": {"effort": "high"},
		"stream": true
	}`)

	converted, err := ConvertRequest(Conversion{From: APIResponses, To: APIChatCompletions}, request)
	if err != nil {
		t.Fatalf("ConvertRequest() error = %v", err)
	}
	body := document(t, converted)
	messages, ok := body["messages"].([]any)
	if !ok || len(messages) != 4 {
		t.Fatalf("messages = %#v, want system, user, assistant with both calls, and tool", body["messages"])
	}
	system, _ := messages[0].(map[string]any)
	if system["role"] != "system" || system["content"] != "be brief" {
		t.Errorf("system message = %v", system)
	}
	user, _ := messages[1].(map[string]any)
	if user["role"] != "user" {
		t.Errorf("user message = %v", user)
	}
	userParts, _ := user["content"].([]any)
	if len(userParts) != 2 {
		t.Fatalf("user content = %#v, want text and image", user["content"])
	}
	image, _ := userParts[1].(map[string]any)
	if image["type"] != "image_url" {
		t.Errorf("image part = %v, want a chat image_url part", image)
	}
	assistant, _ := messages[2].(map[string]any)
	calls, _ := assistant["tool_calls"].([]any)
	if len(calls) != 2 {
		t.Fatalf("assistant tool calls = %#v, want both calls in one message", assistant["tool_calls"])
	}
	second, _ := calls[1].(map[string]any)
	if second["id"] != "call_2" || nested(t, second, "function", "name") != "other" {
		t.Errorf("second tool call = %v", second)
	}
	result, _ := messages[3].(map[string]any)
	if result["role"] != "tool" || result["tool_call_id"] != "call_1" || result["content"] != "the result" {
		t.Errorf("tool message = %v", result)
	}
	if got := body["max_tokens"]; got != float64(50) {
		t.Errorf("max_tokens = %v, want the output budget", got)
	}
	if got := body["reasoning_effort"]; got != "high" {
		t.Errorf("reasoning_effort = %v, want high", got)
	}
	if got := nested(t, body, "response_format", "type"); got != "json_object" {
		t.Errorf("response_format.type = %v, want json_object", got)
	}
	if got := nested(t, body, "stream_options", "include_usage"); got != true {
		t.Errorf("stream_options.include_usage = %v, want true so the answer states its cost", got)
	}
	tools, _ := body["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %#v", body["tools"])
	}
	tool, _ := tools[0].(map[string]any)
	if nested(t, tool, "function", "name") != "lookup" {
		t.Errorf("tool = %v, want the function nested as chat completions expects", tool)
	}
	if got := nested(t, body, "tool_choice", "function", "name"); got != "lookup" {
		t.Errorf("tool_choice.function.name = %v, want lookup", got)
	}
}

func TestUnconvertibleRequestIsReportedAsARequestError(t *testing.T) {
	request := []byte(`{"model":"gpt-4.1","previous_response_id":"resp_1","input":"and then?"}`)
	if _, err := ConvertRequest(Conversion{From: APIResponses, To: APIChatCompletions}, request); !errors.Is(err, ErrRequestConversion) {
		t.Fatalf("ConvertRequest() error = %v, want ErrRequestConversion", err)
	}
	if _, err := ConvertRequest(Conversion{From: APIChatCompletions, To: APIResponses}, []byte(`not json`)); !errors.Is(err, ErrRequestConversion) {
		t.Fatalf("ConvertRequest() for a non-object error = %v, want ErrRequestConversion", err)
	}
}

func TestResponsesAnswerBecomesChatCompletion(t *testing.T) {
	answer := []byte(`{
		"id": "resp_123",
		"created_at": 1700000000,
		"model": "gpt-4.1",
		"status": "completed",
		"output": [
			{"type": "message", "id": "msg_1", "content": [{"type": "output_text", "text": "hello"}]},
			{"type": "function_call", "id": "fc_1", "call_id": "call_1", "name": "lookup", "arguments": "{\"q\":\"x\"}"}
		],
		"usage": {"input_tokens": 10, "output_tokens": 5, "total_tokens": 15}
	}`)

	converted, err := convertAnswer(Conversion{From: APIResponses, To: APIChatCompletions}, answer)
	if err != nil {
		t.Fatalf("convertAnswer() error = %v", err)
	}
	body := document(t, converted)
	if body["id"] != "resp_123" || body["object"] != "chat.completion" || body["model"] != "gpt-4.1" {
		t.Errorf("envelope = %v", body)
	}
	choices, _ := body["choices"].([]any)
	if len(choices) != 1 {
		t.Fatalf("choices = %#v", body["choices"])
	}
	choice, _ := choices[0].(map[string]any)
	if choice["finish_reason"] != "tool_calls" {
		t.Errorf("finish_reason = %v, want tool_calls", choice["finish_reason"])
	}
	if got := nested(t, choice, "message", "content"); got != "hello" {
		t.Errorf("message content = %v, want hello", got)
	}
	calls, _ := nested(t, choice, "message", "tool_calls").([]any)
	if len(calls) != 1 || nested(t, calls[0], "function", "name") != "lookup" {
		t.Errorf("tool calls = %#v", calls)
	}
	if got := nested(t, body, "usage", "prompt_tokens"); got != float64(10) {
		t.Errorf("usage.prompt_tokens = %v, want 10", got)
	}
	if got := nested(t, body, "usage", "completion_tokens"); got != float64(5) {
		t.Errorf("usage.completion_tokens = %v, want 5", got)
	}
}

func TestResponsesIncompleteAnswerBecomesALengthFinish(t *testing.T) {
	answer := []byte(`{
		"id": "resp_1", "created_at": 1, "model": "m", "status": "incomplete",
		"incomplete_details": {"reason": "max_output_tokens"},
		"output": [{"type": "message", "content": [{"type": "output_text", "text": "half"}]}]
	}`)
	converted, err := convertAnswer(Conversion{From: APIResponses, To: APIChatCompletions}, answer)
	if err != nil {
		t.Fatalf("convertAnswer() error = %v", err)
	}
	if got := nested(t, document(t, converted), "choices"); got == nil {
		t.Fatal("converted answer has no choices")
	}
	body := document(t, converted)
	choices, _ := body["choices"].([]any)
	choice, _ := choices[0].(map[string]any)
	if choice["finish_reason"] != "length" {
		t.Errorf("finish_reason = %v, want length", choice["finish_reason"])
	}
}

func TestChatAnswerBecomesResponsesObject(t *testing.T) {
	answer := []byte(`{
		"id": "chatcmpl-abc",
		"created": 1700000000,
		"model": "gpt-4.1",
		"choices": [{"index": 0, "message": {
			"role": "assistant", "content": "hello",
			"tool_calls": [{"id": "call_1", "type": "function", "function": {"name": "lookup", "arguments": "{}"}}]
		}, "finish_reason": "tool_calls"}],
		"usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}
	}`)

	converted, err := convertAnswer(Conversion{From: APIChatCompletions, To: APIResponses}, answer)
	if err != nil {
		t.Fatalf("convertAnswer() error = %v", err)
	}
	body := document(t, converted)
	if body["id"] != "resp_abc" || body["object"] != "response" || body["status"] != "completed" {
		t.Errorf("envelope = %v", body)
	}
	if body["output_text"] != "hello" {
		t.Errorf("output_text = %v, want hello", body["output_text"])
	}
	output, _ := body["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("output = %#v, want a message and a function call", body["output"])
	}
	message, _ := output[0].(map[string]any)
	if message["type"] != "message" || message["role"] != "assistant" {
		t.Errorf("message item = %v", message)
	}
	if got := nested(t, message, "content"); got == nil {
		t.Error("message item carries no content")
	}
	call, _ := output[1].(map[string]any)
	if call["type"] != "function_call" || call["call_id"] != "call_1" || call["name"] != "lookup" {
		t.Errorf("function item = %v", call)
	}
	if got := nested(t, body, "usage", "input_tokens"); got != float64(10) {
		t.Errorf("usage.input_tokens = %v, want 10", got)
	}
	if got := nested(t, body, "usage", "output_tokens"); got != float64(5) {
		t.Errorf("usage.output_tokens = %v, want 5", got)
	}
}

func TestConvertResponseRewritesAnOrdinaryAnswer(t *testing.T) {
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type":   []string{"application/json"},
			"Content-Length": []string{"1"},
		},
		Body: io.NopCloser(strings.NewReader(`{"id":"chatcmpl-1","created":1,"model":"m","choices":[{"message":{"content":"hi"},"finish_reason":"stop"}]}`)),
	}
	// The caller asked in the Responses protocol and the line speaks chat
	// completions, so the answer travels chat completions -> Responses.
	converted := ConvertResponse(Conversion{From: APIResponses, To: APIChatCompletions}, response)
	body, err := io.ReadAll(converted.Body)
	if err != nil {
		t.Fatalf("read converted body: %v", err)
	}
	if got := converted.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	if got := converted.Header.Get("Content-Length"); got != strconv.Itoa(len(body)) {
		t.Errorf("Content-Length = %q, want the converted length %d", got, len(body))
	}
	if !strings.Contains(string(body), `"object":"response"`) {
		t.Errorf("converted body = %s, want a Responses answer", body)
	}
}

func TestConvertResponseLeavesAFailedOrUnreadableAnswerAlone(t *testing.T) {
	failed := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"slow down"}}`)),
	}
	if got := ConvertResponse(Conversion{From: APIChatCompletions, To: APIResponses}, failed); got != failed {
		t.Fatal("a failed answer was converted")
	}

	unreadable := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`not json`)),
	}
	converted := ConvertResponse(Conversion{From: APIResponses, To: APIChatCompletions}, unreadable)
	body, err := io.ReadAll(converted.Body)
	if err != nil {
		t.Fatalf("read passed-through body: %v", err)
	}
	if string(body) != "not json" {
		t.Fatalf("body = %q, want the upstream's bytes unchanged", body)
	}

	// An upstream that reports an error under a success status is forwarded as
	// it is, rather than turned into an empty completion.
	reportedError := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"quota exhausted"}}`)),
	}
	converted = ConvertResponse(Conversion{From: APIResponses, To: APIChatCompletions}, reportedError)
	body, err = io.ReadAll(converted.Body)
	if err != nil {
		t.Fatalf("read reported error body: %v", err)
	}
	if !strings.Contains(string(body), "quota exhausted") {
		t.Fatalf("body = %q, want the upstream's error unchanged", body)
	}
}

// sseFrames splits an event stream into its frames, each one the bytes between
// two blank lines.
func sseFrames(raw string) []string {
	normalized := strings.ReplaceAll(raw, "\r\n", "\n")
	chunks := strings.Split(normalized, "\n\n")
	frames := make([]string, 0, len(chunks))
	for _, chunk := range chunks {
		if strings.TrimSpace(chunk) != "" {
			frames = append(frames, chunk)
		}
	}
	return frames
}

func frameData(t *testing.T, frame string) map[string]any {
	t.Helper()
	payload := ""
	for _, line := range strings.Split(frame, "\n") {
		if strings.HasPrefix(line, "data: ") {
			payload = strings.TrimPrefix(line, "data: ")
		}
		if line == "data: [DONE]" {
			return nil
		}
	}
	if payload == "" {
		t.Fatalf("frame %q carries no data", frame)
	}
	return document(t, []byte(payload))
}
