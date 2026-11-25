package protocol

import (
	"errors"
	"fmt"
	"strings"
)

// Request translation.
//
// Both protocols carry the same conversation in a different arrangement: chat
// completions send one message list, where a tool call is a field of an assistant
// message and a tool result is a message of its own; the Responses API sends a
// flat list of input items, where a tool call and a tool result are items beside
// the messages. Translating therefore means moving the conversation between those
// two arrangements and carrying over the request parameters that have a
// counterpart: the instruction text, the token budget, the tools, the output
// format, and the sampling controls.
//
// Parameters with no counterpart are dropped rather than guessed at. A chat
// completion's stop sequence has no field in the Responses API, and inventing one
// would send the upstream a value it does not understand.

// chatRequestToResponses arranges a chat completion request as a Responses API
// request.
func chatRequestToResponses(request map[string]any) (map[string]any, error) {
	messages, ok := array(request["messages"])
	if !ok || len(messages) == 0 {
		return nil, errors.New("the chat completion request carries no messages")
	}

	instructions := make([]string, 0, 2)
	input := make([]any, 0, len(messages))
	for index, raw := range messages {
		message, ok := object(raw)
		if !ok {
			return nil, fmt.Errorf("message %d is not a JSON object", index)
		}
		role := strings.ToLower(stringField(message, "role"))
		switch role {
		case "system", "developer":
			// The Responses API states the standing instruction once, beside the
			// conversation rather than inside it.
			if text := anyText(message["content"]); text != "" {
				instructions = append(instructions, text)
			}
		case "user":
			content, err := chatContentToResponses(message["content"])
			if err != nil {
				return nil, fmt.Errorf("message %d: %w", index, err)
			}
			input = append(input, map[string]any{"role": "user", "content": content})
		case "assistant":
			if text := anyText(message["content"]); text != "" {
				input = append(input, map[string]any{"role": "assistant", "content": text})
			}
			toolCalls, _ := array(message["tool_calls"])
			for position, rawCall := range toolCalls {
				call, ok := object(rawCall)
				if !ok {
					return nil, fmt.Errorf("message %d tool call %d is not a JSON object", index, position)
				}
				function, _ := object(call["function"])
				name := stringField(function, "name")
				if name == "" {
					return nil, fmt.Errorf("message %d tool call %d names no function", index, position)
				}
				callID := stringField(call, "id")
				if callID == "" {
					// A Responses function call is paired with its result by the
					// call identifier, so one that was not sent still has to be
					// named for the conversation to stay readable.
					callID = fmt.Sprintf("call_%d_%d", index, position)
				}
				input = append(input, map[string]any{
					"type":      "function_call",
					"call_id":   callID,
					"name":      name,
					"arguments": stringField(function, "arguments"),
				})
			}
		case "tool", "function":
			callID := stringField(message, "tool_call_id")
			if callID == "" {
				return nil, fmt.Errorf("message %d is a tool result with no tool_call_id", index)
			}
			input = append(input, map[string]any{
				"type":    "function_call_output",
				"call_id": callID,
				"output":  anyText(message["content"]),
			})
		default:
			return nil, fmt.Errorf("message %d has the role %q, which the Responses API has no counterpart for", index, role)
		}
	}

	converted := map[string]any{"input": input}
	copyModel(request, converted)
	if choices, ok := request["n"].(float64); ok && choices > 1 {
		// The Responses API answers one choice. Answering one where several were
		// asked for would be a silently different request, so the caller is told
		// instead.
		return nil, errors.New("the chat completion request asks for several choices, which the Responses API cannot return")
	}
	if len(instructions) > 0 {
		converted["instructions"] = strings.Join(instructions, "\n\n")
	}
	if stream, ok := boolField(request, "stream"); ok {
		converted["stream"] = stream
	}
	copyField(request, converted, "temperature", "top_p", "user", "parallel_tool_calls")
	if budget, ok := numberFrom(request, "max_completion_tokens", "max_tokens"); ok {
		converted["max_output_tokens"] = budget
	}
	if effort := stringField(request, "reasoning_effort"); effort != "" {
		converted["reasoning"] = map[string]any{"effort": effort}
	}
	if tools, err := chatToolsToResponses(request["tools"]); err != nil {
		return nil, err
	} else if len(tools) > 0 {
		converted["tools"] = tools
		if choice, ok := chatToolChoiceToResponses(request["tool_choice"]); ok {
			converted["tool_choice"] = choice
		}
	}
	if format, ok := chatResponseFormatToResponses(request["response_format"]); ok {
		converted["text"] = map[string]any{"format": format}
	}
	return converted, nil
}

// responsesRequestToChat arranges a Responses API request as a chat completion
// request.
func responsesRequestToChat(request map[string]any) (map[string]any, error) {
	if previous := stringField(request, "previous_response_id"); previous != "" {
		// The earlier turns live at the upstream under that identifier. A chat
		// completion upstream cannot continue them, and dropping the reference
		// would silently answer as if the conversation had just started.
		return nil, fmt.Errorf("the request continues response %q, which a Chat Completions upstream cannot continue", previous)
	}

	messages := make([]any, 0, 4)
	if instructions := stringField(request, "instructions"); instructions != "" {
		messages = append(messages, map[string]any{"role": "system", "content": instructions})
	}

	switch input := request["input"].(type) {
	case nil:
	case string:
		if input != "" {
			messages = append(messages, map[string]any{"role": "user", "content": input})
		}
	case []any:
		for index, raw := range input {
			converted, err := responsesInputItemToChat(raw, index)
			if err != nil {
				return nil, err
			}
			messages = append(messages, converted...)
		}
	default:
		return nil, errors.New("the Responses request input is neither text nor a list of items")
	}
	messages = mergeToolCallMessages(messages)
	if len(messages) == 0 {
		return nil, errors.New("the Responses request carries no input")
	}

	converted := map[string]any{"messages": messages}
	copyModel(request, converted)
	if stream, ok := boolField(request, "stream"); ok {
		converted["stream"] = stream
		if stream {
			// The caller asked for a streamed Responses answer, which carries the
			// token usage in its final event. A chat upstream states usage only
			// when asked to, so it is asked here; without this the record of the
			// request would have no cost to report.
			converted["stream_options"] = map[string]any{"include_usage": true}
		}
	}
	copyField(request, converted, "temperature", "top_p", "user", "parallel_tool_calls")
	if budget, ok := numberField(request, "max_output_tokens"); ok {
		converted["max_tokens"] = budget
	}
	if reasoning, ok := object(request["reasoning"]); ok {
		if effort := stringField(reasoning, "effort"); effort != "" {
			converted["reasoning_effort"] = effort
		}
	}
	if tools, err := responsesToolsToChat(request["tools"]); err != nil {
		return nil, err
	} else if len(tools) > 0 {
		converted["tools"] = tools
		if choice, ok := responsesToolChoiceToChat(request["tool_choice"]); ok {
			converted["tool_choice"] = choice
		}
	}
	if text, ok := object(request["text"]); ok {
		if format, ok := responsesTextFormatToChat(text["format"]); ok {
			converted["response_format"] = format
		}
	}
	return converted, nil
}

// responsesInputItemToChat turns one Responses input item into the chat messages
// it describes. A function call becomes an assistant message carrying the call,
// which is where chat completions keep one.
func responsesInputItemToChat(raw any, index int) ([]any, error) {
	item, ok := object(raw)
	if !ok {
		return nil, fmt.Errorf("input item %d is not a JSON object", index)
	}
	kind := stringField(item, "type")
	switch kind {
	case "", "message":
		role := strings.ToLower(stringField(item, "role"))
		if role == "" {
			role = "user"
		}
		content, err := responsesContentToChat(item["content"])
		if err != nil {
			return nil, fmt.Errorf("input item %d: %w", index, err)
		}
		return []any{map[string]any{"role": role, "content": content}}, nil
	case "function_call":
		callID := stringField(item, "call_id")
		if callID == "" {
			callID = stringField(item, "id")
		}
		name := stringField(item, "name")
		if callID == "" || name == "" {
			return nil, fmt.Errorf("input item %d is a function call without a call identifier and a name", index)
		}
		call := map[string]any{
			"id":   callID,
			"type": "function",
			"function": map[string]any{
				"name":      name,
				"arguments": stringField(item, "arguments"),
			},
		}
		return []any{map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{call}}}, nil
	case "function_call_output":
		callID := stringField(item, "call_id")
		if callID == "" {
			return nil, fmt.Errorf("input item %d is a tool result with no call identifier", index)
		}
		return []any{map[string]any{
			"role":         "tool",
			"tool_call_id": callID,
			"content":      anyText(item["output"]),
		}}, nil
	case "reasoning", "item_reference":
		// Server-side state with no chat counterpart, and nothing the
		// conversation needs in order to be read.
		return nil, nil
	default:
		// A provider may name its own message item type while still carrying a
		// role and content; that is still a message and is read as one.
		if role := strings.ToLower(stringField(item, "role")); role != "" {
			content, err := responsesContentToChat(item["content"])
			if err != nil {
				return nil, fmt.Errorf("input item %d: %w", index, err)
			}
			return []any{map[string]any{"role": role, "content": content}}, nil
		}
		return nil, fmt.Errorf("input item %d has the type %q, which a Chat Completions upstream has no counterpart for", index, kind)
	}
}

// mergeToolCallMessages folds the assistant messages that each carry one tool
// call into the single assistant message chat completions expect for one turn.
func mergeToolCallMessages(messages []any) []any {
	merged := make([]any, 0, len(messages))
	for _, raw := range messages {
		message, ok := object(raw)
		if !ok {
			merged = append(merged, raw)
			continue
		}
		calls, hasCalls := array(message["tool_calls"])
		if !hasCalls || len(calls) == 0 || message["content"] != nil || len(merged) == 0 {
			merged = append(merged, raw)
			continue
		}
		previous, ok := object(merged[len(merged)-1])
		if !ok || stringField(previous, "role") != "assistant" || previous["content"] != nil {
			merged = append(merged, raw)
			continue
		}
		existing, ok := array(previous["tool_calls"])
		if !ok {
			merged = append(merged, raw)
			continue
		}
		previous["tool_calls"] = append(existing, calls...)
	}
	return merged
}

// chatContentToResponses arranges message content the way the Responses API
// reads it: text becomes input_text, an image becomes input_image, and a list of
// nothing but text collapses back to a plain string, which every upstream
// accepts and some require.
func chatContentToResponses(content any) (any, error) {
	if text, ok := content.(string); ok {
		return text, nil
	}
	parts, ok := array(content)
	if !ok {
		if content == nil {
			return "", nil
		}
		return nil, errors.New("the message content is neither text nor a list of parts")
	}
	converted := make([]any, 0, len(parts))
	for _, raw := range parts {
		part, ok := object(raw)
		if !ok {
			continue
		}
		switch stringField(part, "type") {
		case "text", "input_text", "output_text":
			converted = append(converted, map[string]any{"type": "input_text", "text": stringField(part, "text")})
		case "image_url":
			url, detail := chatImage(part["image_url"])
			if url == "" {
				continue
			}
			image := map[string]any{"type": "input_image", "image_url": url}
			if detail != "" {
				image["detail"] = detail
			}
			converted = append(converted, image)
		}
	}
	if len(converted) == 0 {
		return "", nil
	}
	if text, ok := textPartsOnly(converted); ok {
		return text, nil
	}
	return converted, nil
}

// chatImage reads a chat completion's image reference, which is either the
// address itself or an object carrying the address and a detail level.
func chatImage(value any) (string, string) {
	if url, ok := value.(string); ok {
		return strings.TrimSpace(url), ""
	}
	image, ok := object(value)
	if !ok {
		return "", ""
	}
	return stringField(image, "url"), stringField(image, "detail")
}

// responsesContentToChat arranges message content the way chat completions read
// it. A list of nothing but text collapses to a plain string for the same reason
// as the reverse translation.
func responsesContentToChat(content any) (any, error) {
	if text, ok := content.(string); ok {
		return text, nil
	}
	parts, ok := array(content)
	if !ok {
		if content == nil {
			return "", nil
		}
		return nil, errors.New("the message content is neither text nor a list of parts")
	}
	converted := make([]any, 0, len(parts))
	for _, raw := range parts {
		part, ok := object(raw)
		if !ok {
			continue
		}
		switch stringField(part, "type") {
		case "input_text", "output_text", "text":
			converted = append(converted, map[string]any{"type": "text", "text": stringField(part, "text")})
		case "refusal":
			converted = append(converted, map[string]any{"type": "text", "text": stringField(part, "refusal")})
		case "input_image":
			url, detail := chatImage(part["image_url"])
			if url == "" {
				continue
			}
			imageURL := map[string]any{"url": url}
			if detail != "" {
				imageURL["detail"] = detail
			}
			converted = append(converted, map[string]any{"type": "image_url", "image_url": imageURL})
		case "input_file":
			file, _ := object(part["file"])
			if id := stringField(file, "file_id"); id != "" {
				converted = append(converted, map[string]any{"type": "file", "file": map[string]any{"file_id": id}})
			}
		}
	}
	if len(converted) == 0 {
		return "", nil
	}
	if text, ok := textPartsOnly(converted); ok {
		return text, nil
	}
	return converted, nil
}

// textPartsOnly reports whether every part is text, and if so the text they
// read as. It accepts the chat and the Responses spellings, because both
// translations build their parts before asking.
func textPartsOnly(parts []any) (string, bool) {
	var builder strings.Builder
	for _, raw := range parts {
		part, ok := object(raw)
		if !ok {
			return "", false
		}
		kind := part["type"]
		if kind != "text" && kind != "input_text" {
			return "", false
		}
		builder.WriteString(stringField(part, "text"))
	}
	return builder.String(), true
}

// chatToolsToResponses moves a chat completion tool list into the Responses
// shape, where the function is the tool rather than a field inside it.
func chatToolsToResponses(raw any) ([]any, error) {
	list, ok := array(raw)
	if !ok {
		if raw == nil {
			return nil, nil
		}
		return nil, errors.New("tools must be a list")
	}
	converted := make([]any, 0, len(list))
	for index, rawTool := range list {
		tool, ok := object(rawTool)
		if !ok {
			return nil, fmt.Errorf("tool %d is not a JSON object", index)
		}
		// A provider's own tool type has no Responses counterpart; dropping it
		// leaves the rest of the request intact.
		if kind := stringField(tool, "type"); kind != "" && kind != "function" {
			continue
		}
		function, ok := object(tool["function"])
		if !ok {
			continue
		}
		name := stringField(function, "name")
		if name == "" {
			return nil, fmt.Errorf("tool %d names no function", index)
		}
		item := map[string]any{"type": "function", "name": name}
		copyField(function, item, "description", "parameters", "strict")
		converted = append(converted, item)
	}
	return converted, nil
}

// responsesToolsToChat moves a Responses tool list into the chat completion
// shape, where the function is a field inside the tool.
func responsesToolsToChat(raw any) ([]any, error) {
	list, ok := array(raw)
	if !ok {
		if raw == nil {
			return nil, nil
		}
		return nil, errors.New("tools must be a list")
	}
	converted := make([]any, 0, len(list))
	for index, rawTool := range list {
		tool, ok := object(rawTool)
		if !ok {
			return nil, fmt.Errorf("tool %d is not a JSON object", index)
		}
		if stringField(tool, "type") != "function" {
			// Built-in tools — web search, file search, computer use — are served
			// by the Responses upstream itself and have no chat tool to become.
			continue
		}
		name := stringField(tool, "name")
		if name == "" {
			return nil, fmt.Errorf("tool %d names no function", index)
		}
		function := map[string]any{"name": name}
		copyField(tool, function, "description", "parameters", "strict")
		converted = append(converted, map[string]any{"type": "function", "function": function})
	}
	return converted, nil
}

// chatToolChoiceToResponses carries the tool choice across. A named function
// becomes the Responses function choice.
func chatToolChoiceToResponses(raw any) (any, bool) {
	if raw == nil {
		return nil, false
	}
	if text, ok := raw.(string); ok {
		switch text {
		case "auto", "none", "required":
			return text, true
		}
		return nil, false
	}
	choice, ok := object(raw)
	if !ok || stringField(choice, "type") != "function" {
		return nil, false
	}
	function, _ := object(choice["function"])
	name := stringField(function, "name")
	if name == "" {
		return nil, false
	}
	return map[string]any{"type": "function", "name": name}, true
}

// responsesToolChoiceToChat carries the tool choice the other way.
func responsesToolChoiceToChat(raw any) (any, bool) {
	if raw == nil {
		return nil, false
	}
	if text, ok := raw.(string); ok {
		switch text {
		case "auto", "none", "required":
			return text, true
		}
		return nil, false
	}
	choice, ok := object(raw)
	if !ok || stringField(choice, "type") != "function" {
		return nil, false
	}
	name := stringField(choice, "name")
	if name == "" {
		return nil, false
	}
	return map[string]any{"type": "function", "function": map[string]any{"name": name}}, true
}

// chatResponseFormatToResponses moves a chat output format into the Responses
// text format object.
func chatResponseFormatToResponses(raw any) (map[string]any, bool) {
	format, ok := object(raw)
	if !ok {
		return nil, false
	}
	switch stringField(format, "type") {
	case "text":
		return map[string]any{"type": "text"}, true
	case "json_object":
		return map[string]any{"type": "json_object"}, true
	case "json_schema":
		schema, ok := object(format["json_schema"])
		if !ok {
			return nil, false
		}
		converted := map[string]any{"type": "json_schema"}
		copyField(schema, converted, "name", "description", "schema", "strict")
		if _, present := converted["name"]; !present {
			converted["name"] = "response"
		}
		return converted, true
	}
	return nil, false
}

// responsesTextFormatToChat moves a Responses text format into the chat output
// format object.
func responsesTextFormatToChat(raw any) (map[string]any, bool) {
	format, ok := object(raw)
	if !ok {
		return nil, false
	}
	switch stringField(format, "type") {
	case "text":
		return map[string]any{"type": "text"}, true
	case "json_object":
		return map[string]any{"type": "json_object"}, true
	case "json_schema":
		schema := map[string]any{}
		copyField(format, schema, "name", "description", "schema", "strict")
		return map[string]any{"type": "json_schema", "json_schema": schema}, true
	}
	return nil, false
}
