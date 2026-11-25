// Package clientprofile carries the built-in client fingerprint presets: the
// request headers a real AI coding client sends when it talks to a model API.
//
// An upstream frequently treats one client family differently from another — a
// coding agent gets a different error, a different rate limit, and a different
// model list than a bare HTTP client — so an operator sometimes needs the
// gateway to reach that upstream as the client the upstream expects. The preset
// supplies those headers; the operator selects one per upstream, and the
// upstream's own configured headers still override any header of the preset.
//
// The presets are data, not code: each one records where its headers were
// observed and how confident that observation is, so an operator can see which
// values were verified against public sources and which were filled in from the
// conventions of a client family. Authentication headers are deliberately
// absent: the gateway injects the upstream's own credential, and a preset that
// carried an Authorization header would replace it.
package clientprofile

import (
	"crypto/rand"
	"encoding/hex"
	"net/textproto"
	"strconv"
	"strings"
	"time"
)

// Profile is one client fingerprint preset.
type Profile struct {
	// ID is the stable identifier stored in the configuration and used in URLs.
	ID string `json:"id"`
	// Name is the label the console shows.
	Name string `json:"name"`
	// Starred marks the clients an operator reaches for most often, which the
	// console lists first.
	Starred bool `json:"starred,omitempty"`
	// Confidence says how well the header values are evidenced: verified,
	// partial, or unverified.
	Confidence string `json:"confidence"`
	// Headers are sent as written on every request.
	Headers map[string]string `json:"headers"`
	// Dynamic maps a header name to a generator whose value is produced per
	// request, such as a fresh session identifier.
	Dynamic map[string]string `json:"dynamic,omitempty"`
	// Evidence records where the header values were observed.
	Evidence []string `json:"evidence,omitempty"`
	// Note explains the version a preset was taken from and which headers are
	// inferred rather than verified.
	Note string `json:"note,omitempty"`
}

// How well a preset's header values are evidenced.
const (
	ConfidenceVerified   = "verified"
	ConfidencePartial    = "partial"
	ConfidenceUnverified = "unverified"
)

// aliases maps the spellings an operator is likely to type onto the preset id
// they mean, so a hand-written configuration and an API call accept the same
// convenient names the console does.
var aliases = map[string]string{
	"claude":          "claude-code",
	"claudecli":       "claude-code",
	"anthropic-cli":   "claude-code",
	"openai-codex":    "codex",
	"roo":             "roocode",
	"roo-code":        "roocode",
	"roocline":        "roocode",
	"kilo":            "kilo-code",
	"kilocode":        "kilo-code",
	"qwen":            "qwencode",
	"qwen-code":       "qwencode",
	"qwencli":         "qwencode",
	"kimi":            "kimi-code",
	"kimicode":        "kimi-code",
	"copilot":         "github-copilot",
	"githubcopilot":   "github-copilot",
	"dsh":             "deepseek-harness",
	"deepseekharness": "deepseek-harness",
	"oh-my-opencode":  "ohmyopencode",
	"mimocode":        "mimo-code",
	"z-code":          "zcode",
	"zai":             "zcode",
}

// Normalize renders the canonical form of a profile id: trimmed, lowercased,
// underscores read as hyphens, and an alias expanded to the preset it names. An
// empty value stays empty, which means "no preset", and an unknown value is
// returned as given so the caller can report it rather than silently dropping a
// configured choice.
func Normalize(id string) string {
	value := strings.ToLower(strings.TrimSpace(id))
	if value == "" {
		return ""
	}
	value = strings.ReplaceAll(value, "_", "-")
	if known(value) {
		return value
	}
	if alias, ok := aliases[value]; ok {
		return alias
	}
	// A display name ("Kimi Code") is accepted as well, because that is what the
	// console shows and what an operator copies into a hand-written row.
	if collapsed := strings.Join(strings.Fields(value), "-"); collapsed != value {
		if known(collapsed) {
			return collapsed
		}
		if alias, ok := aliases[collapsed]; ok {
			return alias
		}
	}
	return value
}

// known reports whether an id names a built-in preset exactly.
func known(id string) bool {
	for index := range profiles {
		if profiles[index].ID == id {
			return true
		}
	}
	return false
}

// Lookup resolves an id, or an alias of one, to its preset. The returned value is
// a copy, so a caller cannot change what the next request sees.
func Lookup(id string) (Profile, bool) {
	normalized := Normalize(id)
	if normalized == "" {
		return Profile{}, false
	}
	for index := range profiles {
		if profiles[index].ID == normalized {
			return clone(profiles[index]), true
		}
	}
	return Profile{}, false
}

// Valid reports whether a value names a preset or asks for none at all.
func Valid(id string) bool {
	if strings.TrimSpace(id) == "" {
		return true
	}
	_, ok := Lookup(id)
	return ok
}

// IDs lists every preset id in the order the console shows them.
func IDs() []string {
	ids := make([]string, 0, len(profiles))
	for index := range profiles {
		ids = append(ids, profiles[index].ID)
	}
	return ids
}

// Catalog returns every preset for a management response. The values are copies,
// so the caller may render them, sort them, or hand them to a template without
// changing the presets the gateway applies.
func Catalog() []Profile {
	out := make([]Profile, 0, len(profiles))
	for index := range profiles {
		out = append(out, clone(profiles[index]))
	}
	return out
}

// Headers expands one preset into the headers a single outbound request carries:
// the static values as written, plus a freshly generated value for every dynamic
// header. A "same:<name>" generator copies another header of the same request,
// which is what a client does when it sends one identifier under two names.
//
// The header names are canonicalized, so a preset that spells a name in lower
// case and an upstream header rule that spells it in another case still collide
// the way HTTP says they do. An unknown id, or one that names no preset, yields
// nil: the request goes out with the caller's own headers.
func Headers(id string) map[string]string {
	profile, ok := Lookup(id)
	if !ok {
		return nil
	}
	out := make(map[string]string, len(profile.Headers)+len(profile.Dynamic))
	for name, value := range profile.Headers {
		key := textproto.CanonicalMIMEHeaderKey(strings.TrimSpace(name))
		if key == "" || value == "" {
			continue
		}
		out[key] = value
	}
	references := make(map[string]string, len(profile.Dynamic))
	for name, generator := range profile.Dynamic {
		key := textproto.CanonicalMIMEHeaderKey(strings.TrimSpace(name))
		if key == "" {
			continue
		}
		if reference, ok := sameHeader(generator); ok {
			references[key] = reference
			continue
		}
		if value := dynamicValue(generator); value != "" {
			out[key] = value
		}
	}
	for key, reference := range references {
		if value := out[reference]; value != "" {
			out[key] = value
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// sameHeader resolves a "same:<header>" dynamic reference.
func sameHeader(generator string) (string, bool) {
	value := strings.ToLower(strings.TrimSpace(generator))
	if !strings.HasPrefix(value, "same:") {
		return "", false
	}
	reference := textproto.CanonicalMIMEHeaderKey(strings.TrimSpace(value[len("same:"):]))
	if reference == "" {
		return "", false
	}
	return reference, true
}

// dynamicValue produces one header value. An unknown generator yields an empty
// string, which means the header is not sent on this request.
func dynamicValue(generator string) string {
	switch strings.ToLower(strings.TrimSpace(generator)) {
	case "uuid":
		return randomUUID()
	case "uuid32":
		return strings.ReplaceAll(randomUUID(), "-", "")
	case "ts_ms":
		return strconv.FormatInt(time.Now().UnixMilli(), 10)
	case "ts_s":
		return strconv.FormatInt(time.Now().Unix(), 10)
	case "rand16":
		return randomHex(8)
	case "rand32":
		return randomHex(16)
	default:
		return ""
	}
}

// randomUUID returns a version 4 UUID, the shape a client uses for a session
// identifier. If the entropy source fails it falls back to a timestamp: a
// repeated identifier is better than a request that never leaves.
func randomUUID() string {
	var buffer [16]byte
	if _, err := rand.Read(buffer[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	buffer[6] = (buffer[6] & 0x0f) | 0x40
	buffer[8] = (buffer[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(buffer[:])
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:32]
}

// randomHex returns n random bytes as a lowercase hex string.
func randomHex(n int) string {
	buffer := make([]byte, n)
	if _, err := rand.Read(buffer); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(buffer)
}

// clone copies a preset so a caller holding it cannot change the shared value.
func clone(profile Profile) Profile {
	profile.Headers = cloneMap(profile.Headers)
	profile.Dynamic = cloneMap(profile.Dynamic)
	if profile.Evidence != nil {
		profile.Evidence = append([]string(nil), profile.Evidence...)
	}
	return profile
}

func cloneMap(source map[string]string) map[string]string {
	if len(source) == 0 {
		return nil
	}
	out := make(map[string]string, len(source))
	for key, value := range source {
		out[key] = value
	}
	return out
}
