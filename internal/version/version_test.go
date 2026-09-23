package version

import "testing"

func TestFormatAbbreviatesAndMarksModifiedCheckouts(t *testing.T) {
	for _, test := range []struct {
		revision string
		modified bool
		want     string
	}{
		{revision: "38553d8aa1b2c3d4e5f60718293a4b5c6d7e8f90", want: "38553d8"},
		{revision: "38553d8aa1b2c3d4e5f60718293a4b5c6d7e8f90", modified: true, want: "38553d8-dirty"},
		// A revision that is already shorter than the abbreviation stays whole.
		{revision: "38553d8", want: "38553d8"},
		{revision: "", want: "unknown"},
		{revision: "", modified: true, want: "unknown"},
	} {
		if got := format(test.revision, test.modified); got != test.want {
			t.Errorf("format(%q, %v) = %q, want %q", test.revision, test.modified, got, test.want)
		}
	}
}

func TestInjectedCommitOverridesTheStampedRevision(t *testing.T) {
	defer func(previous string) { Commit = previous }(Commit)
	Commit = "abc1234def5678901234567890abcdef12345678"
	if got := Revision(); got != Commit {
		t.Fatalf("Revision() = %q, want the injected %q", got, Commit)
	}
	if got := Short(); got != "abc1234" {
		t.Fatalf("Short() = %q, want %q", got, "abc1234")
	}
}
