package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/yhw5231/fluxgate/internal/breaker"
	"github.com/yhw5231/fluxgate/internal/store"
)

// The SQLite loader includes inactive lines: a disabled upstream still
// needs a label, including for a credential no longer presented by its lines.
func TestSnapshotNamesDisabledUpstreamsAndTheirHistoricalCircuits(t *testing.T) {
	server, _ := managementServer(t)
	siteID := addUpstream(t, server, "alpha", "gpt-4o", "current-secret")
	response := callManagement(server, http.MethodPut, "/management/configuration/upstreams/"+itoa(siteID), "management-secret", map[string]any{
		"status": "disabled",
	})
	if response.status != http.StatusOK {
		t.Fatalf("disable_upstream_status=%d", response.status)
	}
	server.BreakerSnapshotter = staticCircuits{
		{SiteID: siteID, KeyID: "current-secret"}:                  {ConsecutiveFailures: 1},
		{SiteID: siteID, KeyID: "retired-secret", Model: "gpt-4o"}: {Disabled: true},
	}
	response = callManagement(server, http.MethodGet, "/management/snapshot", "management-secret", nil)
	if response.status != http.StatusOK {
		t.Fatalf("snapshot_status=%d", response.status)
	}
	entries, _ := response.body["breakers"].([]any)
	if len(entries) != 2 {
		t.Fatalf("breakers=%d,want_2", len(entries))
	}
	for _, value := range entries {
		entry, _ := value.(map[string]any)
		if entry["site_name"] != "alpha" || entry["site_id"] != float64(siteID) {
			t.Errorf("disabled_upstream_label_or_identity_changed:%v", entry)
		}
		if entry["scope"] == "key" {
			names, _ := entry["lines"].([]any)
			if len(names) != 1 || names[0] != "alpha" {
				t.Errorf("disabled_current_line=%v,want_alpha", names)
			}
		} else if entry["lines"] != nil {
			t.Errorf("historical_circuit_invented_a_current_line:%v", entry["lines"])
		}
	}
}

// Deleting an upstream removes persisted circuits, but an adapter already
// serving traffic may still hold them. A read-only snapshot keeps these
// records without misattributing them to another upstream of the same key.
func TestSnapshotKeepsDeletedUpstreamCircuitsUnassociated(t *testing.T) {
	server, persistent := managementServer(t)
	alpha := addUpstream(t, server, "alpha", "gpt-4o", "shared-secret")
	beta := addUpstream(t, server, "beta", "gpt-4o", "shared-secret")
	ctx := context.Background()
	if err := persistent.EnsureBreakerSchema(ctx); err != nil {
		t.Fatal(err)
	}
	adapter, err := store.NewBreakerAdapter(ctx, persistent)
	if err != nil {
		t.Fatal(err)
	}
	scopes := []breaker.Scope{
		{SiteID: alpha, KeyID: "shared-secret"},
		{SiteID: alpha, KeyID: "shared-secret", Model: "gpt-4o"},
		{SiteID: beta, KeyID: "shared-secret"},
	}
	for _, scope := range scopes {
		adapter.Update(scope, func(breaker.State) breaker.State {
			return breaker.State{Disabled: true}
		})
	}
	server.BreakerSnapshotter = adapter
	response := callManagement(server, http.MethodDelete, "/management/configuration/upstreams/"+itoa(alpha), "management-secret", nil)
	if response.status != http.StatusOK {
		t.Fatalf("delete_upstream_status=%d", response.status)
	}
	persisted, err := persistent.LoadBreakerStates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(persisted) != 1 {
		t.Fatalf("persisted_circuits=%d,want_only_surviving_upstream", len(persisted))
	}
	response = callManagement(server, http.MethodGet, "/management/snapshot", "management-secret", nil)
	if response.status != http.StatusOK {
		t.Fatalf("snapshot_status=%d", response.status)
	}
	entries, _ := response.body["breakers"].([]any)
	if len(entries) != len(scopes) {
		t.Fatalf("runtime_circuits=%d,want_%d", len(entries), len(scopes))
	}
	for _, value := range entries {
		entry, _ := value.(map[string]any)
		if entry["site_id"] == float64(alpha) {
			if entry["site_name"] != nil || entry["lines"] != nil {
				t.Error("deleted_upstreams_circuit_borrowed_a_surviving_upstreams_label")
			}
		} else {
			names, _ := entry["lines"].([]any)
			if entry["site_id"] != float64(beta) || entry["site_name"] != "beta" || len(names) != 1 || names[0] != "beta" {
				t.Errorf("surviving_upstream_not_identified:%v", entry)
			}
		}
	}
	encoded, err := json.Marshal(response.body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "shared-secret") {
		t.Error("snapshot_leaked_an_orphaned_credential")
	}
	if len(adapter.Snapshot()) != len(scopes) {
		t.Error("read_only_snapshot_mutated_runtime_breaker_state")
	}
}
