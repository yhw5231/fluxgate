package main

import (
	"fmt"
	"strings"
	"sync"

	"github.com/yhw5231/fluxgate/internal/breaker"
	"github.com/yhw5231/fluxgate/internal/domain"
	"github.com/yhw5231/fluxgate/internal/probe"
	"github.com/yhw5231/fluxgate/internal/router"
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

// probeLines builds one probe target per upstream credential.
//
// A probe is a question about an account, not about a model: one key serving
// five models is one credential to verify, and a sweep that asked it five times
// would spend five times the quota to answer the same question. The first line
// that credential appears on is kept, together with the first model whose name
// can be sent to the upstream.
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
			key := fmt.Sprintf("%d\x00%s", channel.SiteID, channel.APIKey)
			if seen[key] {
				continue
			}
			model := probeModel(route, channel)
			if model == "" {
				// A line whose only routes name a pattern has no model to ask about.
				// Leaving it out is honest: a probe would send a glob as a model name
				// and report a working account as broken.
				continue
			}
			seen[key] = true
			lines = append(lines, probe.Line{
				SiteID:         channel.SiteID,
				ChannelID:      channel.ID,
				ChannelName:    channel.Name,
				BaseURL:        channel.BaseURL,
				APIKey:         channel.APIKey,
				Model:          model,
				BreakerMode:    channel.BreakerMode,
				ClientProfile:  channel.ClientProfile,
				UpstreamAPI:    channel.UpstreamAPI,
				Transform:      channel.Transform,
				ProxyURL:       channel.ProxyURL,
				UseSystemProxy: channel.UseSystemProxy,
			})
		}
	}
	return lines
}

// probeModel picks the model a probe asks one line about, or an empty string when
// the configuration names no concrete model for that line.
//
// The name is the one a real request would carry, not the name the route exposes.
// A route may map its exposed name onto the spelling an upstream knows, and a
// channel may name its own upstream model; asking with anything else is what makes
// a working account answer 400 for a model the upstream has never heard of. The
// mapping is resolved by the router itself, so a probe asks exactly what dispatch
// would send.
func probeModel(route domain.Route, channel domain.Channel) string {
	// A group exposes an operator's alias, which no upstream knows; only a channel
	// that names its own upstream model can serve it, and only that name can be
	// asked.
	if route.IsGroup() {
		if exactModelName(channel.SourceModel) {
			return strings.TrimSpace(channel.SourceModel)
		}
		return ""
	}
	// A route that exposes a concrete model is asked with the model a request for
	// that route would send: the channel's own spelling when it has one, else the
	// route's mapping target, else the exposed name itself.
	if exactModelName(route.ModelPattern) {
		model := strings.TrimSpace(router.ActualModel(strings.TrimSpace(route.ModelPattern), route, channel))
		if exactModelName(model) {
			return model
		}
		return ""
	}
	// A glob route names no model of its own, so the channel's spelling is the only
	// concrete name there is. Sending the glob would ask the upstream about a model
	// it has never heard of.
	if exactModelName(channel.SourceModel) {
		return strings.TrimSpace(channel.SourceModel)
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
