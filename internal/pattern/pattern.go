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

// Canonical reduces a model name to the identity that every spelling of it
// shares. One model is routinely written several ways: a listing may call it
// "cline-free/deepseek-v4.1-flash:free" while a client asks for
// "DeepSeek-V4.1-Flash". The channel path before the last "/", the variant
// suffix after the last ":" and a trailing "-free" name a channel's own
// arrangement of the model rather than a different model, so dropping them makes
// the spellings compare equal.
//
// An empty result means the value carried nothing else, which a caller should
// treat as no model name at all rather than as one that matches everything.
func Canonical(value string) string {
	name := strings.ToLower(strings.TrimSpace(value))
	if index := strings.LastIndex(name, "/"); index >= 0 {
		name = name[index+1:]
	}
	if index := strings.LastIndex(name, ":"); index >= 0 {
		name = name[:index]
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
