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

// One model is written several ways: a listing prefixes the channel, suffixes a
// variant, and a client capitalizes it. All of them name the same model.
func TestCanonicalReducesEverySpellingOfOneModel(t *testing.T) {
	for _, test := range []struct {
		value string
		want  string
	}{
		{value: "DeepSeek-V4.1-Flash", want: "deepseek-v4.1-flash"},
		{value: "cline-free/deepseek-v4.1-flash", want: "deepseek-v4.1-flash"},
		{value: "cline-free/deepseek-v4.1-flash:free", want: "deepseek-v4.1-flash"},
		{value: "  deepseek-ai/deepseek-v4-free  ", want: "deepseek-v4"},
		{value: "deepseek-v4.1-flash:nitro", want: "deepseek-v4.1-flash"},
		{value: "deepseek-v4.1-flash:extended", want: "deepseek-v4.1-flash"},
		{value: "gpt-4o", want: "gpt-4o"},
		{value: "openrouter/anthropic/claude-3.5-sonnet", want: "claude-3.5-sonnet"},
		{value: "", want: ""},
		{value: "   ", want: ""},
		{value: ":", want: ""},
		{value: "cline-free/", want: ""},
	} {
		if got := Canonical(test.value); got != test.want {
			t.Errorf("Canonical(%q) = %q, want %q", test.value, got, test.want)
		}
	}
}

// A platform that serves its models from channel groups writes the group before
// the colon, so there the name after it is the model and not a variant of one.
// Reading every colon as a variant suffix would leave "cn" as the identity of
// every model that platform serves, and the plain model name it lists would be
// spelled by its display label instead.
func TestCanonicalKeepsAChannelGroupPrefix(t *testing.T) {
	for _, value := range []string{"cn:deepseek-v4.1-flash", "cn:auto", "global:deepseek-v4.1-flash", "cn:hy3-x"} {
		if got := Canonical(value); got != value {
			t.Errorf("Canonical(%q) = %q, want the name kept whole", value, got)
		}
	}
	if Equivalent("cn:auto", "cn:deepseek-v4.1-flash") {
		t.Fatal("two models of one channel group compared equivalent")
	}
	if Equivalent("cn:deepseek-v4.1-flash", "deepseek-v4.1-flash") {
		t.Fatal("a channel group's model compared equivalent to the plain name")
	}
}

func TestEquivalentComparesModelIdentity(t *testing.T) {
	for _, test := range []struct {
		left  string
		right string
		want  bool
	}{
		{left: "DeepSeek-V4.1-Flash", right: "deepseek-v4.1-flash", want: true},
		{left: "cline-free/deepseek-v4.1-flash:free", right: "deepseek-v4.1-flash", want: true},
		{left: "deepseek-v4.1-flash", right: "deepseek-v4.1-pro", want: false},
		// A value that carries no model name must never match another one, or an
		// empty column would make every model equivalent to every other.
		{left: ":", right: ":", want: false},
		{left: "", right: "", want: false},
		{left: "cline-free/", right: "/", want: false},
	} {
		if got := Equivalent(test.left, test.right); got != test.want {
			t.Errorf("Equivalent(%q, %q) = %v, want %v", test.left, test.right, got, test.want)
		}
	}
}

// A route or a deny list written for the plain model name has to answer for the
// decorated spellings of it, or a client could walk around a restriction by
// asking for a channel's own spelling.
func TestMatchNameAcceptsEverySpellingOfTheModel(t *testing.T) {
	for _, test := range []struct {
		model   string
		pattern string
		want    bool
	}{
		{model: "cline-free/deepseek-v4.1-flash:free", pattern: "deepseek-v4.1-flash", want: true},
		{model: "DeepSeek-V4.1-Flash", pattern: "deepseek-v4.1-flash", want: true},
		{model: "cline-free/deepseek-v4.1-flash:free", pattern: "deepseek-*", want: true},
		{model: "cline-free/deepseek-v4.1-pro", pattern: "deepseek-v4.1-flash", want: false},
		// A regex is written against what the client sends, so it is matched
		// against the name as sent only.
		{model: "cline-free/deepseek-v4.1-flash:free", pattern: `re:^deepseek-`, want: false},
		{model: "deepseek-v4.1-flash", pattern: `re:^deepseek-`, want: true},
	} {
		if got := MatchName(test.model, test.pattern); got != test.want {
			t.Errorf("MatchName(%q, %q) = %v, want %v", test.model, test.pattern, got, test.want)
		}
	}
}
