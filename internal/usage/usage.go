// Package usage reads the token accounting an upstream reports and describes the
// records the gateway keeps of it.
//
// A gateway is the one party that sees every request and every answer, so it is
// the right place to count what a model actually consumed: the prompt it was
// sent and the completion it produced. Both are the costs an operator is billed
// for, and neither is visible from the outside once a request has been served.
//
// The upstream states the counts in its own answer, in one of two shapes: a JSON
// body whose top-level "usage" object carries them, or — for a streamed answer —
// a final server-sent event that carries the same object. The tap below follows
// an answer as it is relayed to the client, so counting costs the client nothing:
// no buffering of the whole answer, no second pass, no delay.
package usage

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"time"
)

// Event is one served request as the usage log keeps it. It names the client key
// and the line by their labels rather than by a credential, so a usage record
// never carries a secret.
type Event struct {
	// RequestID is the identity the client was told, which is what ties a usage
	// record to the request record that explains it.
	RequestID string
	// At is when the request started.
	At time.Time
	// Path is the endpoint that was called.
	Path string
	// Model is the model the client asked for.
	Model string
	// ClientIP is the address the request came from.
	ClientIP string
	// KeyID and KeyName identify the client key. The credential itself is never
	// part of a usage record.
	KeyID   int64
	KeyName string
	// ChannelID and ChannelName identify the line that answered, which is how
	// usage is attributed to an upstream.
	ChannelID   string
	ChannelName string
	// Status is the status the client was answered with.
	Status int
	// Stream marks a request whose answer the client read as it arrived.
	Stream bool
	// DurationMS is how long the request took end to end.
	DurationMS int64
	// PromptTokens and CompletionTokens are what the upstream reported. Zero is
	// an upstream that answered without stating them, which is not an error: the
	// request is still counted.
	PromptTokens     int64
	CompletionTokens int64
	// Reported marks a request whose token counts actually came from the
	// upstream. A request counted without them is not evidence of zero cost, so a
	// view can say how much of the total is measured rather than assumed.
	Reported bool
}

// TotalTokens is the sum an operator is billed for.
func (e Event) TotalTokens() int64 {
	return e.PromptTokens + e.CompletionTokens
}

// Failed reports whether the request was not served.
func (e Event) Failed() bool {
	return e.Status == 0 || e.Status >= 400
}

// Breakdown names one grouping of usage — a model, an upstream, or a client key —
// together with what was counted under it.
type Breakdown struct {
	Name             string `json:"name"`
	Requests         int64  `json:"requests"`
	Succeeded        int64  `json:"succeeded"`
	Failed           int64  `json:"failed"`
	PromptTokens     int64  `json:"prompt_tokens"`
	CompletionTokens int64  `json:"completion_tokens"`
	TotalTokens      int64  `json:"total_tokens"`
	Reported         int64  `json:"reported"`
}

// Retention describes how far back the log reaches and how much it holds, so a
// view can say what its numbers cover instead of implying they are complete.
type Retention struct {
	// Enabled is false when the gateway keeps no usage records at all.
	Enabled bool `json:"enabled"`
	// Keep is the number of records the log holds at most.
	Keep int64 `json:"keep"`
	// Count is how many records it holds now.
	Count int64 `json:"count"`
	// Oldest and Newest bound the records it holds, in UTC. They are zero when the
	// log is empty.
	Oldest time.Time `json:"oldest,omitempty"`
	Newest time.Time `json:"newest,omitempty"`
}

// Summary is the answer to one usage question: the totals over a window, the
// same numbers grouped by model, upstream, and client key, and what the log can
// actually vouch for.
type Summary struct {
	// Window is the name of the window that was asked for.
	Window string `json:"window"`
	// Since and Until bound the window; Until is the moment the answer was built.
	Since time.Time `json:"since"`
	Until time.Time `json:"until"`
	// Requests counts every recorded request in the window, including the ones
	// that failed; Succeeded and Failed split it.
	Requests         int64 `json:"requests"`
	Succeeded        int64 `json:"succeeded"`
	Failed           int64 `json:"failed"`
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
	// Reported counts the requests whose token counts came from the upstream.
	// The difference from Requests is the part of the totals that is unknown
	// rather than zero.
	Reported  int64       `json:"reported"`
	Models    []Breakdown `json:"models"`
	Channels  []Breakdown `json:"channels"`
	Keys      []Breakdown `json:"keys"`
	Retention Retention   `json:"retention"`
}

// Filter narrows a usage question. An empty filter asks for everything the log
// holds.
type Filter struct {
	Since time.Time
	Until time.Time
	// Model keeps one model's requests, compared without case.
	Model string
	// KeyID keeps one client key's requests. Zero means every key.
	KeyID int64
	// Channel keeps one upstream's requests, matched against the line's name or
	// its id.
	Channel string
	// Limit bounds each breakdown list. Zero means the store's own default.
	Limit int
}

// Window names the windows a view offers. They are time spans rather than
// calendar periods, except for today, which is the operator's own calendar day.
const (
	WindowToday = "today"
	Window24h   = "24h"
	Window7d    = "7d"
	Window30d   = "30d"
	WindowAll   = "all"
)

// ResolveWindow turns a window name into the span it describes, ending at now.
// An unknown name falls back to the last seven days, which is the span a view
// shows before an operator picks one.
func ResolveWindow(name string, now time.Time) (string, time.Time, time.Time) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case WindowToday:
		// Midnight in the operator's own zone: "today" is a question about a local
		// calendar day, and the console shows times in the local zone too.
		local := now.Local()
		year, month, day := local.Date()
		since := time.Date(year, month, day, 0, 0, 0, 0, local.Location())
		return WindowToday, since.UTC(), now.UTC()
	case Window24h:
		return Window24h, now.Add(-24 * time.Hour).UTC(), now.UTC()
	case Window30d:
		return Window30d, now.Add(-30 * 24 * time.Hour).UTC(), now.UTC()
	case WindowAll:
		return WindowAll, time.Time{}, now.UTC()
	default:
		return Window7d, now.Add(-7 * 24 * time.Hour).UTC(), now.UTC()
	}
}

// maxBodyScan bounds how much of an answer read as one body is held for parsing.
// An answer that carries a usage object carries it near the end of a document
// that is normally far smaller than this; an answer larger than the bound — an
// inlined image, say — is not buffered at all, and its usage is reported as
// unknown rather than as zero.
const maxBodyScan = 2 << 20

// maxStreamTail bounds the copy of a streamed answer kept for the case where the
// upstream answered a streaming request with an ordinary JSON body.
const maxStreamTail = 64 << 10

// maxLineLength bounds one server-sent event line. A line longer than this is
// discarded rather than buffered, so a misbehaving upstream cannot grow the
// gateway's memory through the tap.
const maxLineLength = 1 << 20

// Tap follows an answer on its way to the client and records the token usage the
// upstream stated. It reads through to the underlying body unchanged: every byte
// the client receives is the byte the upstream sent.
type Tap struct {
	source io.ReadCloser
	stream bool

	mu         sync.Mutex
	body       bytes.Buffer
	overflow   bool
	pending    []byte
	tail       []byte
	prompt     int64
	completion int64
	found      bool
}

// NewTap wraps the body of an upstream answer. A streamed answer is scanned event
// by event as it passes; an answer read as one body is parsed once it is whole.
func NewTap(source io.ReadCloser, stream bool) *Tap {
	return &Tap{source: source, stream: stream}
}

// Read relays one chunk and observes it. The bytes returned are exactly the bytes
// the upstream sent.
func (t *Tap) Read(buffer []byte) (int, error) {
	count, err := t.source.Read(buffer)
	if count > 0 {
		t.observe(buffer[:count])
	}
	return count, err
}

// Close closes the upstream body.
func (t *Tap) Close() error {
	return t.source.Close()
}

// Usage returns what the upstream stated, and whether it stated anything at all.
// It is read after the answer has been relayed.
func (t *Tap) Usage() (int64, int64, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.found {
		return t.prompt, t.completion, true
	}
	// A streaming request answered with an ordinary body — an upstream error, most
	// often — still carries a usage object when it carries anything at all.
	if t.stream {
		if len(t.tail) > 0 {
			if prompt, completion, ok := ParseBody(t.tail); ok {
				t.prompt, t.completion, t.found = prompt, completion, true
				return t.prompt, t.completion, true
			}
		}
		return 0, 0, false
	}
	// An answer read as one body is parsed once it is complete: the usage object
	// is part of a JSON document, which cannot be read before its last byte.
	if t.overflow || t.body.Len() == 0 {
		return 0, 0, false
	}
	if prompt, completion, ok := ParseBody(t.body.Bytes()); ok {
		t.prompt, t.completion, t.found = prompt, completion, true
		return t.prompt, t.completion, true
	}
	return 0, 0, false
}

// observe records one chunk of the answer.
func (t *Tap) observe(chunk []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.stream {
		t.observeStream(chunk)
		return
	}
	if t.overflow {
		return
	}
	if t.body.Len()+len(chunk) > maxBodyScan {
		// The answer is too large to keep; its usage stays unknown rather than
		// being guessed at.
		t.overflow = true
		t.body.Reset()
		return
	}
	_, _ = t.body.Write(chunk)
}

// observeStream scans a streamed answer line by line, keeping the last usage the
// upstream stated. A server-sent event is one or more "data:" lines; the final
// chunk of a streamed completion is where OpenAI puts its usage, and some
// providers repeat it in every chunk, in which case the last one wins.
func (t *Tap) observeStream(chunk []byte) {
	// A tail is kept as well, because an upstream may answer a streaming request
	// with one JSON body instead of events.
	if len(chunk) >= maxStreamTail {
		t.tail = append(t.tail[:0], chunk[len(chunk)-maxStreamTail:]...)
	} else {
		if len(t.tail)+len(chunk) > maxStreamTail {
			t.tail = t.tail[len(t.tail)+len(chunk)-maxStreamTail:]
		}
		t.tail = append(t.tail, chunk...)
	}

	t.pending = append(t.pending, chunk...)
	for {
		index := bytes.IndexByte(t.pending, '\n')
		if index < 0 {
			break
		}
		line := t.pending[:index]
		t.pending = t.pending[index+1:]
		t.observeLine(line)
	}
	if len(t.pending) > maxLineLength {
		// No newline in sight over a whole line's worth of bytes: this is not an
		// event stream, and keeping it would only grow the buffer.
		t.pending = nil
	}
}

// observeLine reads one server-sent event line for a usage object.
func (t *Tap) observeLine(line []byte) {
	trimmed := bytes.TrimSpace(line)
	if !bytes.HasPrefix(trimmed, []byte("data:")) {
		return
	}
	payload := bytes.TrimSpace(trimmed[len("data:"):])
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		return
	}
	if !bytes.Contains(payload, []byte("usage")) {
		return
	}
	prompt, completion, ok := parseUsageObject(payload)
	if !ok {
		return
	}
	t.prompt, t.completion, t.found = prompt, completion, true
}

// ParseBody reads the usage an ordinary JSON answer states, in the shapes the
// OpenAI-compatible APIs use: chat completions report prompt_tokens and
// completion_tokens, the Responses API reports input_tokens and output_tokens,
// and embeddings report prompt_tokens only.
func ParseBody(raw []byte) (int64, int64, bool) {
	if len(raw) == 0 {
		return 0, 0, false
	}
	return parseUsageObject(raw)
}

// parseUsageObject reads one JSON document's top-level usage object.
func parseUsageObject(raw []byte) (int64, int64, bool) {
	var envelope struct {
		Usage *struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
			InputTokens      int64 `json:"input_tokens"`
			OutputTokens     int64 `json:"output_tokens"`
			TotalTokens      int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil || envelope.Usage == nil {
		return 0, 0, false
	}
	usage := envelope.Usage
	prompt, completion := usage.PromptTokens, usage.CompletionTokens
	if prompt == 0 {
		prompt = usage.InputTokens
	}
	if completion == 0 {
		completion = usage.OutputTokens
	}
	// A provider that states only the total still tells the operator what the
	// request cost; it is recorded as prompt tokens because there is no way to
	// split a total it did not split.
	if prompt == 0 && completion == 0 && usage.TotalTokens > 0 {
		prompt = usage.TotalTokens
	}
	if prompt == 0 && completion == 0 {
		return 0, 0, false
	}
	return prompt, completion, true
}
