package store

import (
	"context"
	"testing"
)

// A site can name an endpoint that overrides its address for upstream requests.
// It is a column an operator can edit through the management API, so a gateway
// that ignored it would dispatch to an address the operator had replaced.
func TestLoadConfigurationHonorsAForcedUpstreamEndpoint(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	if err := store.EnsureUpstreamSchema(ctx); err != nil {
		t.Fatalf("EnsureUpstreamSchema() error = %v", err)
	}

	statements := []string{
		`INSERT INTO sites (id, name, url, platform, status, global_weight, forced_upstream_endpoint)
		 VALUES (1, 'relay', 'https://management.example.com', 'openai', 'active', 1, 'https://relay.example.com/openai/v1')`,
		`INSERT INTO sites (id, name, url, platform, status, global_weight)
		 VALUES (2, 'direct', 'https://api.example.com/v1', 'openai', 'active', 1)`,
		`INSERT INTO accounts (id, site_id, access_token, status) VALUES (10, 1, 'forced-key', 'active')`,
		`INSERT INTO accounts (id, site_id, access_token, status) VALUES (11, 2, 'plain-key', 'active')`,
		`INSERT INTO token_routes (id, model_pattern, routing_strategy, enabled) VALUES (30, 'gpt-4o', 'weighted', 1)`,
		`INSERT INTO route_channels (id, route_id, account_id, priority, weight, enabled) VALUES (40, 30, 10, 0, 10, 1)`,
		`INSERT INTO route_channels (id, route_id, account_id, priority, weight, enabled) VALUES (41, 30, 11, 0, 10, 1)`,
	}
	for _, statement := range statements {
		if _, err := store.db.Exec(statement); err != nil {
			t.Fatalf("execute test statement: %v", err)
		}
	}

	configuration, err := store.LoadConfiguration(ctx)
	if err != nil {
		t.Fatalf("LoadConfiguration() error = %v", err)
	}
	byID := map[string]string{}
	for _, channel := range configuration.Channels {
		byID[channel.ID] = channel.BaseURL
	}
	if got := byID["40"]; got != "https://relay.example.com/openai/v1" {
		t.Errorf("forced endpoint line base URL = %q, want the forced endpoint", got)
	}
	if got := byID["41"]; got != "https://api.example.com/v1" {
		t.Errorf("plain line base URL = %q, want the site address", got)
	}
}
