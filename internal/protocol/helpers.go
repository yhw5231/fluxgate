package protocol

import (
	"encoding/json"
	"strings"
)

// The small readers both directions of the translation share. JSON decodes
// objects as map[string]any and arrays as []any, and every value a writer may
// have omitted is nil; these helpers state what this package does with each of
// those cases once rather than at every field.

// object reads a decoded JSON object, reporting false for anything else.
func object(value any) (map[string]any, bool) {
	typed, ok := value.(map[string]any)
	return typed, ok
}

// array reads a decoded JSON array, reporting false for anything else.
func array(value any) ([]any, bool) {
	typed, ok := value.([]any)
	return typed, ok
}

// stringField reads a string field, trimmed, and an empty string for a value
// that is absent or not a string.
func stringField(document map[string]any, name string) string {
	text, _ := document[name].(string)
	return strings.TrimSpace(text)
}

// boolField reads a boolean field, reporting whether the field is present and
// really a boolean.
func boolField(document map[string]any, name string) (bool, bool) {
	flag, ok := document[name].(bool)
	return flag, ok
}

// numberField reads a numeric field, reporting whether the field is present and
// really a number. The value is handed on as it was decoded, so a parameter the
// translation does not interpret keeps its exact representation.
func numberField(document map[string]any, name string) (any, bool) {
	value, present := document[name]
	if !present || value == nil {
		return nil, false
	}
	if _, ok := value.(float64); !ok {
		return nil, false
	}
	return value, true
}

// numberFrom reads the first numeric field that is present, which is how a
// parameter with an older and a newer spelling is carried over once.
func numberFrom(document map[string]any, names ...string) (any, bool) {
	for _, name := range names {
		if value, ok := numberField(document, name); ok {
			return value, true
		}
	}
	return nil, false
}

// copyField carries one field across unchanged when the request carries it.
func copyField(from, to map[string]any, names ...string) {
	for _, name := range names {
		if value, present := from[name]; present && value != nil {
			to[name] = value
		}
	}
}

// copyModel carries the requested model across unchanged. It is written by the
// route before a line sees the request, and a translation must not lose it.
func copyModel(from, to map[string]any) {
	if model := stringField(from, "model"); model != "" {
		to["model"] = model
	}
}

// anyText reads the text of a value that may be a string or a list of content
// parts, each carrying its own text or refusal. It joins the parts in order,
// which is how both protocols expect content to read.
func anyText(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case []any:
		var builder strings.Builder
		for _, raw := range typed {
			part, ok := object(raw)
			if !ok {
				continue
			}
			text := stringField(part, "text")
			if text == "" {
				text = stringField(part, "refusal")
			}
			if text == "" {
				continue
			}
			builder.WriteString(text)
		}
		return builder.String()
	}
	return ""
}

// compactJSON encodes a document for an event or a body, ignoring the error
// that encoding a value assembled from decoded JSON cannot produce.
func compactJSON(value any) []byte {
	encoded, err := json.Marshal(value)
	if err != nil {
		return []byte("{}")
	}
	return encoded
}
