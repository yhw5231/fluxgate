package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/yhw5231/fluxgate/internal/breaker"
	"github.com/yhw5231/fluxgate/internal/store"
)

// The browser addresses credential circuits by site, even when a historical
// key no longer matches a current line. Resetting that site must clear
// its exact runtime and persisted scopes without clearing another site.
func TestBreakerResetOfAnUpstreamClearsHistoricalCredentialScopes(t *testing.T) {
	for _, deleted := range []bool{false, true} {
		name := "replaced_key"
		if deleted {
			name = "deleted_upstream"
		}
		t.Run(name, func(t *testing.T) {
			server, persistent := managementServer(t)
			alpha := addUpstream(t, server, "alpha", "gpt-4o", "current-secret")
			beta := addUpstream(t, server, "beta", "gpt-4o", "retired-secret")
			ctx := context.Background()
			if err := persistent.EnsureBreakerSchema(ctx); err != nil {
				t.Fatal(err)
			}
			adapter, err := store.NewBreakerAdapter(ctx, persistent)
			if err != nil {
				t.Fatal(err)
			}
			server.BreakerSnapshotter = adapter
			server.BreakerReset = adapter
			historical := []breaker.Scope{
				{SiteID: alpha, KeyID: "retired-secret"},
				{SiteID: alpha, KeyID: "retired-secret", Model: "gpt-4o"},
			}
			untouched := []breaker.Scope{
				{SiteID: beta, KeyID: "retired-secret"},
				{SiteID: beta, KeyID: "retired-secret", Model: "gpt-4o"},
				{KeyID: "retired-secret"},
				{KeyID: "retired-secret", Model: "gpt-4o"},
			}
			for _, scope := range append(append([]breaker.Scope(nil), historical...), untouched...) {
				adapter.Update(scope, func(breaker.State) breaker.State {
					return breaker.State{Disabled: true}
				})
			}
			if deleted {
				response := callManagement(server, http.MethodDelete, "/management/configuration/upstreams/"+itoa(alpha), "management-secret", nil)
				if response.status != http.StatusOK {
					t.Fatalf("delete_upstream_status=%d", response.status)
				}
			}
			response := callManagement(server, http.MethodPost, "/management/breakers/reset", "management-secret", map[string]any{
				"scope": "site", "site_id": alpha,
			})
			if response.status != http.StatusOK {
				t.Fatalf("reset_status=%d", response.status)
			}
			wantRemoved := float64(len(historical))
			if deleted {
				// Deletion already removed the SQLite rows; reset still
				// needs to clear the adapter's in-memory copies.
				wantRemoved = 0
			}
			if response.body["reset"] != wantRemoved {
				t.Errorf("reset=%v,want_%v", response.body["reset"], wantRemoved)
			}
			persisted, err := persistent.LoadBreakerStates(ctx)
			if err != nil {
				t.Fatal(err)
			}
			runtime := adapter.Snapshot()
			for _, scope := range historical {
				if _, known := runtime[scope]; known {
					t.Error("the_reset_left_a_historical_runtime_circuit")
				}
				if _, known := persisted[scope]; known {
					t.Error("the_reset_left_a_historical_persisted_circuit")
				}
			}
			for _, scope := range untouched {
				if _, known := runtime[scope]; !known {
					t.Error("the_reset_cleared_another_site_or_legacy_global_runtime_circuit")
				}
				if _, known := persisted[scope]; !known {
					t.Error("the_reset_cleared_another_site_or_legacy_global_persisted_circuit")
				}
			}
		})
	}
}
