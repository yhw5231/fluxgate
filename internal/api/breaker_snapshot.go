package api

import (
	"sort"
	"strings"

	"github.com/yhw5231/fluxgate/internal/breaker"
	"github.com/yhw5231/fluxgate/internal/domain"
	"github.com/yhw5231/fluxgate/internal/store"
)

// breakerLineIndex resolves display labels without changing a circuit's identity.
// Only credential matches name current lines; a site name alone can still
// describe a historical circuit whose credential was replaced or removed.
// All credential keys stay inside the gateway.
type breakerLineIndex struct {
	linesByCredential map[string][]string
	legacyLinesByKey  map[string][]string
	siteNames         map[int64]string
}

func newBreakerLineIndex(configuration store.Configuration) breakerLineIndex {
	index := breakerLineIndex{
		linesByCredential: make(map[string][]string),
		legacyLinesByKey:  make(map[string][]string),
		siteNames:         make(map[int64]string),
	}
	add := func(channel domain.Channel) {
		name := strings.TrimSpace(channel.Name)
		if channel.SiteID > 0 && name != "" && index.siteNames[channel.SiteID] == "" {
			index.siteNames[channel.SiteID] = name
		}
		if channel.APIKey == "" {
			return
		}
		if name == "" {
			name = domain.LineLabel("", channel.ID)
		}
		key := channel.CredentialKey()
		if !containsString(index.linesByCredential[key], name) {
			index.linesByCredential[key] = append(index.linesByCredential[key], name)
		}
		if !containsString(index.legacyLinesByKey[channel.APIKey], name) {
			index.legacyLinesByKey[channel.APIKey] = append(index.legacyLinesByKey[channel.APIKey], name)
		}
	}
	// The flat list and route lists are two views of the same configuration.
	// Read both so embedders and imported snapshots need not populate both.
	// Disabled lines are included: disabling a line does not erase its owner.
	for _, channel := range configuration.Channels {
		add(channel)
	}
	for _, route := range configuration.Routes {
		for _, channel := range route.Channels {
			add(channel)
		}
	}
	return index
}

func (index breakerLineIndex) lines(scope breaker.Scope) []string {
	if scope.KeyID == "" {
		return nil
	}
	lines := index.linesByCredential[domain.CredentialKeyOf(scope.SiteID, scope.KeyID)]
	if scope.SiteID == 0 {
		// Old credential scopes were global across upstreams. Their display
		// may name all current presenters, but must not invent a site_id
		// or channel_id: either would change the browser's reset target.
		lines = index.legacyLinesByKey[scope.KeyID]
	}
	lines = append([]string(nil), lines...)
	sort.Strings(lines)
	return lines
}
