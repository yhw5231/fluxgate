package transform

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strings"

	"github.com/yhw5231/fluxgate/internal/domain"
)

// ApplyHeaders clones incoming headers, removes configured names, overlays configured values,
// and replaces downstream authorization with the selected channel credential.
func ApplyHeaders(source http.Header, rules domain.TransformRules, apiKey string) http.Header {
	result := source.Clone()
	if result == nil {
		result = make(http.Header)
	}
	for _, name := range rules.RemoveHeaders {
		result.Del(strings.TrimSpace(name))
	}
	for name, values := range rules.SetHeaders {
		result.Del(name)
		for _, value := range values {
			result.Add(name, value)
		}
	}
	result.Del("Host")
	result.Del("Content-Length")
	if strings.TrimSpace(apiKey) != "" {
		result.Set("Authorization", "Bearer "+strings.TrimSpace(apiKey))
	}
	result.Set("Content-Type", "application/json")
	return result
}

// MapModel applies case-insensitive exact mappings before glob mappings.
func MapModel(requested string, mappings map[string]string) string {
	requested = strings.TrimSpace(requested)
	for pattern, target := range mappings {
		if strings.EqualFold(strings.TrimSpace(pattern), requested) && strings.TrimSpace(target) != "" {
			return strings.TrimSpace(target)
		}
	}
	for pattern, target := range mappings {
		if strings.TrimSpace(target) == "" {
			continue
		}
		matched, err := path.Match(strings.ToLower(strings.TrimSpace(pattern)), strings.ToLower(requested))
		if err == nil && matched {
			return strings.TrimSpace(target)
		}
	}
	return requested
}

// ApplyJSON mutates a JSON object using JSON Pointer or dotted paths.
func ApplyJSON(body []byte, mappedModel string, rules domain.TransformRules) ([]byte, error) {
	var document map[string]any
	if err := json.Unmarshal(body, &document); err != nil {
		return nil, fmt.Errorf("decode request JSON: %w", err)
	}
	if document == nil {
		return nil, errors.New("request JSON must be an object")
	}
	if strings.TrimSpace(mappedModel) != "" {
		document["model"] = mappedModel
	}
	for _, pointer := range rules.DeleteJSON {
		if err := deleteValue(document, parsePath(pointer)); err != nil {
			return nil, err
		}
	}
	for pointer, value := range rules.OverrideJSON {
		if err := setValue(document, parsePath(pointer), value); err != nil {
			return nil, err
		}
	}
	return json.Marshal(document)
}

func parsePath(value string) []string {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "/") {
		parts := strings.Split(strings.TrimPrefix(value, "/"), "/")
		for index := range parts {
			parts[index] = strings.ReplaceAll(strings.ReplaceAll(parts[index], "~1", "/"), "~0", "~")
		}
		return parts
	}
	if value == "" {
		return nil
	}
	return strings.Split(value, ".")
}

func deleteValue(document map[string]any, parts []string) error {
	if len(parts) == 0 {
		return errors.New("cannot delete the JSON document root")
	}
	parent, err := objectParent(document, parts[:len(parts)-1], false)
	if err != nil || parent == nil {
		return err
	}
	delete(parent, parts[len(parts)-1])
	return nil
}

func setValue(document map[string]any, parts []string, value any) error {
	if len(parts) == 0 {
		return errors.New("cannot replace the JSON document root")
	}
	parent, err := objectParent(document, parts[:len(parts)-1], true)
	if err != nil {
		return err
	}
	parent[parts[len(parts)-1]] = value
	return nil
}

func objectParent(document map[string]any, parts []string, create bool) (map[string]any, error) {
	current := document
	for _, part := range parts {
		if part == "" {
			return nil, errors.New("JSON path contains an empty segment")
		}
		next, exists := current[part]
		if !exists {
			if !create {
				return nil, nil
			}
			child := make(map[string]any)
			current[part] = child
			current = child
			continue
		}
		child, ok := next.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("JSON path segment %q is not an object", part)
		}
		current = child
	}
	return current, nil
}
