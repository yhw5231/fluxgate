// Package pattern implements the model-name pattern language shared by route
// matching, channel source-model checks, and downstream key model policies.
//
// Matching is case-insensitive. A pattern is either a glob supporting only "*"
// (any run, including empty) and "?" (exactly one character), or a regular
// expression introduced by a case-insensitive "re:" prefix. Regex bodies are
// compiled with Go's RE2 engine, which cannot backtrack catastrophically.
package pattern

import (
	"regexp"
	"strings"
)

const regexPrefix = "re:"

// Normalize lowercases and trims a model name or pattern.
func Normalize(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

// IsRegex reports whether the pattern carries the "re:" prefix.
func IsRegex(pat string) bool {
	return strings.HasPrefix(Normalize(pat), regexPrefix)
}

// IsExact reports whether the pattern is a literal model name: non-empty, not a
// regex, and free of glob wildcards.
func IsExact(pat string) bool {
	trimmed := strings.TrimSpace(pat)
	if trimmed == "" || IsRegex(trimmed) {
		return false
	}
	return !strings.ContainsAny(trimmed, "*?")
}

// variantTags are the colon-separated suffixes that are naming rather than a
// different model, which is how a router that resells one model under several
// arrangements spells them: ":free", ":nitro". The set is closed on purpose: a
// suffix that is not listed here stays part of the name, so nothing is ever
// dropped on a guess, and the colon that is left is read as the channel group
// instead — see Canonical.
var variantTags = map[string]struct{}{
	"free":           {},
	"nitro":          {},
	"thinking":       {},
	"online":         {},
	"extended":       {},
	"floor":          {},
	"beta":           {},
	"self-moderated": {},
}

// isDecoration reports whether what follows a colon is a channel's arrangement
// of the model rather than part of its name: a known variant tag, or nothing at
// all, which is a value that carries no model name past its separator.
func isDecoration(suffix string) bool {
	if suffix == "" {
		return true
	}
	_, known := variantTags[suffix]
	return known
}

// Canonical reduces a model name to the identity that every spelling of it
// shares. One model is routinely written several ways: a listing may call it
// "cline-free/deepseek-v4.1-flash:free" while a client asks for
// "DeepSeek-V4.1-Flash". The channel path before the last "/", a known variant
// suffix after the last ":" and a trailing "-free" name a channel's own
// arrangement of the model rather than a different model, so dropping them makes
// the spellings compare equal.
//
// A colon that is left is the channel group a platform serves the model from —
// "cn:deepseek-v4.1-flash", "global:gpt-5.5" — and there the name after it is the
// model itself: the group says which of a platform's lines serves the model, not
// which model it is. So the identity is what follows the first colon, which is
// what lets one model reachable as "deepseek-v4.1-flash" be served by several
// groups at once. The upstream still receives the spelling it listed, because
// that spelling travels as the channel's source model rather than in the
// identity.
//
// An empty result means the value carried nothing else, which a caller should
// treat as no model name at all rather than as one that matches everything.
func Canonical(value string) string {
	name := strings.ToLower(strings.TrimSpace(value))
	if index := strings.LastIndex(name, "/"); index >= 0 {
		name = name[index+1:]
	}
	if index := strings.LastIndex(name, ":"); index >= 0 && isDecoration(name[index+1:]) {
		name = name[:index]
	}
	if index := strings.Index(name, ":"); index > 0 && index < len(name)-1 {
		name = name[index+1:]
	}
	return strings.TrimSuffix(name, "-free")
}

// Equivalent reports whether two names are two spellings of one model: equal
// after Canonical. Two values that canonicalize to nothing are never equivalent.
func Equivalent(left, right string) bool {
	canonical := Canonical(left)
	return canonical != "" && canonical == Canonical(right)
}

// MatchName reports whether a requested model satisfies a pattern, testing the
// name as it was sent and then the name with its channel decorations removed, so
// "cline-free/deepseek-v4.1-flash:free" reaches a route or a policy written for
// "deepseek-v4.1-flash". A regex is matched against the name as sent only: it is
// written against what the client spells out.
func MatchName(model, pat string) bool {
	if Match(model, pat) {
		return true
	}
	if IsRegex(pat) {
		return false
	}
	canonical := Canonical(model)
	return canonical != "" && canonical != Normalize(model) && Match(canonical, pat)
}

// Match reports whether a model name satisfies a pattern. An empty or invalid
// pattern never matches.
func Match(model, pat string) bool {
	trimmed := strings.TrimSpace(pat)
	if trimmed == "" {
		return false
	}
	normalizedModel := Normalize(model)
	normalizedPattern := Normalize(trimmed)
	if normalizedPattern == normalizedModel {
		return true
	}
	if IsRegex(trimmed) {
		body := strings.TrimSpace(normalizedPattern[len(regexPrefix):])
		if body == "" {
			return false
		}
		compiled, err := regexp.Compile(body)
		if err != nil {
			return false
		}
		return compiled.MatchString(normalizedModel)
	}
	return matchGlob(normalizedModel, normalizedPattern)
}

// matchGlob applies a "*" and "?" only glob with greedy backtracking.
func matchGlob(model, pat string) bool {
	value := []rune(model)
	glob := []rune(pat)
	valueIndex, patternIndex := 0, 0
	starIndex, resumeIndex := -1, 0
	for valueIndex < len(value) {
		switch {
		case patternIndex < len(glob) && (glob[patternIndex] == '?' || glob[patternIndex] == value[valueIndex]):
			valueIndex++
			patternIndex++
		case patternIndex < len(glob) && glob[patternIndex] == '*':
			starIndex = patternIndex
			resumeIndex = valueIndex
			patternIndex++
		case starIndex >= 0:
			patternIndex = starIndex + 1
			resumeIndex++
			valueIndex = resumeIndex
		default:
			return false
		}
	}
	for patternIndex < len(glob) && glob[patternIndex] == '*' {
		patternIndex++
	}
	return patternIndex == len(glob)
}
