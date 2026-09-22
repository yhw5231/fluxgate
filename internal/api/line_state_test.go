package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/yhw5231/fluxgate/internal/breaker"
)

// The console renders one line per upstream key and has to say whether that line
// is usable, cooling down, or held out of rotation by its configuration. The
// circuit behind a cooling line is filed under the upstream credential, which the
// browser is never given, so the gateway resolves the state per line and reports
// only the state — never what it is filed under.

// staticCircuits reports a fixed set of recorded circuits.
type staticCircuits map[breaker.Scope]breaker.State

func (c staticCircuits) Snapshot() map[breaker.Scope]breaker.State { return c }

// recordingReset stands in for the store's reset, capturing the circuits a
// request asked to clear.
type recordingReset struct{ cleared []breaker.Scope }

func (r *recordingReset) ResetBreakerStates(_ context.Context, scopes []breaker.Scope) (int64, error) {
	r.cleared = append(r.cleared, scopes...)
	return int64(len(scopes)), nil
}

// addUpstream creates one upstream through the console's own write path, so the
// fixture arrives the way an operator's would: an upstream with one key serving
// one model, which the gateway turns into one route and one line. The preferred
// key mode makes the line run under a circuit filed by credential.
func addUpstream(t *testing.T, server *Server, name, model, credential string) int64 {
	t.Helper()
	response := callManagement(server, http.MethodPost, "/management/configuration/upstreams", "management-secret", map[string]any{
		"name":          name,
		"url":           "https://" + name + ".example.com/v1",
		"keys":          []string{credential},
		"models":        []string{model},
		"key_mode":      "available_first",
		"global_weight": 1,
	})
	if response.status != http.StatusCreated {
		t.Fatalf("create upstream %s: status = %d, body = %v", name, response.status, response.body)
	}
	row, _ := response.body["row"].(map[string]any)
	siteID, _ := row["id"].(float64)
	return int64(siteID)
}

// snapshotChannelByName returns the snapshot entry of an upstream's only line.
func snapshotChannelByName(t *testing.T, server *Server, name string) map[string]any {
	t.Helper()
	response := callManagement(server, http.MethodGet, "/management/snapshot", "management-secret", nil)
	if response.status != http.StatusOK {
		t.Fatalf("snapshot status = %d, body = %v", response.status, response.body)
	}
	channels, _ := response.body["channels"].([]any)
	for _, entry := range channels {
		channel, _ := entry.(map[string]any)
		if channel["name"] == name {
			return channel
		}
	}
	t.Fatalf("no line of upstream %q in the snapshot: %v", name, channels)
	return nil
}

// channelState reads the state block of a snapshot channel.
func channelState(t *testing.T, channel map[string]any) map[string]any {
	t.Helper()
	state, ok := channel["state"].(map[string]any)
	if !ok {
		t.Fatalf("line %v carries no state: %v", channel["id"], channel)
	}
	return state
}

func TestSnapshotReportsACoolingLineByLine(t *testing.T) {
	server, _ := managementServer(t)
	addUpstream(t, server, "alpha", "gpt-4o", "sk-alpha-key")
	addUpstream(t, server, "beta", "gpt-4o", "sk-beta-key")

	blockedUntil := time.Now().Add(15 * time.Minute).UTC()
	server.BreakerSnapshotter = staticCircuits{
		{KeyID: "sk-alpha-key"}: {CooldownLevel: 2, BlockedUntil: blockedUntil},
	}

	alpha := snapshotChannelByName(t, server, "alpha")
	state := channelState(t, alpha)
	if state["status"] != "cooling" {
		t.Errorf("status = %v, want cooling", state["status"])
	}
	if state["scope"] != "key" {
		t.Errorf("scope = %v, want the shared key circuit", state["scope"])
	}
	if state["cooldown_level"] != float64(2) {
		t.Errorf("cooldown_level = %v, want 2", state["cooldown_level"])
	}
	// The deadline travels as a timestamp, because the console counts it down.
	deadline, _ := state["blocked_until"].(string)
	parsed, err := time.Parse(time.RFC3339Nano, deadline)
	if err != nil {
		t.Fatalf("blocked_until = %q, want a timestamp: %v", deadline, err)
	}
	if !parsed.Equal(blockedUntil) {
		t.Errorf("blocked_until = %v, want %v", parsed, blockedUntil)
	}
	// The circuit is filed under the credential, and the credential must not
	// travel with the state that describes it.
	encoded, err := json.Marshal(alpha)
	if err != nil {
		t.Fatalf("marshal channel: %v", err)
	}
	if strings.Contains(string(encoded), "sk-alpha-key") {
		t.Errorf("the line's snapshot carries the credential: %s", encoded)
	}

	// A different upstream is unaffected by another one's circuit.
	if got := channelState(t, snapshotChannelByName(t, server, "beta"))["status"]; got != "ready" {
		t.Errorf("the other upstream's line = %v, want ready", got)
	}
}

// A line the configuration holds out of rotation says so, whatever a circuit
// says: bringing it back is an edit, not a recovery.
func TestSnapshotReportsALineItsConfigurationDisables(t *testing.T) {
	server, _ := managementServer(t)
	siteID := addUpstream(t, server, "alpha", "gpt-4o", "sk-alpha-key")

	response := callManagement(server, http.MethodPut, "/management/configuration/upstreams/"+itoa(siteID), "management-secret", map[string]any{
		"status": "disabled",
	})
	if response.status != http.StatusOK {
		t.Fatalf("disable upstream: status = %d, body = %v", response.status, response.body)
	}
	server.BreakerSnapshotter = staticCircuits{
		{KeyID: "sk-alpha-key"}: {BlockedUntil: time.Now().Add(time.Hour)},
	}

	line := snapshotChannelByName(t, server, "alpha")
	if line["enabled"] != false {
		t.Fatalf("enabled = %v, want false for a disabled upstream", line["enabled"])
	}
	if got := channelState(t, line)["status"]; got != "inactive" {
		t.Errorf("status = %v, want inactive", got)
	}
}

// A line can be held by two circuits at once — its own and the one its credential
// shares — and the view reports the one that releases last.
func TestSnapshotReportsTheCircuitThatHoldsALineLongest(t *testing.T) {
	server, _ := managementServer(t)
	addUpstream(t, server, "alpha", "gpt-4o", "sk-alpha-key")

	line := snapshotChannelByName(t, server, "alpha")
	channelID, _ := line["id"].(string)
	sooner := time.Now().Add(time.Minute).UTC()
	later := time.Now().Add(time.Hour).UTC()
	server.BreakerSnapshotter = staticCircuits{
		{ChannelID: channelID}:  {BlockedUntil: sooner},
		{KeyID: "sk-alpha-key"}: {BlockedUntil: later, CooldownLevel: 3},
	}

	state := channelState(t, snapshotChannelByName(t, server, "alpha"))
	if state["scope"] != "key" {
		t.Errorf("scope = %v, want the circuit that releases last", state["scope"])
	}
	if state["cooldown_level"] != float64(3) {
		t.Errorf("cooldown_level = %v, want 3", state["cooldown_level"])
	}
	if deadline, _ := state["blocked_until"].(string); deadline != later.Format(time.RFC3339Nano) {
		t.Errorf("blocked_until = %q, want %v", deadline, later.Format(time.RFC3339Nano))
	}
}

// Clearing a line clears every circuit that can hold it, so an operator who has
// fixed an upstream does not have to know which circuit the failure landed in.
func TestBreakerResetClearsEveryCircuitOfALine(t *testing.T) {
	server, _ := managementServer(t)
	addUpstream(t, server, "alpha", "gpt-4o", "sk-alpha-key")
	channelID, _ := snapshotChannelByName(t, server, "alpha")["id"].(string)

	resetter := &recordingReset{}
	server.BreakerReset = resetter
	server.BreakerSnapshotter = staticCircuits{
		{KeyID: "sk-alpha-key"}:                       {BlockedUntil: time.Now().Add(time.Hour)},
		{KeyID: "sk-alpha-key", Model: "gpt-4o-mini"}: {},
	}

	response := callManagement(server, http.MethodPost, "/management/breakers/reset", "management-secret", map[string]any{
		"scope": "channel", "channel_id": channelID, "model": "gpt-4o",
	})
	if response.status != http.StatusOK {
		t.Fatalf("reset status = %d, body = %v", response.status, response.body)
	}

	want := []breaker.Scope{
		{ChannelID: channelID},
		{KeyID: "sk-alpha-key"},
		{KeyID: "sk-alpha-key", Model: "gpt-4o"},
		{KeyID: "sk-alpha-key", Model: "gpt-4o-mini"},
	}
	if len(resetter.cleared) != len(want) {
		t.Fatalf("cleared = %v, want %v", resetter.cleared, want)
	}
	for _, scope := range want {
		if indexOfScope(resetter.cleared, scope) == -1 {
			t.Errorf("cleared = %v, want it to contain %v", resetter.cleared, scope)
		}
	}
}

// A line the configuration does not know is still addressed by its own circuit,
// which is what a circuit recorded before a configuration change needs.
func TestBreakerResetOfAnUnknownLineClearsOnlyThatLine(t *testing.T) {
	server, _ := managementServer(t)
	resetter := &recordingReset{}
	server.BreakerReset = resetter

	response := callManagement(server, http.MethodPost, "/management/breakers/reset", "management-secret", map[string]any{
		"scope": "channel", "channel_id": "404",
	})
	if response.status != http.StatusOK {
		t.Fatalf("reset status = %d, body = %v", response.status, response.body)
	}
	if len(resetter.cleared) != 1 || resetter.cleared[0] != (breaker.Scope{ChannelID: "404"}) {
		t.Errorf("cleared = %v, want the named channel alone", resetter.cleared)
	}
}

func indexOfScope(scopes []breaker.Scope, wanted breaker.Scope) int {
	for index, scope := range scopes {
		if scope == wanted {
			return index
		}
	}
	return -1
}

// A circuit filed under a credential, and the recovery view that lists circuits,
// must not hand the browser the credential: the whole point of resolving the
// state per line is that the gateway is the only holder of it.
func TestSnapshotNeverCarriesTheCredential(t *testing.T) {
	server, _ := managementServer(t)
	addUpstream(t, server, "alpha", "gpt-4o", "sk-alpha-secret")
	line := snapshotChannelByName(t, server, "alpha")
	channelID, _ := line["id"].(string)

	server.BreakerSnapshotter = staticCircuits{
		{KeyID: "sk-alpha-secret"}:                  {CooldownLevel: 1, BlockedUntil: time.Now().Add(time.Hour)},
		{KeyID: "sk-alpha-secret", Model: "gpt-4o"}: {CooldownLevel: 2, BlockedUntil: time.Now().Add(time.Hour)},
		{ChannelID: channelID}:                      {ConsecutiveFailures: 2},
	}

	response := callManagement(server, http.MethodGet, "/management/snapshot", "management-secret", nil)
	if response.status != http.StatusOK {
		t.Fatalf("snapshot status = %d, body = %v", response.status, response.body)
	}
	encoded, err := json.Marshal(response.body)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	if strings.Contains(string(encoded), "sk-alpha-secret") {
		t.Fatalf("the snapshot carries the upstream credential: %s", encoded)
	}

	// The circuit the credential was filed under names the lines it holds instead,
	// which is what an operator needs to act on it.
	breakers, _ := response.body["breakers"].([]any)
	if len(breakers) != 3 {
		t.Fatalf("breakers = %d, want 3", len(breakers))
	}
	named := 0
	for _, entry := range breakers {
		breaker, _ := entry.(map[string]any)
		if breaker["key_id"] != nil {
			t.Errorf("breaker entry carries a key_id: %v", breaker)
		}
		lines, _ := breaker["lines"].([]any)
		if len(lines) == 0 {
			continue
		}
		named++
		if lines[0] != "alpha" {
			t.Errorf("lines = %v, want the line presenting the credential", lines)
		}
	}
	if named != 2 {
		t.Errorf("breaker entries naming a line = %d, want the two credential-scoped circuits", named)
	}
}
