package transform

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/yhw5231/fluxgate/internal/domain"
)

func TestApplyHeadersRemovesOverridesAndReplacesAuthorization(t *testing.T) {
	source := http.Header{
		"Authorization":  []string{"Bearer downstream"},
		"X-Remove-Me":    []string{"secret"},
		"X-Replace-Me":   []string{"old"},
		"Content-Length": []string{"123"},
	}
	rules := domain.TransformRules{
		RemoveHeaders: []string{"x-remove-me"},
		SetHeaders: http.Header{
			"X-Replace-Me": []string{"new", "second"},
			"X-Added":      []string{"yes"},
		},
	}

	result := ApplyHeaders(source, rules, "upstream-key")

	if got := result.Get("Authorization"); got != "Bearer upstream-key" {
		t.Fatalf("Authorization = %q, want upstream credential", got)
	}
	if got := result.Get("X-Remove-Me"); got != "" {
		t.Fatalf("X-Remove-Me = %q, want removed", got)
	}
	if got := result.Values("X-Replace-Me"); len(got) != 2 || got[0] != "new" || got[1] != "second" {
		t.Fatalf("X-Replace-Me = %#v, want configured values", got)
	}
	if got := result.Get("X-Added"); got != "yes" {
		t.Fatalf("X-Added = %q, want yes", got)
	}
	if got := result.Get("Content-Length"); got != "" {
		t.Fatalf("Content-Length = %q, want removed", got)
	}
	if got := source.Get("Authorization"); got != "Bearer downstream" {
		t.Fatalf("source Authorization mutated to %q", got)
	}
}

func TestApplyJSONMapsDeletesAndOverrides(t *testing.T) {
	body := []byte(`{
		"model":"gpt-4.1",
		"stream":true,
		"metadata":{"secret":"remove","keep":"yes"},
		"nested":{"old":1}
	}`)
	rules := domain.TransformRules{
		DeleteJSON: []string{"/metadata/secret", "nested.old"},
		OverrideJSON: map[string]any{
			"/stream":           false,
			"nested.created":    "value",
			"/new/object/value": 42,
		},
	}

	result, err := ApplyJSON(body, "upstream-model", rules)
	if err != nil {
		t.Fatalf("ApplyJSON() error = %v", err)
	}

	var document map[string]any
	if err := json.Unmarshal(result, &document); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if got := document["model"]; got != "upstream-model" {
		t.Fatalf("model = %#v, want upstream-model", got)
	}
	if got := document["stream"]; got != false {
		t.Fatalf("stream = %#v, want false", got)
	}
	metadata := document["metadata"].(map[string]any)
	if _, exists := metadata["secret"]; exists {
		t.Fatal("metadata.secret was not deleted")
	}
	if got := metadata["keep"]; got != "yes" {
		t.Fatalf("metadata.keep = %#v, want yes", got)
	}
	nested := document["nested"].(map[string]any)
	if _, exists := nested["old"]; exists {
		t.Fatal("nested.old was not deleted")
	}
	if got := nested["created"]; got != "value" {
		t.Fatalf("nested.created = %#v, want value", got)
	}
	newObject := document["new"].(map[string]any)["object"].(map[string]any)
	if got := newObject["value"]; got != float64(42) {
		t.Fatalf("new.object.value = %#v, want 42", got)
	}
}

func TestApplyJSONSupportsEscapedJSONPointerSegments(t *testing.T) {
	body := []byte(`{"a/b":{"~key":"remove"}}`)
	rules := domain.TransformRules{DeleteJSON: []string{"/a~1b/~0key"}}

	result, err := ApplyJSON(body, "", rules)
	if err != nil {
		t.Fatalf("ApplyJSON() error = %v", err)
	}
	if string(result) != `{"a/b":{}}` {
		t.Fatalf("ApplyJSON() = %s, want escaped pointer field deleted", result)
	}
}

func TestApplyJSONRejectsTraversalThroughScalar(t *testing.T) {
	_, err := ApplyJSON(
		[]byte(`{"metadata":"scalar"}`),
		"",
		domain.TransformRules{OverrideJSON: map[string]any{"metadata.value": true}},
	)
	if err == nil {
		t.Fatal("ApplyJSON() error = nil, want scalar traversal error")
	}
}
