// Package protocol translates between the two OpenAI-compatible conversational
// protocols the gateway serves: chat completions and the Responses API.
//
// A client reaches the gateway through one of them and an upstream may speak the
// other. The gateway is the one party that knows both ends, so it translates the
// request on the way out and the answer on the way back, including a streamed
// answer event by event. The client keeps the protocol it asked for and never
// learns which one its upstream chose; the operator states what a line speaks
// once, on the line.
//
// Only these two protocols are translated. Embeddings, images, videos, and the
// Anthropic messages route are forwarded as they arrived, because neither has a
// counterpart here: a line's protocol setting is a statement about the
// conversational endpoints and nothing else.
package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// The protocol identifiers a line may be configured with, as they are stored in
// the configuration database.
const (
	// APIChatCompletions is the chat-completions protocol, the one most clients
	// and most OpenAI-compatible upstreams speak.
	APIChatCompletions = "chat_completions"
	// APIResponses is the Responses API protocol, which some agents speak
	// natively and some upstreams serve exclusively.
	APIResponses = "responses"
)

// The paths the two protocols live under on the gateway and on an upstream.
const (
	PathChatCompletions = "/v1/chat/completions"
	PathResponses       = "/v1/responses"
)

// ErrRequestConversion marks a request the gateway could not express in the
// protocol the selected line speaks. It is a property of the request and the
// line together, not of the upstream — no attempt is made — so the caller can
// answer it as a request error rather than as a bad gateway.
var ErrRequestConversion = errors.New("the request cannot be converted to the protocol the upstream speaks")

// maxBufferedAnswer bounds how much of an ordinary answer is held while it is
// translated. A conversational answer is a small document; an answer larger than
// this — an inline image, say — is forwarded untranslated rather than buffered,
// because holding it would cost the gateway its memory for a body it mostly
// cannot improve.
const maxBufferedAnswer = 32 << 20

// NormalizeAPI reads the protocol an operator or another writer stored on a
// line. The stored values are the constants above; the synonyms are accepted so
// a value written by hand or by automation still means what it says. An unknown
// value reads as no choice at all, which leaves the line on the callers'
// protocol, the behavior every line had before this setting existed.
func NormalizeAPI(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case APIChatCompletions, "chat", "chatcompletions", "chat-completions", PathChatCompletions:
		return APIChatCompletions
	case APIResponses, "response", PathResponses:
		return APIResponses
	}
	return ""
}

// APIForPath names the protocol one of the gateway's conversational routes
// speaks, or an empty string for a route this package does not translate.
func APIForPath(path string) string {
	switch strings.TrimSuffix(strings.TrimSpace(path), "/") {
	case PathChatCompletions, "/chat/completions":
		return APIChatCompletions
	case PathResponses, "/responses":
		return APIResponses
	}
	return ""
}

// PathForAPI is the path an upstream speaking the protocol is called at.
func PathForAPI(api string) string {
	switch NormalizeAPI(api) {
	case APIChatCompletions:
		return PathChatCompletions
	case APIResponses:
		return PathResponses
	}
	return ""
}

// Conversion is the translation one attempt needs: the protocol the caller
// speaks, and the protocol the selected line speaks. Both empty means the
// request travels as it arrived.
type Conversion struct {
	From string
	To   string
}

// Resolve decides whether one attempt needs a translation. A line that names no
// protocol, a route that is not conversational, and a line that already speaks
// the caller's protocol all need none.
func Resolve(path, upstreamAPI string) Conversion {
	from := APIForPath(path)
	to := NormalizeAPI(upstreamAPI)
	if from == "" || to == "" || from == to {
		return Conversion{}
	}
	return Conversion{From: from, To: to}
}

// Active reports whether a translation is needed.
func (c Conversion) Active() bool {
	return c.From != "" && c.To != ""
}

// UpstreamPath is the path the attempt is sent to: the caller's own path while
// no translation is needed, and the line's protocol path when it is.
func (c Conversion) UpstreamPath(fallback string) string {
	if !c.Active() {
		return fallback
	}
	return PathForAPI(c.To)
}

// ConvertRequest rewrites a request body from the caller's protocol into the
// line's. The body must be a JSON object, which is the only shape either
// conversational endpoint accepts. The model field, when the request carries
// one, travels through unchanged: renaming a model is the route's business and
// has already happened by the time this is called.
func ConvertRequest(c Conversion, body []byte) ([]byte, error) {
	if !c.Active() {
		return body, nil
	}
	var document map[string]any
	if err := json.Unmarshal(body, &document); err != nil || document == nil {
		return nil, fmt.Errorf("%w: the request body is not a JSON object", ErrRequestConversion)
	}

	var converted map[string]any
	var err error
	switch {
	case c.From == APIChatCompletions && c.To == APIResponses:
		converted, err = chatRequestToResponses(document)
	case c.From == APIResponses && c.To == APIChatCompletions:
		converted, err = responsesRequestToChat(document)
	default:
		err = fmt.Errorf("conversion from %s to %s is not supported", c.From, c.To)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrRequestConversion, err.Error())
	}
	encoded, err := json.Marshal(converted)
	if err != nil {
		return nil, fmt.Errorf("%w: re-encode the converted request: %s", ErrRequestConversion, err.Error())
	}
	return encoded, nil
}

// ConvertResponse rewrites an upstream answer into the caller's protocol. It is
// called once an attempt has succeeded; an error status is passed through
// untouched, because both protocols answer a failure with the same error object
// and the upstream's own words are worth more than a translation of them.
//
// A streamed answer is translated as it is read, so the client receives each
// event at the moment the upstream sent the one it came from. An ordinary answer
// is read, translated, and handed back as a new body. A body this package cannot
// translate — a protocol it does not know, a document that is not the shape the
// protocol promises, an answer too large to hold — is forwarded exactly as it
// arrived rather than replaced by an error: the upstream did answer, and passing
// its bytes through is never worse than failing a request that succeeded.
func ConvertResponse(c Conversion, response *http.Response) *http.Response {
	if !c.Active() || response == nil || response.Body == nil {
		return response
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return response
	}
	if response.Header == nil {
		response.Header = make(http.Header)
	}
	// The request travelled caller -> line, so its answer travels line -> caller.
	answer := Conversion{From: c.To, To: c.From}
	if isEventStream(response.Header.Get("Content-Type")) {
		response.Body = newStreamConverter(answer, response.Body)
		response.ContentLength = -1
		response.Header.Del("Content-Length")
		response.Header.Set("Content-Type", "text/event-stream")
		return response
	}

	original := response.Body
	prefix := make([]byte, maxBufferedAnswer+1)
	count, readErr := io.ReadFull(original, prefix)
	prefix = prefix[:count]
	// replay puts the bytes read so far back in front of the body, so an answer
	// that is not translated still reaches the caller whole.
	replay := func() *http.Response {
		response.Body = &replayBody{Reader: io.MultiReader(bytes.NewReader(prefix), original), Closer: original}
		return response
	}
	if count == 0 {
		return replay()
	}
	if count > maxBufferedAnswer {
		return replay()
	}
	if readErr != nil && !errors.Is(readErr, io.EOF) && !errors.Is(readErr, io.ErrUnexpectedEOF) {
		return replay()
	}

	converted, err := convertAnswer(answer, prefix)
	if err != nil {
		return replay()
	}
	_ = original.Close()
	response.Body = io.NopCloser(bytes.NewReader(converted))
	response.ContentLength = int64(len(converted))
	response.Header.Set("Content-Length", fmt.Sprintf("%d", len(converted)))
	response.Header.Set("Content-Type", "application/json")
	return response
}

// convertAnswer translates one complete conversational answer.
func convertAnswer(c Conversion, raw []byte) ([]byte, error) {
	// An upstream that reports a failure under a success status is not an answer
	// to translate: both protocols already carry the same error object, so the
	// bytes are left alone.
	var envelope map[string]any
	if err := json.Unmarshal(raw, &envelope); err == nil && envelope != nil {
		if _, failed := envelope["error"]; failed {
			return nil, errors.New("the answer describes an upstream error")
		}
	}
	switch {
	case c.From == APIResponses && c.To == APIChatCompletions:
		return responsesAnswerToChat(raw)
	case c.From == APIChatCompletions && c.To == APIResponses:
		return chatAnswerToResponses(raw)
	}
	return nil, fmt.Errorf("conversion from %s to %s is not supported", c.From, c.To)
}

// isEventStream reports whether a media type describes a server-sent event
// stream.
func isEventStream(contentType string) bool {
	return strings.Contains(strings.ToLower(strings.TrimSpace(contentType)), "text/event-stream")
}

// replayBody is a body whose beginning was read ahead and put back: it yields
// the bytes already read, then whatever the upstream still has, and closes the
// upstream body with itself.
type replayBody struct {
	io.Reader
	io.Closer
}
