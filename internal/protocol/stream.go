package protocol

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// Stream translation.
//
// A streamed answer is not one document but a sequence of events, and the two
// protocols name and shape those events differently. The translation therefore
// runs event by event as the client reads: each frame the upstream sent is read,
// translated into zero or more frames in the caller's protocol, and handed to the
// client, so the answer arrives as it is produced rather than after the last
// token.
//
// The translators keep the little state an event stream needs 鈥?the answer
// identifier, which items are open, and the token usage stated at the end 鈥?and
// close the answer out themselves when the upstream's stream ends without having
// said how the answer ended.

// maxSSELine bounds one server-sent event line. A tool call's arguments are the
// longest thing a line normally carries; a line beyond this is not one either
// protocol sends, and buffering it would only grow the gateway's memory.
const maxSSELine = 1 << 20

// sseEvent is one server-sent event: its name, and the data lines joined the way
// the event stream specification joins them.
type sseEvent struct {
	name string
	data string
	id   string
}

// readSSEEvent reads the next event from a stream. It reports false at the end
// of the stream.
func readSSEEvent(reader *bufio.Reader) (sseEvent, bool, error) {
	var event sseEvent
	data := make([]string, 0, 2)
	for {
		line, err := readSSELine(reader)
		if line != "" {
			text := strings.TrimRight(line, "\r\n")
			switch {
			case text == "":
				if len(data) > 0 || event.name != "" || event.id != "" {
					event.data = strings.Join(data, "\n")
					return event, true, nil
				}
			case strings.HasPrefix(text, ":"):
				// A comment line is a keep-alive, not an event.
			default:
				field, value, found := strings.Cut(text, ":")
				if !found {
					field, value = text, ""
				}
				value = strings.TrimPrefix(value, " ")
				switch field {
				case "event":
					event.name = value
				case "data":
					data = append(data, value)
				case "id":
					event.id = value
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				if len(data) > 0 || event.name != "" {
					event.data = strings.Join(data, "\n")
					return event, true, nil
				}
				return event, false, io.EOF
			}
			return event, false, err
		}
	}
}

// readSSELine reads one line, bounded by maxSSELine. bufio's own slice is small,
// so a long line arrives in pieces and is assembled here up to the bound.
func readSSELine(reader *bufio.Reader) (string, error) {
	var builder []byte
	for {
		chunk, err := reader.ReadSlice('\n')
		if len(builder)+len(chunk) > maxSSELine {
			return "", errors.New("the upstream sent a server-sent event line that is too long")
		}
		builder = append(builder, chunk...)
		if err == nil {
			return string(builder), nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) {
			if len(builder) > 0 {
				return string(builder), nil
			}
			return "", io.EOF
		}
		return "", err
	}
}

// chatDataFrame renders one event for a chat completion client.
func chatDataFrame(payload any) []byte {
	return []byte("data: " + string(compactJSON(payload)) + "\n\n")
}

// chatRawFrame relays a data line this package did not parse. A chat completion
// stream is a list of data lines and clients tolerate extensions, so the bytes go
// through rather than being dropped.
func chatRawFrame(data string) []byte {
	return []byte("data: " + data + "\n\n")
}

// chatDoneFrame is the end of a chat completion stream.
func chatDoneFrame() []byte {
	return []byte("data: [DONE]\n\n")
}

// responsesEventFrame renders one typed event for a Responses client.
func responsesEventFrame(name string, payload map[string]any) []byte {
	payload["type"] = name
	return []byte("event: " + name + "\ndata: " + string(compactJSON(payload)) + "\n\n")
}

// streamTranslator turns the events of one protocol into the frames of the
// other. handle returns the frames one event produces and whether the answer is
// complete; finish closes an answer whose stream ended without a closing event
// and is safe to call more than once.
type streamTranslator interface {
	handle(event sseEvent) ([]byte, bool)
	finish() []byte
}

// streamConverter is the body of a streamed answer as the client reads it.
type streamConverter struct {
	source     io.ReadCloser
	reader     *bufio.Reader
	translator streamTranslator

	pending []byte
	done    bool
	err     error
}

// newStreamConverter wraps one streamed answer body.
func newStreamConverter(c Conversion, source io.ReadCloser) io.ReadCloser {
	var translator streamTranslator
	switch {
	case c.From == APIResponses && c.To == APIChatCompletions:
		translator = &responsesToChatTranslator{
			tools: make(map[int]int),
			args:  make(map[int]string),
		}
	case c.From == APIChatCompletions && c.To == APIResponses:
		translator = &chatToResponsesTranslator{tools: make(map[int]*chatToolState)}
	default:
		translator = &passthroughTranslator{}
	}
	return &streamConverter{source: source, reader: bufio.NewReaderSize(source, 32*1024), translator: translator}
}

// Read translates events until it has something to return.
func (c *streamConverter) Read(buffer []byte) (int, error) {
	for {
		if len(c.pending) > 0 {
			count := copy(buffer, c.pending)
			c.pending = c.pending[count:]
			return count, nil
		}
		if c.done {
			if c.err != nil && !errors.Is(c.err, io.EOF) {
				return 0, c.err
			}
			return 0, io.EOF
		}
		event, ok, err := readSSEEvent(c.reader)
		if err != nil || !ok {
			// A stream that ends normally may still owe the client its closing
			// event. One that failed is not closed out as if it had completed: the
			// caller is told the answer broke off, which is what it should record.
			if err == nil || errors.Is(err, io.EOF) {
				c.pending = append(c.pending, c.translator.finish()...)
			}
			c.done = true
			if err != nil {
				c.err = err
			} else {
				c.err = io.EOF
			}
			continue
		}
		frames, complete := c.translator.handle(event)
		c.pending = append(c.pending, frames...)
		if complete {
			c.pending = append(c.pending, c.translator.finish()...)
			c.done = true
		}
	}
}

// Close releases the upstream body.
func (c *streamConverter) Close() error {
	return c.source.Close()
}

// passthroughTranslator is used when a conversion has no translator, which
// ConvertResponse never asks for; it keeps the type total and lets the bytes
// through unchanged.
type passthroughTranslator struct{}

func (passthroughTranslator) handle(event sseEvent) ([]byte, bool) {
	return chatRawFrame(event.data), false
}

func (passthroughTranslator) finish() []byte {
	return nil
}

/* ===== Responses API answers -> chat completions ===== */

// responsesToChatTranslator turns Responses events into chat completion chunks.
type responsesToChatTranslator struct {
	id      string
	model   string
	created int64

	roleSent bool
	finished bool
	// tools maps a Responses output index to the index the chat client sees.
	tools    map[int]int
	hasTools bool
	// args and text keep what has been relayed, so an upstream that states a
	// whole value only in its closing event still reaches the chat client.
	args         map[int]string
	text         strings.Builder
	usage        map[string]any
	finishReason string
}

func (t *responsesToChatTranslator) handle(event sseEvent) ([]byte, bool) {
	if strings.TrimSpace(event.data) == "[DONE]" {
		return t.complete(), true
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(event.data), &payload); err != nil || payload == nil {
		if event.name == "" && strings.TrimSpace(event.data) != "" {
			return chatRawFrame(event.data), false
		}
		return nil, false
	}

	eventType := event.name
	if eventType == "" {
		eventType = stringField(payload, "type")
	}
	switch eventType {
	case "response.created":
		if response, ok := object(payload["response"]); ok {
			t.capture(response)
		}
		return t.ensureRole(), false
	case "response.in_progress":
		return nil, false
	case "response.output_text.delta":
		if delta := stringField(payload, "delta"); delta != "" {
			t.text.WriteString(delta)
			out := t.ensureRole()
			out = append(out, chatDataFrame(t.chunk(map[string]any{"content": delta}, nil, nil))...)
			return out, false
		}
		return nil, false
	case "response.output_text.done":
		// A provider may state the whole text only here, without having sent it as
		// deltas. What the client has not seen yet is relayed as one delta.
		text := stringField(payload, "text")
		sent := t.text.String()
		remaining := ""
		switch {
		case text == "" || text == sent:
			return nil, false
		case strings.HasPrefix(text, sent):
			remaining = text[len(sent):]
		case sent == "":
			remaining = text
		}
		if remaining == "" {
			return nil, false
		}
		t.text.WriteString(remaining)
		out := t.ensureRole()
		out = append(out, chatDataFrame(t.chunk(map[string]any{"content": remaining}, nil, nil))...)
		return out, false
	case "response.output_item.added":
		item, ok := object(payload["item"])
		if !ok || stringField(item, "type") != "function_call" {
			return nil, false
		}
		outputIndex := intField(payload, "output_index")
		if _, seen := t.tools[outputIndex]; seen {
			return nil, false
		}
		chatIndex := len(t.tools)
		t.tools[outputIndex] = chatIndex
		t.hasTools = true
		callID := stringField(item, "call_id")
		if callID == "" {
			callID = stringField(item, "id")
		}
		if callID == "" {
			callID = fmt.Sprintf("call_%d", chatIndex)
		}
		// Some providers state the arguments here rather than as deltas.
		initialArgs := stringField(item, "arguments")
		t.args[outputIndex] = initialArgs
		call := map[string]any{
			"index": chatIndex,
			"id":    callID,
			"type":  "function",
			"function": map[string]any{
				"name":      stringField(item, "name"),
				"arguments": initialArgs,
			},
		}
		out := t.ensureRole()
		out = append(out, chatDataFrame(t.chunk(map[string]any{"tool_calls": []any{call}}, nil, nil))...)
		return out, false
	case "response.function_call_arguments.delta":
		delta := stringField(payload, "delta")
		if delta == "" {
			return nil, false
		}
		outputIndex := intField(payload, "output_index")
		chatIndex, ok := t.tools[outputIndex]
		if !ok {
			return nil, false
		}
		t.args[outputIndex] += delta
		call := map[string]any{
			"index":    chatIndex,
			"function": map[string]any{"arguments": delta},
		}
		out := t.ensureRole()
		out = append(out, chatDataFrame(t.chunk(map[string]any{"tool_calls": []any{call}}, nil, nil))...)
		return out, false
	case "response.output_item.done":
		// The closing item may carry arguments the deltas did not, which is the
		// only statement of them some providers make.
		item, ok := object(payload["item"])
		if !ok || stringField(item, "type") != "function_call" {
			return nil, false
		}
		outputIndex := intField(payload, "output_index")
		chatIndex, ok := t.tools[outputIndex]
		if !ok {
			return nil, false
		}
		full := stringField(item, "arguments")
		sent := t.args[outputIndex]
		remaining := ""
		switch {
		case full == "" || full == sent:
			return nil, false
		case strings.HasPrefix(full, sent):
			remaining = full[len(sent):]
		case sent == "":
			remaining = full
		}
		if remaining == "" {
			return nil, false
		}
		t.args[outputIndex] = full
		call := map[string]any{
			"index":    chatIndex,
			"function": map[string]any{"arguments": remaining},
		}
		out := t.ensureRole()
		out = append(out, chatDataFrame(t.chunk(map[string]any{"tool_calls": []any{call}}, nil, nil))...)
		return out, false
	case "response.completed", "response.incomplete":
		response, _ := object(payload["response"])
		t.capture(response)
		if usage, ok := object(response["usage"]); ok {
			t.usage = responsesUsageMapToChat(usage)
		}
		t.finishReason = responsesEventFinish(response, t.hasTools)
		return t.complete(), true
	case "response.failed":
		response, _ := object(payload["response"])
		message := eventErrorMessage(response)
		return t.fail(message), true
	case "error":
		return t.fail(eventErrorMessage(payload)), true
	default:
		return nil, false
	}
}

// capture reads the answer identity out of a response object.
func (t *responsesToChatTranslator) capture(response map[string]any) {
	if response == nil {
		return
	}
	if id := stringField(response, "id"); id != "" {
		t.id = id
	}
	if model := stringField(response, "model"); model != "" {
		t.model = model
	}
	if created := int64(intField(response, "created_at")); created > 0 {
		t.created = created
	}
}

// ensureRole emits the first chunk, which tells the client who is answering.
func (t *responsesToChatTranslator) ensureRole() []byte {
	if t.roleSent {
		return nil
	}
	t.roleSent = true
	return chatDataFrame(t.chunk(map[string]any{"role": "assistant"}, nil, nil))
}

// chunk builds one chat completion chunk.
func (t *responsesToChatTranslator) chunk(delta any, finish any, usage map[string]any) map[string]any {
	if t.created == 0 {
		t.created = time.Now().Unix()
	}
	if t.id == "" {
		// A stream that began mid-answer still needs an identifier the client can
		// group its chunks under.
		t.id = "chatcmpl-fluxgate"
	}
	payload := map[string]any{
		"id":      t.id,
		"object":  "chat.completion.chunk",
		"created": t.created,
		"model":   t.model,
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         delta,
			"finish_reason": finish,
		}},
	}
	if usage != nil {
		payload["usage"] = usage
	}
	return payload
}

// complete closes the answer with its finish reason and the usage it stated.
func (t *responsesToChatTranslator) complete() []byte {
	if t.finished {
		return nil
	}
	t.finished = true
	finish := t.finishReason
	if finish == "" {
		if t.hasTools {
			finish = "tool_calls"
		} else {
			finish = "stop"
		}
	}
	out := t.ensureRole()
	out = append(out, chatDataFrame(t.chunk(map[string]any{}, finish, t.usage))...)
	out = append(out, chatDoneFrame()...)
	return out
}

// fail ends the stream with the upstream's reason, in the shape a chat client
// understands.
func (t *responsesToChatTranslator) fail(message string) []byte {
	if t.finished {
		return nil
	}
	t.finished = true
	if strings.TrimSpace(message) == "" {
		message = "the upstream failed the request"
	}
	out := t.ensureRole()
	out = append(out, chatDataFrame(map[string]any{"error": map[string]any{
		"message": message,
		"type":    "upstream_error",
	}})...)
	out = append(out, chatDoneFrame()...)
	return out
}

// finish closes a stream that ended without a terminal event.
func (t *responsesToChatTranslator) finish() []byte {
	return t.complete()
}

// responsesEventFinish names why a Responses answer ended in the chat
// vocabulary.
func responsesEventFinish(response map[string]any, tools bool) string {
	if tools {
		return "tool_calls"
	}
	if strings.EqualFold(stringField(response, "status"), "incomplete") {
		if details, ok := object(response["incomplete_details"]); ok {
			switch stringField(details, "reason") {
			case "max_output_tokens":
				return "length"
			case "content_filter":
				return "content_filter"
			}
		}
	}
	return "stop"
}

/* ===== chat completions -> Responses API answers ===== */

// chatToolState is one function call as a chat stream disclosed it.
type chatToolState struct {
	itemID      string
	callID      string
	name        string
	arguments   strings.Builder
	outputIndex int
}

// chatToResponsesTranslator turns chat completion chunks into Responses events.
type chatToResponsesTranslator struct {
	id      string
	model   string
	created int64
	started bool

	sequence int
	finished bool

	textID    string
	text      strings.Builder
	textOpen  bool
	textIndex int

	tools     map[int]*chatToolState
	toolOrder []int
	nextIndex int

	finishReason string
	usage        map[string]any
}

func (t *chatToResponsesTranslator) handle(event sseEvent) ([]byte, bool) {
	if event.name == "" && strings.TrimSpace(event.data) == "[DONE]" {
		return t.complete(), true
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(event.data), &payload); err != nil || payload == nil {
		return nil, false
	}
	if _, failed := payload["error"]; failed && payload["choices"] == nil {
		return t.fail(eventErrorMessage(payload)), true
	}

	var out []byte
	if !t.started {
		out = append(out, t.start(payload)...)
	}
	if usage, ok := object(payload["usage"]); ok {
		t.usage = usage
	}
	choices, _ := array(payload["choices"])
	for _, rawChoice := range choices {
		choice, ok := object(rawChoice)
		if !ok {
			continue
		}
		if delta, ok := object(choice["delta"]); ok {
			if content, ok := delta["content"]; ok {
				if text := anyText(content); text != "" {
					out = append(out, t.appendText(text)...)
				}
			}
			toolCalls, _ := array(delta["tool_calls"])
			for _, rawCall := range toolCalls {
				call, ok := object(rawCall)
				if !ok {
					continue
				}
				out = append(out, t.appendToolCall(call)...)
			}
		}
		if reason, ok := choice["finish_reason"].(string); ok && reason != "" {
			t.finishReason = reason
		}
	}
	return out, false
}

// start emits the opening events that tell a Responses client which answer it is
// reading.
func (t *chatToResponsesTranslator) start(chunk map[string]any) []byte {
	t.started = true
	if t.created == 0 {
		t.created = time.Now().Unix()
	}
	if created := int64(intField(chunk, "created")); created > 0 {
		t.created = created
	}
	t.model = stringField(chunk, "model")
	t.id = responsesID(stringField(chunk, "id"), t.created)
	response := t.responseObject("in_progress", nil, nil)
	var out []byte
	out = append(out, t.frame("response.created", map[string]any{"response": response})...)
	out = append(out, t.frame("response.in_progress", map[string]any{"response": response})...)
	return out
}

// appendText opens the assistant message on its first token and emits each
// token as it arrives.
func (t *chatToResponsesTranslator) appendText(delta string) []byte {
	var out []byte
	if !t.textOpen {
		out = append(out, t.openText()...)
	}
	if delta == "" {
		return out
	}
	t.text.WriteString(delta)
	out = append(out, t.frame("response.output_text.delta", map[string]any{
		"item_id":       t.textID,
		"output_index":  t.textIndex,
		"content_index": 0,
		"delta":         delta,
	})...)
	return out
}

// openText emits the item and content-part events that must precede the first
// text delta.
func (t *chatToResponsesTranslator) openText() []byte {
	t.textOpen = true
	t.textIndex = t.nextIndex
	t.nextIndex++
	t.textID = "msg_" + shortID(t.id)
	var out []byte
	out = append(out, t.frame("response.output_item.added", map[string]any{
		"output_index": t.textIndex,
		"item": map[string]any{
			"id":      t.textID,
			"type":    "message",
			"status":  "in_progress",
			"role":    "assistant",
			"content": []any{},
		},
	})...)
	out = append(out, t.frame("response.content_part.added", map[string]any{
		"item_id":       t.textID,
		"output_index":  t.textIndex,
		"content_index": 0,
		"part": map[string]any{
			"type":        "output_text",
			"text":        "",
			"annotations": []any{},
		},
	})...)
	return out
}

// appendToolCall opens a function call on its first chunk and emits each
// argument fragment after it.
func (t *chatToResponsesTranslator) appendToolCall(call map[string]any) []byte {
	index := intField(call, "index")
	state, opened := t.tools[index]
	var out []byte
	if !opened {
		state = &chatToolState{
			outputIndex: t.nextIndex,
			callID:      stringField(call, "id"),
		}
		t.nextIndex++
		if function, ok := object(call["function"]); ok {
			state.name = stringField(function, "name")
		}
		if state.callID == "" {
			state.callID = fmt.Sprintf("call_%s_%d", shortID(t.id), index)
		}
		state.itemID = fmt.Sprintf("fc_%s_%d", shortID(t.id), index)
		t.tools[index] = state
		t.toolOrder = append(t.toolOrder, index)
		out = append(out, t.frame("response.output_item.added", map[string]any{
			"output_index": state.outputIndex,
			"item": map[string]any{
				"id":        state.itemID,
				"type":      "function_call",
				"status":    "in_progress",
				"call_id":   state.callID,
				"name":      state.name,
				"arguments": "",
			},
		})...)
	}
	function, _ := object(call["function"])
	if fragment := stringField(function, "arguments"); fragment != "" {
		state.arguments.WriteString(fragment)
		out = append(out, t.frame("response.function_call_arguments.delta", map[string]any{
			"item_id":      state.itemID,
			"output_index": state.outputIndex,
			"delta":        fragment,
		})...)
	}
	return out
}

// complete closes every open item and states how the answer ended, which is the
// event a Responses client waits for.
func (t *chatToResponsesTranslator) complete() []byte {
	if t.finished {
		return nil
	}
	t.finished = true
	var out []byte
	if !t.started {
		out = append(out, t.start(nil)...)
	}

	if !t.textOpen && len(t.tools) == 0 {
		// An answer with neither text nor a tool call still names one empty
		// assistant message, which is what a chat completion with empty content
		// means.
		out = append(out, t.openText()...)
	}
	items := make([]any, t.nextIndex)
	if t.textOpen {
		text := t.text.String()
		part := map[string]any{
			"type":        "output_text",
			"text":        text,
			"annotations": []any{},
		}
		out = append(out, t.frame("response.output_text.done", map[string]any{
			"item_id":       t.textID,
			"output_index":  t.textIndex,
			"content_index": 0,
			"text":          text,
		})...)
		out = append(out, t.frame("response.content_part.done", map[string]any{
			"item_id":       t.textID,
			"output_index":  t.textIndex,
			"content_index": 0,
			"part":          part,
		})...)
		item := map[string]any{
			"id":      t.textID,
			"type":    "message",
			"status":  "completed",
			"role":    "assistant",
			"content": []any{part},
		}
		out = append(out, t.frame("response.output_item.done", map[string]any{
			"output_index": t.textIndex,
			"item":         item,
		})...)
		items[t.textIndex] = item
	}
	for _, index := range t.toolOrder {
		state := t.tools[index]
		arguments := state.arguments.String()
		item := map[string]any{
			"id":        state.itemID,
			"type":      "function_call",
			"status":    "completed",
			"call_id":   state.callID,
			"name":      state.name,
			"arguments": arguments,
		}
		out = append(out, t.frame("response.function_call_arguments.done", map[string]any{
			"item_id":      state.itemID,
			"output_index": state.outputIndex,
			"arguments":    arguments,
		})...)
		out = append(out, t.frame("response.output_item.done", map[string]any{
			"output_index": state.outputIndex,
			"item":         item,
		})...)
		items[state.outputIndex] = item
	}

	compact := make([]any, 0, len(items))
	for _, item := range items {
		if item != nil {
			compact = append(compact, item)
		}
	}
	status := "completed"
	var incomplete any
	switch t.finishReason {
	case "length":
		status = "incomplete"
		incomplete = map[string]any{"reason": "max_output_tokens"}
	case "content_filter":
		status = "incomplete"
		incomplete = map[string]any{"reason": "content_filter"}
	}
	usage := chatUsageMapToResponses(t.usage)
	response := t.responseObject(status, compact, usage)
	if incomplete != nil {
		response["incomplete_details"] = incomplete
	}
	out = append(out, t.frame("response.completed", map[string]any{"response": response})...)
	return out
}

// fail ends the stream with an error event, which is how a Responses client is
// told that the answer it was reading will not continue.
func (t *chatToResponsesTranslator) fail(message string) []byte {
	if t.finished {
		return nil
	}
	t.finished = true
	var out []byte
	if !t.started {
		out = append(out, t.start(nil)...)
	}
	if strings.TrimSpace(message) == "" {
		message = "the upstream failed the request"
	}
	response := t.responseObject("failed", []any{}, nil)
	response["error"] = map[string]any{"code": "upstream_error", "message": message}
	out = append(out, t.frame("response.failed", map[string]any{"response": response})...)
	return out
}

// responseObject builds the response document the opening and closing events
// carry.
func (t *chatToResponsesTranslator) responseObject(status string, output []any, usage map[string]any) map[string]any {
	if output == nil {
		output = []any{}
	}
	response := map[string]any{
		"id":         t.id,
		"object":     "response",
		"created_at": t.created,
		"status":     status,
		"model":      t.model,
		"output":     output,
	}
	if text := t.text.String(); text != "" {
		response["output_text"] = text
	}
	if usage != nil {
		response["usage"] = usage
	}
	return response
}

// frame adds the event's sequence number, which a Responses stream numbers in
// the order it emits events.
func (t *chatToResponsesTranslator) frame(name string, payload map[string]any) []byte {
	payload["sequence_number"] = t.sequence
	t.sequence++
	return responsesEventFrame(name, payload)
}

// finish closes a stream that ended without a terminal event.
func (t *chatToResponsesTranslator) finish() []byte {
	return t.complete()
}

/* ===== Shared ===== */

// intField reads an integer field, whether it arrived as a JSON number or as a
// string of digits.
func intField(document map[string]any, name string) int {
	switch value := document[name].(type) {
	case float64:
		return int(value)
	case json.Number:
		parsed, _ := strconv.Atoi(value.String())
		return parsed
	case string:
		parsed, _ := strconv.Atoi(strings.TrimSpace(value))
		return parsed
	}
	return 0
}

// eventErrorMessage reads the message an error event or a failed response
// carries, in the shapes providers use.
func eventErrorMessage(document map[string]any) string {
	if document == nil {
		return ""
	}
	if message := stringField(document, "message"); message != "" {
		return message
	}
	switch nested := document["error"].(type) {
	case string:
		return strings.TrimSpace(nested)
	case map[string]any:
		if message := stringField(nested, "message"); message != "" {
			return message
		}
		return stringField(nested, "code")
	}
	return ""
}

// usageRaw re-encodes a decoded usage object so one of the usage converters can
// read it.
func usageRaw(value map[string]any) json.RawMessage {
	if value == nil {
		return nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	return encoded
}

// chatUsageMapToResponses states a decoded chat usage object in the Responses
// vocabulary.
func chatUsageMapToResponses(usage map[string]any) map[string]any {
	if usage == nil {
		return nil
	}
	return chatUsageToResponses(usageRaw(usage))
}

// responsesUsageMapToChat states a decoded Responses usage object in the chat
// vocabulary.
func responsesUsageMapToChat(usage map[string]any) map[string]any {
	if usage == nil {
		return nil
	}
	return responsesUsageToChat(usageRaw(usage))
}
