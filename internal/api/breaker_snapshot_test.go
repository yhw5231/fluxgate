package api

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/yhw5231/fluxgate/internal/breaker"
	"github.com/yhw5231/fluxgate/internal/domain"
	"github.com/yhw5231/fluxgate/internal/store"
)

// Attribution is display-only: both configuration views, site isolation,
// and unchanged reset identities including legacy and orphaned circuits.
func TestSnapshotMatchesCredentialCircuitsToCurrentLines(t *testing.T) {
	alpha := domain.Channel{ID: "alpha", Name: "alpha", SiteID: 1, APIKey: "shared-secret"}
	beta := domain.Channel{ID: "beta", Name: "beta", SiteID: 2, APIKey: "shared-secret"}
	flat := domain.Channel{ID: "flat", Name: "flat-only", SiteID: 3, APIKey: "flat-secret"}
	unnamed := domain.Channel{ID: "unnamed", Name: "\t", SiteID: 4, APIKey: "unnamed-secret"}
	configuration := store.Configuration{
		Channels: []domain.Channel{alpha, flat, unnamed},
		Routes: []domain.Route{{Channels: []domain.Channel{
			alpha, alpha, beta, unnamed,
		}}},
	}
	for _, model := range []string{"", "upstream-model"} {
		for _, tc := range []struct {
			name     string
			siteID   int64
			key      string
			want     []string
			wantSite string
		}{
			{name: "site_scoped_shared_credential", siteID: 1, key: "shared-secret", want: []string{"alpha"}, wantSite: "alpha"},
			{name: "another_site_with_the_same_credential", siteID: 2, key: "shared-secret", want: []string{"beta"}, wantSite: "beta"},
			{name: "flat_configuration_only", siteID: 3, key: "flat-secret", want: []string{"flat-only"}, wantSite: "flat-only"},
			{name: "unnamed_line", siteID: 4, key: "unnamed-secret", want: []string{"line\u0020unnamed"}},
			{name: "legacy_scope_without_upstream", key: "shared-secret", want: []string{"alpha", "beta"}},
			{name: "deleted_upstream_cannot_borrow_another_upstream", siteID: 99, key: "shared-secret"},
			{name: "replaced_credential_cannot_borrow_the_sites_current_line", siteID: 1, key: "retired-secret", wantSite: "alpha"},
			{name: "orphan_legacy_credential", key: "retired-secret"},
		} {
			name := "key/" + tc.name
			if model != "" {
				name = "key_model/" + tc.name
			}
			t.Run(name, func(t *testing.T) {
				scope := breaker.Scope{SiteID: tc.siteID, KeyID: tc.key, Model: model}
				server := &Server{ManagementToken: "management-secret", BreakerSnapshotter: staticCircuits{scope: {ConsecutiveFailures: 2}}}
				server.SetConfiguration(configuration)
				response := callManagement(server, http.MethodGet, "/management/snapshot", "management-secret", nil)
				if response.status != http.StatusOK {
					t.Fatalf("snapshot_status=%d", response.status)
				}
				entries, _ := response.body["breakers"].([]any)
				if len(entries) != 1 {
					t.Fatalf("breakers=%d,want_1", len(entries))
				}
				entry, _ := entries[0].(map[string]any)
				var got []string
				if names, ok := entry["lines"].([]any); ok {
					for _, name := range names {
						got = append(got, name.(string))
					}
				}
				if !reflect.DeepEqual(got, tc.want) {
					t.Errorf("lines=%v,want_%v", got, tc.want)
				}
				gotSite, _ := entry["site_name"].(string)
				if gotSite != tc.wantSite {
					t.Errorf("site_name=%q,want_%q", gotSite, tc.wantSite)
				}
				wantScope := "key"
				if model != "" {
					wantScope = "key_model"
				}
				if entry["scope"] != wantScope {
					t.Errorf("scope=%v,want_%s", entry["scope"], wantScope)
				}
				if entry["channel_id"] != nil {
					t.Errorf("credential_scope_gained_a_channel_reset_identity:%v", entry["channel_id"])
				}
				if tc.siteID == 0 {
					if entry["site_id"] != nil {
						t.Error("legacy_scope_gained_an_invented_upstream_reset_identity")
					}
				} else if entry["site_id"] != float64(tc.siteID) {
					t.Errorf("site_id=%v,want_original_%d", entry["site_id"], tc.siteID)
				}
				if model != "" && entry["model"] != model {
					t.Errorf("model=%v,want_%s", entry["model"], model)
				}
				encoded, err := json.Marshal(response.body)
				if err != nil {
					t.Fatal(err)
				}
				for _, secret := range []string{"shared-secret", "flat-secret", "unnamed-secret", "retired-secret"} {
					if strings.Contains(string(encoded), secret) {
						t.Error("snapshot_leaked_an_upstream_credential")
					}
				}
			})
		}
	}
}
