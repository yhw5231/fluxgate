package protocol

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Answer translation.
//
// An answer is translated whole: the gateway has already received it as one
// document, so the only question is how the same answer is arranged for the other
// protocol. The text, the tool calls, the finish reason, and the token usage all
// move; the identifier moves too, so a client that logs it can find the request
// again.

// chatAnswer is the part of a chat completion answer the translation reads. The
// content is kept as raw JSON because a chat answer may state it as a string or
// as a list of parts.
type chatAnswer struct {
	ID      string `json:"id"`
	Created int64  `json:"created"`
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Content   json.RawMessage `json:"content"`
			ToolCalls []struct {
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage json.RawMessage `json:"usage"`
}

// responsesAnswerItem is one item of a Responses API answer: a message, a
// function call, or a reasoning item the translation ignores.
type responsesAnswerItem struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	Content   []struct {
		Type    string `json:"type"`
		Text    string `json:"text"`
		Refusal string `json:"refusal"`
	} `json:"content"`
}

// responsesAnswer is the part of a Responses API answer the translation reads.
type responsesAnswer struct {
	ID         string `json:"id"`
	CreatedAt  int64  `json:"created_at"`
	Model      string `json:"model"`
	Status     string `json:"status"`
	Incomplete struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
	Output     []responsesAnswerItem `json:"output"`
	OutputText string                `json:"output_text"`
	Usage      json.RawMessage       `json:"usage"`
}

// responsesAnswerToChat arranges a Responses API answer as a chat completion.
func responsesAnswerToChat(raw []byte) ([]byte, error) {
	var answer responsesAnswer
	if err := json.Unmarshal(raw, &answer); err != nil {
		return nil, fmt.Errorf("decode the Responses answer: %w", err)
	}

	text := answer.OutputText
	toolCalls := make([]any, 0)
	if text == "" {
		var builder strings.Builder
		for _, item := range answer.Output {
			if item.Type != "" && item.Type != "message" {
				continue
			}
			for _, part := range item.Content {
				if part.Text != "" {
					builder.WriteString(part.Text)
				}
				if part.Refusal != "" {
					builder.WriteString(part.Refusal)
				}
			}
		}
		text = builder.String()
	}
	for _, item := range answer.Output {
		if item.Type != "function_call" {
			continue
		}
		callID := strings.TrimSpace(item.CallID)
		if callID == "" {
			callID = strings.TrimSpace(item.ID)
		}
		if callID == "" {
			callID = fmt.Sprintf("call_%d_%s", len(toolCalls), shortID(answer.ID))
		}
		toolCalls = append(toolCalls, map[string]any{
			"id":   callID,
			"type": "function",
			"function": map[string]any{
				"name":      strings.TrimSpace(item.Name),
				"arguments": item.Arguments,
			},
		})
	}

	message := map[string]any{"role": "assistant"}
	switch {
	case len(toolCalls) > 0 && text == "":
		message["content"] = nil
		message["tool_calls"] = toolCalls
	case len(toolCalls) > 0:
		message["content"] = text
		message["tool_calls"] = toolCalls
	default:
		message["content"] = text
	}

	created := answer.CreatedAt
	if created == 0 {
		created = time.Now().Unix()
	}
	id := strings.TrimSpace(answer.ID)
	if id == "" {
		id = "chatcmpl-fluxgate-" + strconv.FormatInt(created, 10)
	}

	converted := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": created,
		"model":   answer.Model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       message,
			"finish_reason": responsesFinishReason(answer, len(toolCalls) > 0),
		}},
	}
	if usage := responsesUsageToChat(answer.Usage); usage != nil {
		converted["usage"] = usage
	}
	return json.Marshal(converted)
}

// responsesFinishReason names why a Responses answer ended, in the vocabulary a
// chat completion client understands.
func responsesFinishReason(answer responsesAnswer, tools bool) string {
	if tools {
		return "tool_calls"
	}
	if strings.EqualFold(answer.Status, "incomplete") {
		switch answer.Incomplete.Reason {
		case "max_output_tokens":
			return "length"
		case "content_filter":
			return "content_filter"
		}
	}
	return "stop"
}

// chatAnswerToResponses arranges a chat completion answer as a Responses API
// answer.
func chatAnswerToResponses(raw []byte) ([]byte, error) {
	var answer chatAnswer
	if err := json.Unmarshal(raw, &answer); err != nil {
		return nil, fmt.Errorf("decode the chat completion answer: %w", err)
	}
	if len(answer.Choices) == 0 {
		return nil, errors.New("the upstream answer carries no choices")
	}
	choice := answer.Choices[0]
	text := anyText(decodeContent(choice.Message.Content))

	created := answer.Created
	if created == 0 {
		created = time.Now().Unix()
	}
	responseID := responsesID(answer.ID, created)

	output := make([]any, 0, 1+len(choice.Message.ToolCalls))
	if text != "" || len(choice.Message.ToolCalls) == 0 {
		output = append(output, map[string]any{
			"id":     "msg_" + responseID,
			"type":   "message",
			"status": "completed",
			"role":   "assistant",
			"content": []any{map[string]any{
				"type":        "output_text",
				"text":        text,
				"annotations": []any{},
			}},
		})
	}
	for index, call := range choice.Message.ToolCalls {
		callID := strings.TrimSpace(call.ID)
		if callID == "" {
			callID = fmt.Sprintf("call_%d_%s", index, shortID(responseID))
		}
		output = append(output, map[string]any{
			"id":        fmt.Sprintf("fc_%s_%d", responseID, index),
			"type":      "function_call",
			"status":    "completed",
			"call_id":   callID,
			"name":      strings.TrimSpace(call.Function.Name),
			"arguments": call.Function.Arguments,
		})
	}

	status := "completed"
	var incomplete map[string]any
	if choice.FinishReason != nil {
		switch *choice.FinishReason {
		case "length":
			status = "incomplete"
			incomplete = map[string]any{"reason": "max_output_tokens"}
		case "content_filter":
			status = "incomplete"
			incomplete = map[string]any{"reason": "content_filter"}
		}
	}

	converted := map[string]any{
		"id":         responseID,
		"object":     "response",
		"created_at": created,
		"status":     status,
		"model":      answer.Model,
		"output":     output,
	}
	if text != "" {
		converted["output_text"] = text
	}
	if incomplete != nil {
		converted["incomplete_details"] = incomplete
	}
	if usage := chatUsageToResponses(answer.Usage); usage != nil {
		converted["usage"] = usage
	}
	return json.Marshal(converted)
}

// decodeContent decodes a message content field that was kept raw because chat
// completions state it either as a string or as a list of parts.
func decodeContent(raw json.RawMessage) any {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil
	}
	return decoded
}

// responsesID names the Responses answer that stands for a chat completion. A
// chat completion's identifier is kept recognizable inside it, so an operator
// holding either one can tell they describe the same answer.
func responsesID(chatID string, created int64) string {
	trimmed := strings.TrimSpace(chatID)
	switch {
	case trimmed == "":
		return "resp_fluxgate_" + strconv.FormatInt(created, 10)
	case strings.HasPrefix(trimmed, "resp_"):
		return trimmed
	case strings.HasPrefix(trimmed, "chatcmpl-"):
		return "resp_" + strings.TrimPrefix(trimmed, "chatcmpl-")
	default:
		return "resp_" + trimmed
	}
}

// shortID makes an identifier safe to embed in another identifier: a provider's
// id may carry punctuation that reads badly inside a generated one.
func shortID(id string) string {
	cleaned := strings.Map(func(character rune) rune {
		switch {
		case character >= 'a' && character <= 'z',
			character >= 'A' && character <= 'Z',
			character >= '0' && character <= '9',
			character == '_', character == '-':
			return character
		default:
			return '_'
		}
	}, strings.TrimSpace(id))
	if cleaned == "" {
		return "response"
	}
	return cleaned
}

// chatUsageToResponses states a chat completion's token usage in the Responses
// vocabulary.
func chatUsageToResponses(raw json.RawMessage) map[string]any {
	usage, ok := decodeObject(raw)
	if !ok {
		return nil
	}
	converted := map[string]any{}
	prompt, hasPrompt := usageNumber(usage, "prompt_tokens")
	completion, hasCompletion := usageNumber(usage, "completion_tokens")
	total, hasTotal := usageNumber(usage, "total_tokens")
	if hasPrompt {
		converted["input_tokens"] = prompt
	}
	if hasCompletion {
		converted["output_tokens"] = completion
	}
	if hasTotal {
		converted["total_tokens"] = total
	} else if hasPrompt || hasCompletion {
		converted["total_tokens"] = addNumbers(prompt, completion)
	}
	if details, ok := object(usage["prompt_tokens_details"]); ok {
		if cached, ok := usageNumber(details, "cached_tokens"); ok {
			converted["input_tokens_details"] = map[string]any{"cached_tokens": cached}
		}
	}
	if details, ok := object(usage["completion_tokens_details"]); ok {
		if reasoning, ok := usageNumber(details, "reasoning_tokens"); ok {
			converted["output_tokens_details"] = map[string]any{"reasoning_tokens": reasoning}
		}
	}
	if len(converted) == 0 {
		return nil
	}
	return converted
}

// responsesUsageToChat states a Responses API answer's token usage in the chat
// completion vocabulary.
func responsesUsageToChat(raw json.RawMessage) map[string]any {
	usage, ok := decodeObject(raw)
	if !ok {
		return nil
	}
	converted := map[string]any{}
	input, hasInput := usageNumber(usage, "input_tokens")
	output, hasOutput := usageNumber(usage, "output_tokens")
	total, hasTotal := usageNumber(usage, "total_tokens")
	if hasInput {
		converted["prompt_tokens"] = input
	}
	if hasOutput {
		converted["completion_tokens"] = output
	}
	if hasTotal {
		converted["total_tokens"] = total
	} else if hasInput || hasOutput {
		converted["total_tokens"] = addNumbers(input, output)
	}
	if details, ok := object(usage["input_tokens_details"]); ok {
		if cached, ok := usageNumber(details, "cached_tokens"); ok {
			converted["prompt_tokens_details"] = map[string]any{"cached_tokens": cached}
		}
	}
	if details, ok := object(usage["output_tokens_details"]); ok {
		if reasoning, ok := usageNumber(details, "reasoning_tokens"); ok {
			converted["completion_tokens_details"] = map[string]any{"reasoning_tokens": reasoning}
		}
	}
	if len(converted) == 0 {
		return nil
	}
	return converted
}

// usageNumber reads one token count. Counts arrive as JSON numbers, and a value
// that is absent or not a number is reported as unknown rather than as zero.
func usageNumber(usage map[string]any, name string) (float64, bool) {
	value, ok := usage[name].(float64)
	return value, ok
}

// addNumbers adds two counts that may each be unknown.
func addNumbers(first, second float64) float64 {
	return first + second
}

// decodeObject reads a raw JSON object, reporting false for a value that is
// absent, null, or not an object.
func decodeObject(raw json.RawMessage) (map[string]any, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, false
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, false
	}
	return decoded, decoded != nil
}
