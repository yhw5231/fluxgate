package pattern

import "testing"

func TestMatchHandlesGlobWildcards(t *testing.T) {
	for _, test := range []struct {
		model   string
		pattern string
		want    bool
	}{
		{model: "gpt-4o", pattern: "gpt-*", want: true},
		{model: "gpt-4o", pattern: "gpt-4?", want: true},
		{model: "gpt-4o-mini", pattern: "gpt-4?", want: false},
		{model: "gpt-4o", pattern: "*", want: true},
		{model: "gpt-4o", pattern: "g?t-*", want: true},
		{model: "claude-3", pattern: "gpt-*", want: false},
		{model: "GPT-4O", pattern: "gpt-*", want: true},
		{model: "gpt-4o", pattern: "", want: false},
		// Character classes are literal here, unlike path.Match.
		{model: "gpt-4", pattern: "gpt-[45]", want: false},
	} {
		if got := Match(test.model, test.pattern); got != test.want {
			t.Errorf("Match(%q, %q) = %v, want %v", test.model, test.pattern, got, test.want)
		}
	}
}

func TestMatchHandlesRegexPrefix(t *testing.T) {
	if !Match("claude-sonnet-4-5", `re:^claude-(opus|sonnet)-4-5$`) {
		t.Fatal("regex alternation did not match")
	}
	if Match("claude-haiku-4-5", `re:^claude-(opus|sonnet)-4-5$`) {
		t.Fatal("regex alternation matched an excluded branch")
	}
	// An invalid regex must never match rather than falling back to a glob.
	if Match("re:whatever", `re:(unclosed`) {
		t.Fatal("invalid regex pattern matched")
	}
	if Match("anything", `re:`) {
		t.Fatal("empty regex body matched")
	}
}

func TestIsExactDistinguishesLiteralPatterns(t *testing.T) {
	for _, test := range []struct {
		pattern string
		want    bool
	}{
		{pattern: "gpt-4o", want: true},
		{pattern: "gpt-*", want: false},
		{pattern: "gpt-4?", want: false},
		{pattern: `re:^gpt-4o$`, want: false},
		{pattern: "", want: false},
		{pattern: "  ", want: false},
	} {
		if got := IsExact(test.pattern); got != test.want {
			t.Errorf("IsExact(%q) = %v, want %v", test.pattern, got, test.want)
		}
	}
}
