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
