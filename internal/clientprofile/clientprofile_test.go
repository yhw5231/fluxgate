package clientprofile

import (
	"net/http"
	"regexp"
	"strings"
	"testing"
)

// supportedClients is the catalog the project documents. It is listed here so a
// preset that is dropped, renamed, or reordered fails a test rather than
// quietly changing what an existing configuration means.
var supportedClients = []string{
	"claude-code", "codex", "openclaw", "pi", "opencode", "ohmyopencode",
	"kilo-code", "roocode", "cline", "crush", "droid", "qwencode", "hermes",
	"zcode", "qoder", "kimi-code", "craft-agent", "trae", "github-copilot",
	"workbuddy", "cursor", "deepseek-harness", "mimo-code",
}

// forbiddenHeaders are the headers a preset must never carry: the gateway, the
// transport, or the target decides them. A preset carrying Authorization would
// replace the upstream credential the gateway injects.
var forbiddenHeaders = map[string]string{
	"Authorization":     "the gateway injects the upstream credential",
	"Content-Length":    "the HTTP client computes it from the body",
	"Host":              "the target URL decides it",
	"Connection":        "the transport manages connection reuse",
	"Transfer-Encoding": "the transport decides it",
}

func TestCatalogMatchesSupportedClients(t *testing.T) {
	if len(profiles) != len(supportedClients) {
		t.Fatalf("catalog has %d presets, want %d", len(profiles), len(supportedClients))
	}
	seen := make(map[string]bool, len(profiles))
	for index, want := range supportedClients {
		got := profiles[index]
		if got.ID != want {
			t.Fatalf("preset %d = %q, want %q", index, got.ID, want)
		}
		if seen[got.ID] {
			t.Fatalf("duplicate preset id %q", got.ID)
		}
		seen[got.ID] = true
		if strings.TrimSpace(got.Name) == "" {
			t.Fatalf("preset %q has no display name", got.ID)
		}
		if Normalize(got.ID) != got.ID {
			t.Fatalf("preset id %q is not in normalized form", got.ID)
		}
		if _, ok := Lookup(got.ID); !ok {
			t.Fatalf("preset %q cannot be looked up by its id", got.ID)
		}
	}
	if ids := IDs(); len(ids) != len(supportedClients) {
		t.Fatalf("IDs() returned %d entries, want %d", len(ids), len(supportedClients))
	}
}

func TestCatalogPutsStarredClientsFirst(t *testing.T) {
	for _, id := range []string{"claude-code", "codex"} {
		profile, ok := Lookup(id)
		if !ok {
			t.Fatalf("starred preset %q is missing", id)
		}
		if !profile.Starred {
			t.Fatalf("preset %q should be starred", id)
		}
	}
}

func TestHeaderSanity(t *testing.T) {
	valid := map[string]bool{
		ConfidenceVerified: true, ConfidencePartial: true, ConfidenceUnverified: true,
	}
	for _, profile := range profiles {
		if !valid[profile.Confidence] {
			t.Fatalf("preset %q has confidence %q", profile.ID, profile.Confidence)
		}
		if profile.Confidence != ConfidenceUnverified && len(profile.Headers) == 0 {
			t.Fatalf("preset %q is %s but carries no headers", profile.ID, profile.Confidence)
		}
		if profile.Confidence != ConfidenceVerified && strings.TrimSpace(profile.Note) == "" {
			t.Fatalf("preset %q is not verified and needs a note explaining what is inferred", profile.ID)
		}
		if profile.Confidence == ConfidenceVerified && len(profile.Evidence) == 0 {
			t.Fatalf("preset %q is verified and needs its evidence", profile.ID)
		}
		for name, value := range profile.Headers {
			if strings.TrimSpace(name) == "" || strings.TrimSpace(value) == "" {
				t.Fatalf("preset %q has an empty header: %q=%q", profile.ID, name, value)
			}
			if reason, forbidden := forbiddenHeaders[http.CanonicalHeaderKey(name)]; forbidden {
				t.Fatalf("preset %q carries %q: %s", profile.ID, name, reason)
			}
			if strings.ContainsAny(name, "\r\n") || strings.ContainsAny(value, "\r\n") {
				t.Fatalf("preset %q has a header with a newline: %q=%q", profile.ID, name, value)
			}
		}
		for name, generator := range profile.Dynamic {
			if strings.TrimSpace(name) == "" {
				t.Fatalf("preset %q has an empty dynamic header name", profile.ID)
			}
			if reference, ok := sameHeader(generator); ok {
				if !hasHeader(profile, reference) {
					t.Fatalf("preset %q references the missing header %q", profile.ID, reference)
				}
				continue
			}
			if dynamicValue(generator) == "" {
				t.Fatalf("preset %q uses the unknown generator %q", profile.ID, generator)
			}
		}
	}
}

// hasHeader reports whether a preset sends a header under either name, compared
// canonically.
func hasHeader(profile Profile, name string) bool {
	want := http.CanonicalHeaderKey(name)
	for candidate := range profile.Headers {
		if http.CanonicalHeaderKey(candidate) == want {
			return true
		}
	}
	for candidate := range profile.Dynamic {
		if http.CanonicalHeaderKey(candidate) == want {
			return true
		}
	}
	return false
}

func TestNormalizeAndAliases(t *testing.T) {
	for alias, target := range aliases {
		if got := Normalize(alias); got != target {
			t.Fatalf("Normalize(%q) = %q, want %q", alias, got, target)
		}
		if _, ok := Lookup(target); !ok {
			t.Fatalf("alias %q points at the unknown preset %q", alias, target)
		}
	}
	for _, input := range []string{"Claude-Code", "claude_code", "  CLAUDE-CODE  ", "claude", "Claude Code"} {
		if got := Normalize(input); got != "claude-code" {
			t.Fatalf("Normalize(%q) = %q, want claude-code", input, got)
		}
	}
	if got := Normalize("   "); got != "" {
		t.Fatalf("Normalize(blank) = %q, want an empty string", got)
	}
	if _, ok := Lookup("no-such-client"); ok {
		t.Fatal("an unknown id must not resolve to a preset")
	}
	if !Valid("") || !Valid("codex") || Valid("no-such-client") {
		t.Fatal("Valid disagrees with Lookup")
	}
}

func TestDynamicHeaders(t *testing.T) {
	uuidPattern := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	if value := dynamicValue("uuid"); !uuidPattern.MatchString(value) {
		t.Fatalf("uuid = %q, which is not a v4 UUID", value)
	}
	if value := dynamicValue("UUID"); !uuidPattern.MatchString(value) {
		t.Fatalf("a generator name is case-insensitive; got %q", value)
	}
	if value := dynamicValue("uuid32"); len(value) != 32 || strings.Contains(value, "-") {
		t.Fatalf("uuid32 = %q, want 32 hex characters", value)
	}
	if value := dynamicValue("rand16"); len(value) != 16 {
		t.Fatalf("rand16 = %q, want 16 hex characters", value)
	}
	if value := dynamicValue("rand32"); len(value) != 32 {
		t.Fatalf("rand32 = %q, want 32 hex characters", value)
	}
	if value := dynamicValue("nonsense"); value != "" {
		t.Fatalf("an unknown generator must not send a header; got %q", value)
	}
}

func TestHeadersExpandsDynamicValues(t *testing.T) {
	headers := Headers("workbuddy")
	if headers == nil {
		t.Fatal("workbuddy expands to no headers")
	}
	// The two conversation identifiers are one value sent under two names.
	if first, second := headers["X-Conversation-Id"], headers["X-Conversation-Request-Id"]; first == "" || first != second {
		t.Fatalf("conversation identifiers differ: %q vs %q", first, second)
	}
	if Headers("no-such-client") != nil {
		t.Fatal("an unknown preset must expand to nothing")
	}
	if Headers("") != nil {
		t.Fatal("no preset must expand to nothing")
	}
}

func TestLookupReturnsACopy(t *testing.T) {
	profile, ok := Lookup("codex")
	if !ok {
		t.Fatal("codex is missing")
	}
	original := profiles[0].Headers
	profile.Headers["injected"] = "1"
	if _, changed := original["injected"]; changed {
		t.Fatal("a caller changed the shared preset through Lookup")
	}
	catalog := Catalog()
	for index := range catalog {
		if catalog[index].Headers != nil {
			catalog[index].Headers["injected"] = "1"
		}
		if catalog[index].Dynamic != nil {
			catalog[index].Dynamic["injected"] = "uuid"
		}
	}
	for index := range profiles {
		if _, changed := profiles[index].Headers["injected"]; changed {
			t.Fatalf("a caller changed preset %q through Catalog", profiles[index].ID)
		}
		if _, changed := profiles[index].Dynamic["injected"]; changed {
			t.Fatalf("a caller changed preset %q through Catalog", profiles[index].ID)
		}
	}
}

func TestSameHeaderReference(t *testing.T) {
	header, ok := sameHeader("same:X-Conversation-ID")
	if !ok || header != "X-Conversation-Id" {
		t.Fatalf("sameHeader = %q, %v", header, ok)
	}
	if _, ok := sameHeader("uuid"); ok {
		t.Fatal("a generator is not a same-header reference")
	}
	if _, ok := sameHeader("same:"); ok {
		t.Fatal("an empty reference is not a reference")
	}
}
