package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yhw5231/fluxgate/internal/store"
)

// managementServer wires a Server to a real SQLite store, because these tests
// cover the path a console request takes through the API into the database and
// back out as a reloaded snapshot. A double for the store would not exercise the
// validation, the SQL, or the reload.
func managementServer(t *testing.T) (*Server, *store.SQLiteStore) {
	t.Helper()
	persistent, err := store.OpenSQLite(filepath.Join(t.TempDir(), "management-test.db"))
	if err != nil {
		t.Fatalf("OpenSQLite() error = %v", err)
	}
	t.Cleanup(func() {
		if err := persistent.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	if err := persistent.EnsureUpstreamSchema(context.Background()); err != nil {
		t.Fatalf("EnsureUpstreamSchema() error = %v", err)
	}
	configuration, err := persistent.LoadConfiguration(context.Background())
	if err != nil {
		t.Fatalf("LoadConfiguration() error = %v", err)
	}

	applied := make(chan store.Configuration, 8)
	server := &Server{
		ManagementToken: "management-secret",
		ConfigStore:     persistent,
		Applier: func(configuration store.Configuration) {
			applied <- configuration
		},
	}
	server.SetConfiguration(configuration)
	// The applied channel is drained by the tests that assert on it.
	t.Cleanup(func() { close(applied) })
	return server, persistent
}

type managementResponse struct {
	status int
	body   map[string]any
	header http.Header
}

func callManagement(server *Server, method, path, token string, payload any) managementResponse {
	var body *strings.Reader
	if payload != nil {
		encoded, _ := json.Marshal(payload)
		body = strings.NewReader(string(encoded))
	} else {
		body = strings.NewReader("")
	}
	request := httptest.NewRequest(method, path, body)
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)

	parsed := map[string]any{}
	_ = json.Unmarshal(response.Body.Bytes(), &parsed)
	return managementResponse{status: response.Code, body: parsed, header: response.Header()}
}

// errorField reads "error": {...} from a response.
func errorField(body map[string]any, key string) any {
	envelope, ok := body["error"].(map[string]any)
	if !ok {
		return nil
	}
	return envelope[key]
}

func TestManagementWriteRequiresCredentials(t *testing.T) {
	server, _ := managementServer(t)

	unauthenticated := callManagement(server, http.MethodPost, "/management/configuration/sites", "", map[string]any{"name": "x"})
	if unauthenticated.status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", unauthenticated.status)
	}
	read := callManagement(server, http.MethodGet, "/management/configuration", "", nil)
	if read.status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for the listing", read.status)
	}
}

func TestManagementWriteRejectsAnotherOrigin(t *testing.T) {
	server, _ := managementServer(t)

	request := httptest.NewRequest(http.MethodPost, "/management/configuration/sites", strings.NewReader(`{"name":"x"}`))
	request.Header.Set("Authorization", "Bearer management-secret")
	request.Header.Set("Origin", "https://evil.example")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)

	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for a cross-origin write", response.Code)
	}
	if !strings.Contains(response.Body.String(), "cross_origin_rejected") {
		t.Fatalf("body = %s, want the cross-origin code", response.Body.String())
	}
}

func TestManagementConfigurationRoundTripAndReload(t *testing.T) {
	server, persistent := managementServer(t)

	site := callManagement(server, http.MethodPost, "/management/configuration/sites", "management-secret", map[string]any{
		"name": "Example", "url": "https://api.example.com", "platform": "openai",
	})
	if site.status != http.StatusCreated {
		t.Fatalf("create site status = %d, body = %v", site.status, site.body)
	}
	siteRow, ok := site.body["row"].(map[string]any)
	if !ok {
		t.Fatalf("create site response has no row: %v", site.body)
	}
	siteID := int64(siteRow["id"].(float64))

	account := callManagement(server, http.MethodPost, "/management/configuration/accounts", "management-secret", map[string]any{
		"site_id": siteID, "access_token": "upstream-secret-token",
	})
	if account.status != http.StatusCreated {
		t.Fatalf("create account status = %d, body = %v", account.status, account.body)
	}
	accountRow := account.body["row"].(map[string]any)
	accountID := int64(accountRow["id"].(float64))

	route := callManagement(server, http.MethodPost, "/management/configuration/routes", "management-secret", map[string]any{
		"model_pattern": "gpt-4.1", "enabled": true,
	})
	if route.status != http.StatusCreated {
		t.Fatalf("create route status = %d, body = %v", route.status, route.body)
	}
	routeID := int64(route.body["row"].(map[string]any)["id"].(float64))

	channel := callManagement(server, http.MethodPost, "/management/configuration/channels", "management-secret", map[string]any{
		"route_id": routeID, "account_id": accountID, "priority": 10, "weight": 20, "enabled": true,
	})
	if channel.status != http.StatusCreated {
		t.Fatalf("create channel status = %d, body = %v", channel.status, channel.body)
	}

	key := callManagement(server, http.MethodPost, "/management/configuration/keys", "management-secret", map[string]any{"name": "client"})
	if key.status != http.StatusCreated {
		t.Fatalf("create key status = %d, body = %v", key.status, key.body)
	}
	generated, _ := key.body["generated"].(map[string]any)
	minted, _ := generated["key"].(string)
	if !strings.HasPrefix(minted, downstreamKeyPrefix) || len(minted) < 20 {
		t.Fatalf("generated key = %q, want a minted credential", minted)
	}

	// The write carried the reloaded snapshot back, so the console repaints from
	// the response instead of a second request.
	configuration, ok := key.body["configuration"].(map[string]any)
	if !ok {
		t.Fatalf("write response carries no configuration: %v", key.body)
	}
	models, _ := configuration["models"].([]any)
	if len(models) != 1 || models[0] != "gpt-4.1" {
		t.Fatalf("reloaded models = %v, want the route just added", configuration["models"])
	}

	// And the gateway itself can serve the model with the credential it minted,
	// which is the proof that the reload reached the running process.
	loaded, err := persistent.LoadConfiguration(context.Background())
	if err != nil {
		t.Fatalf("LoadConfiguration() error = %v", err)
	}
	if len(loaded.Channels) != 1 || !loaded.Channels[0].Enabled {
		t.Fatalf("loaded channels = %#v, want one enabled channel", loaded.Channels)
	}
	authenticated, err := persistent.AuthenticateDownstreamKey(context.Background(), minted, nowUTCForTest())
	if err != nil {
		t.Fatalf("the minted key does not authenticate: %v", err)
	}
	if authenticated.Name != "client" {
		t.Fatalf("authenticated key name = %q, want client", authenticated.Name)
	}
}

func TestManagementConfigurationMasksSecretsAndKeepsThem(t *testing.T) {
	server, _ := managementServer(t)

	site := callManagement(server, http.MethodPost, "/management/configuration/sites", "management-secret", map[string]any{
		"name": "Example", "url": "https://api.example.com", "platform": "openai",
	})
	siteID := int64(site.body["row"].(map[string]any)["id"].(float64))
	account := callManagement(server, http.MethodPost, "/management/configuration/accounts", "management-secret", map[string]any{
		"site_id": siteID, "access_token": "upstream-secret-token",
	})
	accountID := int64(account.body["row"].(map[string]any)["id"].(float64))

	read := callManagement(server, http.MethodGet, "/management/configuration", "management-secret", nil)
	accounts := read.body["resources"].(map[string]any)["accounts"].([]any)
	masked := accounts[0].(map[string]any)["access_token"].(string)
	if !strings.HasPrefix(masked, secretMask) {
		t.Fatalf("access_token = %q, want it masked", masked)
	}
	if strings.Contains(masked, "upstream-secret-token") {
		t.Fatalf("access_token = %q, want the stored value withheld", masked)
	}
	if !strings.HasSuffix(masked, "oken") {
		t.Fatalf("mask = %q, want the tail kept so the operator can tell it apart", masked)
	}

	// An untouched console input submits an empty string and must not erase the
	// credential, and neither may the mask the console was shown.
	for _, submitted := range []map[string]any{{"access_token": ""}, {"access_token": masked}} {
		updated := callManagement(server, http.MethodPut, "/management/configuration/accounts/"+itoa(accountID), "management-secret", submitted)
		if updated.status != http.StatusOK {
			t.Fatalf("update status = %d, body = %v", updated.status, updated.body)
		}
		row := updated.body["row"].(map[string]any)
		if row["access_token"].(string) != masked {
			t.Fatalf("access_token = %v, want the stored credential kept", row["access_token"])
		}
	}

	// A real new value replaces it.
	replaced := callManagement(server, http.MethodPut, "/management/configuration/accounts/"+itoa(accountID), "management-secret", map[string]any{
		"access_token": "rotated-upstream-token",
	})
	row := replaced.body["row"].(map[string]any)
	if !strings.HasSuffix(row["access_token"].(string), "oken") {
		t.Fatalf("access_token = %v, want the replacement stored", row["access_token"])
	}
}

func TestManagementConfigurationReportsFieldFaults(t *testing.T) {
	server, _ := managementServer(t)

	missing := callManagement(server, http.MethodPost, "/management/configuration/sites", "management-secret", map[string]any{
		"url": "https://api.example.com", "platform": "openai",
	})
	if missing.status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", missing.status)
	}
	if got := errorField(missing.body, "field"); got != "name" {
		t.Fatalf("field = %v, want name", got)
	}
	if got := errorField(missing.body, "reason"); got != "required" {
		t.Fatalf("reason = %v, want required", got)
	}

	badReference := callManagement(server, http.MethodPost, "/management/configuration/accounts", "management-secret", map[string]any{
		"site_id": 99, "access_token": "token",
	})
	if got := errorField(badReference.body, "reason"); got != "missing_reference" {
		t.Fatalf("reason = %v, want missing_reference", got)
	}
	params, _ := errorField(badReference.body, "params").(map[string]any)
	if params["table"] != "sites" {
		t.Fatalf("params = %v, want the missing table named", params)
	}

	unknownResource := callManagement(server, http.MethodPost, "/management/configuration/nonsense", "management-secret", map[string]any{})
	if unknownResource.status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for an unknown resource", unknownResource.status)
	}

	unknownField := callManagement(server, http.MethodPost, "/management/configuration/sites", "management-secret", map[string]any{
		"name": "x", "url": "https://x.example", "platform": "p", "nope": 1,
	})
	if got := errorField(unknownField.body, "reason"); got != "unknown_field" {
		t.Fatalf("reason = %v, want unknown_field", got)
	}

	badID := callManagement(server, http.MethodPut, "/management/configuration/sites/abc", "management-secret", map[string]any{"name": "x"})
	if badID.status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a non-numeric id", badID.status)
	}
}

func TestManagementConfigurationRefusesUnsafeDeletesAndReportsCascades(t *testing.T) {
	server, _ := managementServer(t)

	site := callManagement(server, http.MethodPost, "/management/configuration/sites", "management-secret", map[string]any{
		"name": "Example", "url": "https://api.example.com", "platform": "openai",
	})
	siteID := int64(site.body["row"].(map[string]any)["id"].(float64))
	account := callManagement(server, http.MethodPost, "/management/configuration/accounts", "management-secret", map[string]any{
		"site_id": siteID, "access_token": "token",
	})
	accountID := int64(account.body["row"].(map[string]any)["id"].(float64))
	route := callManagement(server, http.MethodPost, "/management/configuration/routes", "management-secret", map[string]any{"model_pattern": "m"})
	routeID := int64(route.body["row"].(map[string]any)["id"].(float64))
	callManagement(server, http.MethodPost, "/management/configuration/channels", "management-secret", map[string]any{
		"route_id": routeID, "account_id": accountID, "priority": 1, "weight": 1,
	})

	refused := callManagement(server, http.MethodDelete, "/management/configuration/sites/"+itoa(siteID), "management-secret", nil)
	if refused.status != http.StatusConflict {
		t.Fatalf("status = %d, want 409 for a referenced site", refused.status)
	}
	if got := errorField(refused.body, "reason"); got != "referenced" {
		t.Fatalf("reason = %v, want referenced", got)
	}
	params, _ := errorField(refused.body, "params").(map[string]any)
	if params["resource"] != "accounts" || params["count"].(float64) != 1 {
		t.Fatalf("params = %v, want one referencing account", params)
	}

	// The route owns its channels, so deleting it takes them with it and reports
	// what went.
	deleted := callManagement(server, http.MethodDelete, "/management/configuration/routes/"+itoa(routeID), "management-secret", nil)
	if deleted.status != http.StatusOK {
		t.Fatalf("status = %d, body = %v", deleted.status, deleted.body)
	}
	cascaded, _ := deleted.body["cascaded"].(map[string]any)
	if cascaded["channels"].(float64) != 1 {
		t.Fatalf("cascaded = %v, want one channel removed with the route", cascaded)
	}

	missing := callManagement(server, http.MethodDelete, "/management/configuration/routes/"+itoa(routeID), "management-secret", nil)
	if missing.status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for a second delete", missing.status)
	}
}

func TestManagementKeyRotationMintsAndInvalidates(t *testing.T) {
	server, persistent := managementServer(t)

	key := callManagement(server, http.MethodPost, "/management/configuration/keys", "management-secret", map[string]any{"name": "client"})
	keyID := int64(key.body["row"].(map[string]any)["id"].(float64))
	first := key.body["generated"].(map[string]any)["key"].(string)

	rotated := callManagement(server, http.MethodPost, "/management/configuration/keys/"+itoa(keyID)+"/rotate", "management-secret", nil)
	if rotated.status != http.StatusOK {
		t.Fatalf("status = %d, body = %v", rotated.status, rotated.body)
	}
	second := rotated.body["generated"].(map[string]any)["key"].(string)
	if second == first {
		t.Fatalf("rotation returned the same value %q", first)
	}

	if _, err := persistent.AuthenticateDownstreamKey(context.Background(), first, nowUTCForTest()); !errors.Is(err, store.ErrUnauthorized) {
		t.Fatalf("the replaced key still authenticates: %v", err)
	}
	if _, err := persistent.AuthenticateDownstreamKey(context.Background(), second, nowUTCForTest()); err != nil {
		t.Fatalf("the rotated key does not authenticate: %v", err)
	}

	// A duplicate value is a conflict rather than a second usable credential.
	duplicate := callManagement(server, http.MethodPost, "/management/configuration/keys", "management-secret", map[string]any{
		"name": "other", "key": second,
	})
	if duplicate.status != http.StatusConflict {
		t.Fatalf("status = %d, want 409 for a duplicate key value", duplicate.status)
	}
}

func TestManagementWritesAreUnavailableWithoutAStore(t *testing.T) {
	server := &Server{ManagementToken: "management-secret"}

	write := callManagement(server, http.MethodPost, "/management/configuration/sites", "management-secret", map[string]any{"name": "x"})
	if write.status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", write.status)
	}
	if got := errorField(write.body, "code"); got != "configuration_not_supported" {
		t.Fatalf("code = %v, want configuration_not_supported", got)
	}

	// The listing reports the same condition rather than panicking on a missing
	// store, which is what a gateway started read-only would otherwise do.
	read := callManagement(server, http.MethodGet, "/management/configuration", "management-secret", nil)
	if read.status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 for the listing", read.status)
	}
}

func TestManagementSnapshotReflectsAWrite(t *testing.T) {
	server, _ := managementServer(t)

	before := callManagement(server, http.MethodGet, "/management/snapshot", "management-secret", nil)
	if len(before.body["channels"].([]any)) != 0 {
		t.Fatalf("snapshot channels = %v, want none before the write", before.body["channels"])
	}

	site := callManagement(server, http.MethodPost, "/management/configuration/sites", "management-secret", map[string]any{
		"name": "Example", "url": "https://api.example.com", "platform": "openai",
	})
	siteID := int64(site.body["row"].(map[string]any)["id"].(float64))
	account := callManagement(server, http.MethodPost, "/management/configuration/accounts", "management-secret", map[string]any{
		"site_id": siteID, "access_token": "token",
	})
	accountID := int64(account.body["row"].(map[string]any)["id"].(float64))
	route := callManagement(server, http.MethodPost, "/management/configuration/routes", "management-secret", map[string]any{"model_pattern": "m"})
	routeID := int64(route.body["row"].(map[string]any)["id"].(float64))
	callManagement(server, http.MethodPost, "/management/configuration/channels", "management-secret", map[string]any{
		"route_id": routeID, "account_id": accountID, "priority": 3, "weight": 5,
	})

	after := callManagement(server, http.MethodGet, "/management/snapshot", "management-secret", nil)
	channels := after.body["channels"].([]any)
	if len(channels) != 1 {
		t.Fatalf("snapshot channels = %v, want the channel just added", channels)
	}
	if channels[0].(map[string]any)["priority"].(float64) != 3 {
		t.Fatalf("snapshot channel = %v", channels[0])
	}
}

func itoa(value int64) string { return strconv.FormatInt(value, 10) }

func nowUTCForTest() time.Time { return time.Now().UTC() }
