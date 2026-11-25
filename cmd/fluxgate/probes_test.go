package main

import (
	"testing"

	"github.com/yhw5231/fluxgate/internal/domain"
	"github.com/yhw5231/fluxgate/internal/store"
)

// A probe verifies one credential, so a key that serves two models is asked once:
// asking it twice would spend the account's quota to answer the same question.
func TestProbeLinesKeepsOneLinePerCredential(t *testing.T) {
	configuration := store.Configuration{
		Routes: []domain.Route{
			{ID: 1, ModelPattern: "gpt-4.1", Mode: domain.RouteModePattern, Enabled: true, Channels: []domain.Channel{
				{ID: "10", Name: "Example", BaseURL: "https://a.example/v1", APIKey: "key-one", SiteID: 1, Enabled: true},
				{ID: "11", Name: "Example", BaseURL: "https://a.example/v1", APIKey: "key-two", SiteID: 1, Enabled: true},
			}},
			{ID: 2, ModelPattern: "claude-3", Mode: domain.RouteModePattern, Enabled: true, Channels: []domain.Channel{
				{ID: "12", Name: "Example", BaseURL: "https://a.example/v1", APIKey: "key-one", SiteID: 1, Enabled: true},
				{ID: "13", Name: "Example", BaseURL: "https://a.example/v1", APIKey: "key-two", SiteID: 1, Enabled: true},
			}},
		},
	}
	lines := probeLines(configuration)
	if len(lines) != 2 {
		t.Fatalf("lines = %#v, want one per credential", lines)
	}
	models := map[string]string{}
	for _, line := range lines {
		models[line.APIKey] = line.Model
	}
	if models["key-one"] != "gpt-4.1" || models["key-two"] != "gpt-4.1" {
		t.Fatalf("models = %#v, want the first exact model of each credential", models)
	}
}

// A line whose routes only name a pattern has no model name to send, so it is not
// probed: a glob sent as a model would report a working account as broken.
func TestProbeLinesSkipsALineWithNoExactModel(t *testing.T) {
	configuration := store.Configuration{
		Routes: []domain.Route{
			{ID: 1, ModelPattern: "gpt-*", Mode: domain.RouteModePattern, Enabled: true, Channels: []domain.Channel{
				{ID: "10", Name: "Glob", BaseURL: "https://a.example/v1", APIKey: "key-one", SiteID: 1, Enabled: true},
			}},
		},
	}
	if lines := probeLines(configuration); len(lines) != 0 {
		t.Fatalf("lines = %#v, want none for a pattern-only route", lines)
	}

	// The spelling the upstream listed is a concrete name, so it can be asked.
	configuration.Routes[0].Channels[0].SourceModel = "gpt-4.1-2026-01-01"
	lines := probeLines(configuration)
	if len(lines) != 1 || lines[0].Model != "gpt-4.1-2026-01-01" {
		t.Fatalf("lines = %#v, want the source model", lines)
	}
}

// A group route exposes a display name that may be the operator's alias; the
// upstream has never heard of it, so only a concrete source model makes the line
// probeable.
func TestProbeLinesDoesNotSendAGroupDisplayName(t *testing.T) {
	configuration := store.Configuration{
		Routes: []domain.Route{
			{ID: 1, Mode: domain.RouteModeExplicitGroup, DisplayName: "fast-models", Enabled: true, Channels: []domain.Channel{
				{ID: "10", Name: "Group", BaseURL: "https://a.example/v1", APIKey: "key-one", SiteID: 1, Enabled: true},
			}},
		},
	}
	if lines := probeLines(configuration); len(lines) != 0 {
		t.Fatalf("lines = %#v, want none for a display-name-only group", lines)
	}

	configuration.Routes[0].Channels[0].SourceModel = "cn:deepseek-v4.1-flash"
	lines := probeLines(configuration)
	if len(lines) != 1 || lines[0].Model != "cn:deepseek-v4.1-flash" {
		t.Fatalf("lines = %#v, want the source model", lines)
	}
}

// A disabled line, a disabled route, or a line without a credential is not
// probed: there is nothing to spend a question on.
func TestProbeLinesSkipsWhatCannotAnswer(t *testing.T) {
	configuration := store.Configuration{
		Routes: []domain.Route{
			{ID: 1, ModelPattern: "gpt-4.1", Mode: domain.RouteModePattern, Enabled: false, Channels: []domain.Channel{
				{ID: "10", Name: "Disabled route", BaseURL: "https://a.example/v1", APIKey: "key-one", SiteID: 1, Enabled: true},
			}},
			{ID: 2, ModelPattern: "gpt-4.1", Mode: domain.RouteModePattern, Enabled: true, Channels: []domain.Channel{
				{ID: "11", Name: "Disabled line", BaseURL: "https://a.example/v1", APIKey: "key-two", SiteID: 1, Enabled: false},
				{ID: "12", Name: "No credential", BaseURL: "https://a.example/v1", SiteID: 1, Enabled: true},
			}},
		},
	}
	if lines := probeLines(configuration); len(lines) != 0 {
		t.Fatalf("lines = %#v, want none", lines)
	}
}
