// Package version identifies the repository revision a binary was built from.
//
// A binary built inside a git checkout carries that revision already: the Go
// toolchain stamps vcs.revision and vcs.modified into the build info, which is
// how a local build and the container image both answer without configuration.
// Commit exists for the remaining case, a build made outside a checkout (a
// source archive, a vendored copy), where there is nothing to read:
//
//	go build -ldflags "-X github.com/yhw5231/fluxgate/internal/version.Commit=$(git rev-parse HEAD)"
package version

import (
	"runtime/debug"
	"strings"
)

// Commit overrides the revision read from the build info. It is empty unless
// injected at build time.
var Commit string

// shortLength is how much of a revision the console shows. Seven characters is
// the abbreviation git itself prints.
const shortLength = 7

// Short is the version as the console shows it: the abbreviated revision, with
// "-dirty" when the checkout it was built from had uncommitted changes, and
// "unknown" when the build carries no repository information at all.
func Short() string {
	return format(revisionInfo())
}

// Revision is the full commit the binary was built from, empty when the build
// carries no repository information.
func Revision() string {
	revision, _ := revisionInfo()
	return revision
}

// format renders one revision. The dirty marker belongs to the revision form:
// it says the checkout the commit was read from had changes on top of it.
func format(revision string, modified bool) string {
	if revision == "" {
		return "unknown"
	}
	if len(revision) > shortLength {
		revision = revision[:shortLength]
	}
	if modified {
		return revision + "-dirty"
	}
	return revision
}

// revisionInfo resolves the revision and whether the checkout it was read from
// was modified, preferring an injected Commit over the stamped one.
func revisionInfo() (string, bool) {
	if Commit != "" {
		return Commit, false
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "", false
	}
	var revision string
	var modified bool
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			revision = strings.TrimSpace(setting.Value)
		case "vcs.modified":
			modified = setting.Value == "true"
		}
	}
	return revision, modified
}
