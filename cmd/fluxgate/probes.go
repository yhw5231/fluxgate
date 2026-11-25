package main

import (
	"strings"
	"sync"

	"github.com/yhw5231/fluxgate/internal/breaker"
	"github.com/yhw5231/fluxgate/internal/domain"
	"github.com/yhw5231/fluxgate/internal/probe"
	"github.com/yhw5231/fluxgate/internal/store"
)

// Which lines the account health schedule may probe, and what it asks them.
//
// A probe needs three things from a line: where to send the request, which
// credential to present, and a model name the upstream actually knows. The first
// two come straight from the loaded configuration; the model has to be chosen,
// because a route may match a model by pattern rather than name and there is no
// model to send for a pattern.
//
// One line is probed about one model: the first exact model name its routes give
// it. Asking the same credential about every model it serves would spend the
// account's quota to answer a question one request already answers.

// lineSource holds the configuration the probe schedule reads. A console write
// replaces it while the schedule is running, so it is read under a lock and
// never walked while it is being installed.
type lineSource struct {
	mu            sync.RWMutex
	configuration store.Configuration
}

// Set installs a configuration snapshot.
func (l *lineSource) Set(configuration store.Configuration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.configuration = configuration
}

// Lines reports the lines that can be probed right now.
func (l *lineSource) Lines() []probe.Line {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return probeLines(l.configuration)
}

// probeLines builds one probe target per enabled line.
func probeLines(configuration store.Configuration) []probe.Line {
	seen := make(map[string]bool, len(configuration.Channels))
	lines := make([]probe.Line, 0, len(configuration.Channels))
	for _, route := range configuration.Routes {
		if !route.Enabled {
			continue
		}
		for _, channel := range route.Channels {
			if !channel.Enabled || strings.TrimSpace(channel.APIKey) == "" {
				continue
			}
			if seen[channel.ID] {
				continue
			}
			model := probeModel(route, channel)
			if model == "" {
				// A line whose only routes name a pattern has no model to ask about.
				// Leaving it out is honest: a probe would send a glob as a model name
				// and report a working account as broken.
				continue
			}
			seen[channel.ID] = true
			lines = append(lines, probe.Line{
				ChannelID:      channel.ID,
				ChannelName:    channel.Name,
				BaseURL:        channel.BaseURL,
				APIKey:         channel.APIKey,
				Model:          model,
				BreakerMode:    channel.BreakerMode,
				ClientProfile:  channel.ClientProfile,
				Transform:      channel.Transform,
				ProxyURL:       channel.ProxyURL,
				UseSystemProxy: channel.UseSystemProxy,
			})
		}
	}
	return lines
}

// probeModel picks the model a probe asks one line about, or an empty string when
// the configuration names none exactly.
func probeModel(route domain.Route, channel domain.Channel) string {
	// The spelling the upstream listed is the name it will accept, so it wins
	// whenever it is a concrete name.
	if exactModelName(channel.SourceModel) {
		return strings.TrimSpace(channel.SourceModel)
	}
	if route.IsGroup() {
		// A group route exposes a display name that may be an operator's alias; the
		// upstream has never heard of it.
		return ""
	}
	if exactModelName(route.ModelPattern) {
		return strings.TrimSpace(route.ModelPattern)
	}
	return ""
}

// exactModelName reports whether a name can be sent to an upstream as it stands:
// it is not empty, not a regular expression, and not a glob.
func exactModelName(name string) bool {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return false
	}
	if strings.HasPrefix(trimmed, "re:") {
		return false
	}
	return !strings.ContainsAny(trimmed, "*?")
}

// observingBreaker forwards attempt outcomes to the circuit breaker and to the
// probe schedule, which uses them as the idle clock: a line that has just served
// a request is not one to spend a probe on.
type observingBreaker struct {
	breaker *breaker.Breaker
	note    func(attempt domain.Attempt)
}

func (o observingBreaker) RecordFailure(failure domain.Failure) {
	if o.breaker != nil {
		o.breaker.RecordFailure(failure)
	}
	o.noteAttempt(failure.Attempt)
}

func (o observingBreaker) RecordSuccess(attempt domain.Attempt) {
	if o.breaker != nil {
		o.breaker.RecordSuccess(attempt)
	}
	o.noteAttempt(attempt)
}

func (o observingBreaker) noteAttempt(attempt domain.Attempt) {
	if o.note != nil {
		o.note(attempt)
	}
}
