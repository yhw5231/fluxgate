package protocol

import (
	"io"
	"strings"
	"testing"
)

func readConvertedStream(t *testing.T, conversion Conversion, input string) string {
	t.Helper()
	converter := newStreamConverter(conversion, io.NopCloser(strings.NewReader(input)))
	converted, err := io.ReadAll(converter)
	if err != nil {
		t.Fatalf("read converted stream: %v", err)
	}
	return string(converted)
}

func eventName(frame string) string {
	for _, line := range strings.Split(frame, "\n") {
		if strings.HasPrefix(line, "event: ") {
			return strings.TrimPrefix(line, "event: ")
		}
	}
	return ""
}

func TestResponsesStreamBecomesChatStream(t *testing.T) {
	input := strings.Join([]string{
		"event: response.created",
		`data: {"type":"response.created","response":{"id":"resp_1","created_at":1700000000,"model":"gpt-4.1","status":"in_progress"}}`,
		"",
		"event: response.output_text.delta",
		`data: {"type":"response.output_text.delta","delta":"hel"}`,
		"",
		"event: response.output_text.delta",
		`data: {"type":"response.output_text.delta","delta":"lo"}`,
		"",
		"event: response.completed",
		`data: {"type":"response.completed","response":{"id":"resp_1","created_at":1700000000,"model":"gpt-4.1","status":"completed","usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}}`,
		"",
	}, "\n")
	converted := readConvertedStream(t, Conversion{From: APIResponses, To: APIChatCompletions}, input)
	frames := sseFrames(converted)
	if len(frames) < 4 {
		t.Fatalf("frames = %d, want a role chunk, two deltas, a finish, and DONE: %q", len(frames), converted)
	}
	first := frameData(t, frames[0])
	if first["object"] != "chat.completion.chunk" || first["id"] != "resp_1" {
		t.Errorf("first chunk = %v, want a chat completion chunk for resp_1", first)
	}
	firstChoices, _ := first["choices"].([]any)
	if len(firstChoices) == 0 {
		t.Fatalf("first chunk carries no choices")
	}
	firstChoice, _ := firstChoices[0].(map[string]any)
	firstDelta, _ := firstChoice["delta"].(map[string]any)
	if firstDelta["role"] != "assistant" {
		t.Errorf("first delta = %v, want the assistant role", firstChoice["delta"])
	}
	content := ""
	for _, frame := range frames[:len(frames)-1] {
		payload := frameData(t, frame)
		choices, _ := payload["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		choice, _ := choices[0].(map[string]any)
		delta, _ := choice["delta"].(map[string]any)
		if text, ok := delta["content"].(string); ok {
			content += text
		}
	}
	if content != "hello" {
		t.Errorf("content = %q, want hello", content)
	}
	final := frameData(t, frames[len(frames)-2])
	choices, _ := final["choices"].([]any)
	choice, _ := choices[0].(map[string]any)
	if choice["finish_reason"] != "stop" {
		t.Errorf("finish_reason = %v, want stop", choice["finish_reason"])
	}
	if got := nested(t, final, "usage", "prompt_tokens"); got != float64(3) {
		t.Errorf("usage.prompt_tokens = %v, want 3", got)
	}
	if got := nested(t, final, "usage", "completion_tokens"); got != float64(2) {
		t.Errorf("usage.completion_tokens = %v, want 2", got)
	}
	if frames[len(frames)-1] != "data: [DONE]" {
		t.Errorf("last frame = %q, want [DONE]", frames[len(frames)-1])
	}
}

func TestResponsesStreamFunctionCallBecomesChatToolCalls(t *testing.T) {
	input := strings.Join([]string{
		"event: response.created",
		`data: {"type":"response.created","response":{"id":"resp_1","created_at":1700000000,"model":"gpt-4.1","status":"in_progress"}}`,
		"",
		"event: response.output_item.added",
		`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"lookup","arguments":""}}`,
		"",
		"event: response.function_call_arguments.delta",
		`data: {"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"q\":"}`,
		"",
		"event: response.function_call_arguments.delta",
		`data: {"type":"response.function_call_arguments.delta","output_index":0,"delta":"\"x\"}"}`,
		"",
		"event: response.completed",
		`data: {"type":"response.completed","response":{"id":"resp_1","created_at":1700000000,"model":"gpt-4.1","status":"completed"}}`,
		"",
	}, "\n")
	converted := readConvertedStream(t, Conversion{From: APIResponses, To: APIChatCompletions}, input)
	frames := sseFrames(converted)

	var callID, name, arguments string
	for _, frame := range frames {
		if strings.TrimSpace(strings.TrimPrefix(frame, "data: ")) == "[DONE]" {
			continue
		}
		payload := frameData(t, frame)
		choices, _ := payload["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		choice, _ := choices[0].(map[string]any)
		delta, _ := choice["delta"].(map[string]any)
		calls, _ := delta["tool_calls"].([]any)
		for _, rawCall := range calls {
			call, _ := rawCall.(map[string]any)
			if id, ok := call["id"].(string); ok && id != "" {
				callID = id
			}
			function, _ := call["function"].(map[string]any)
			if value, ok := function["name"].(string); ok && value != "" {
				name = value
			}
			if fragment, ok := function["arguments"].(string); ok {
				arguments += fragment
			}
		}
	}
	if callID != "call_1" || name != "lookup" {
		t.Errorf("tool call = %q %q, want call_1 lookup", callID, name)
	}
	if arguments != `{"q":"x"}` {
		t.Errorf("arguments = %q", arguments)
	}
	final := frameData(t, frames[len(frames)-2])
	choices, _ := final["choices"].([]any)
	choice, _ := choices[0].(map[string]any)
	if choice["finish_reason"] != "tool_calls" {
		t.Errorf("finish_reason = %v, want tool_calls", choice["finish_reason"])
	}
}

func TestChatStreamBecomesResponsesStream(t *testing.T) {
	input := strings.Join([]string{
		`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1700000000,"model":"gpt-4.1","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
		"",
		`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"hel"},"finish_reason":null}]}`,
		"",
		`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":"stop"}]}`,
		"",
		`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	converted := readConvertedStream(t, Conversion{From: APIChatCompletions, To: APIResponses}, input)
	frames := sseFrames(converted)

	want := []string{
		"response.created",
		"response.in_progress",
		"response.output_item.added",
		"response.content_part.added",
		"response.output_text.delta",
		"response.output_text.delta",
		"response.output_text.done",
		"response.content_part.done",
		"response.output_item.done",
		"response.completed",
	}
	if len(frames) != len(want) {
		t.Fatalf("frames = %d %v, want %d %v", len(frames), frameNames(frames), len(want), want)
	}
	for index, name := range want {
		if got := eventName(frames[index]); got != name {
			t.Errorf("frame %d = %q, want %q", index, got, name)
		}
		payload := frameData(t, frames[index])
		if payload["type"] != name {
			t.Errorf("frame %d data.type = %v, want %s", index, payload["type"], name)
		}
		if _, ok := payload["sequence_number"]; !ok {
			t.Errorf("frame %d carries no sequence number", index)
		}
	}
	completed := frameData(t, frames[len(frames)-1])
	if got := nested(t, completed, "response", "status"); got != "completed" {
		t.Errorf("response.status = %v, want completed", got)
	}
	if got := nested(t, completed, "response", "output_text"); got != "hello" {
		t.Errorf("response.output_text = %v, want hello", got)
	}
	if got := nested(t, completed, "response", "usage", "input_tokens"); got != float64(3) {
		t.Errorf("usage.input_tokens = %v, want 3", got)
	}
	output, _ := nested(t, completed, "response", "output").([]any)
	if len(output) != 1 {
		t.Fatalf("response.output = %#v, want one assistant message", output)
	}
	message, _ := output[0].(map[string]any)
	if message["type"] != "message" || message["status"] != "completed" {
		t.Errorf("output item = %v", message)
	}
	parts, _ := message["content"].([]any)
	part, _ := parts[0].(map[string]any)
	if part["text"] != "hello" {
		t.Errorf("output part = %v, want the whole text", part)
	}
}

func frameNames(frames []string) []string {
	names := make([]string, 0, len(frames))
	for _, frame := range frames {
		names = append(names, eventName(frame))
	}
	return names
}

func TestChatStreamToolCallBecomesResponsesFunctionCall(t *testing.T) {
	input := strings.Join([]string{
		`data: {"id":"chatcmpl-1","created":1700000000,"model":"gpt-4.1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"lookup","arguments":""}}]},"finish_reason":null}]}`,
		"",
		`data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"q\":"}}]},"finish_reason":null}]}`,
		"",
		`data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"x\"}"}}]},"finish_reason":"tool_calls"}]}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	converted := readConvertedStream(t, Conversion{From: APIChatCompletions, To: APIResponses}, input)
	frames := sseFrames(converted)

	var added map[string]any
	arguments := ""
	for _, frame := range frames {
		switch eventName(frame) {
		case "response.output_item.added":
			payload := frameData(t, frame)
			if item, ok := payload["item"].(map[string]any); ok && item["type"] == "function_call" {
				added = item
			}
		case "response.function_call_arguments.delta":
			payload := frameData(t, frame)
			if delta, ok := payload["delta"].(string); ok {
				arguments += delta
			}
		}
	}
	if added == nil {
		t.Fatalf("no function call item was announced: %v", frameNames(frames))
	}
	if added["call_id"] != "call_1" || added["name"] != "lookup" {
		t.Errorf("function item = %v", added)
	}
	if arguments != `{"q":"x"}` {
		t.Errorf("arguments = %q", arguments)
	}
	completed := frameData(t, frames[len(frames)-1])
	output, _ := nested(t, completed, "response", "output").([]any)
	if len(output) != 1 {
		t.Fatalf("output = %#v", output)
	}
	item, _ := output[0].(map[string]any)
	if item["arguments"] != `{"q":"x"}` || item["status"] != "completed" {
		t.Errorf("completed function item = %v", item)
	}
}

func TestChatStreamEndingWithoutDoneStillCompletes(t *testing.T) {
	input := "data: {\"id\":\"chatcmpl-1\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":null}]}\n\n"
	converted := readConvertedStream(t, Conversion{From: APIChatCompletions, To: APIResponses}, input)
	frames := sseFrames(converted)
	if len(frames) == 0 || eventName(frames[len(frames)-1]) != "response.completed" {
		t.Fatalf("last event = %q, want response.completed", eventName(frames[len(frames)-1]))
	}
}
