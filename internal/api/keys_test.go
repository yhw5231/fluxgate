package api

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A client key is the credential the operator hands out, so the console can show
// one back: the listing carries only the mask, and one deliberate read returns
// the stored value.
func TestRevealingAClientKeyAnswersWithTheWorkingCredential(t *testing.T) {
	server, persistent := managementServer(t)
	created := callManagement(server, http.MethodPost, "/management/configuration/keys", "management-secret", map[string]any{
		"name": "caller",
	})
	if created.status != http.StatusCreated {
		t.Fatalf("create key: status = %d, body = %v", created.status, created.body)
	}
	row, _ := created.body["row"].(map[string]any)
	id, _ := row["id"].(float64)
	generated, _ := created.body["generated"].(map[string]any)
	value, _ := generated["key"].(string)
	if value == "" {
		t.Fatalf("create key returned no value: %v", created.body)
	}

	// The listing the console paints from never carries the value.
	listing := callManagement(server, http.MethodGet, "/management/configuration", "management-secret", nil)
	encoded := fmt.Sprintf("%v", listing.body)
	if strings.Contains(encoded, value) {
		t.Fatalf("the configuration listing carries the client key")
	}

	var logged bytes.Buffer
	server.Logger = slog.New(slog.NewJSONHandler(&logged, nil))

	revealed := callManagement(server, http.MethodPost,
		fmt.Sprintf("/management/configuration/keys/%d/reveal", int64(id)), "management-secret", nil)
	if revealed.status != http.StatusOK {
		t.Fatalf("reveal: status = %d, body = %v", revealed.status, revealed.body)
	}
	if got, _ := revealed.body["key"].(string); got != value {
		t.Errorf("revealed key = %q, want the stored value", got)
	}

	// The value it answered with is the credential the gateway authenticates, not
	// a copy that happens to look right.
	if _, err := persistent.AuthenticateDownstreamKey(context.Background(), value, time.Now().UTC()); err != nil {
		t.Errorf("the revealed value does not authenticate: %v", err)
	}

	// Reading a credential is audited by row and client, never by value.
	if !strings.Contains(logged.String(), "console_key_revealed") {
		t.Errorf("reveal was not audited: %s", logged.String())
	}
	if strings.Contains(logged.String(), value) {
		t.Errorf("the audit event carries the credential: %s", logged.String())
	}
}

// Reading a credential back is gated like a write: a console credential, a
// configured store, and the gateway's own origin.
func TestRevealingAClientKeyRequiresACredentialAndTheConsoleOrigin(t *testing.T) {
	server, _ := managementServer(t)
	created := callManagement(server, http.MethodPost, "/management/configuration/keys", "management-secret", map[string]any{
		"name": "caller",
	})
	row, _ := created.body["row"].(map[string]any)
	id, _ := row["id"].(float64)

	unauthenticated := callManagement(server, http.MethodPost,
		fmt.Sprintf("/management/configuration/keys/%d/reveal", int64(id)), "", nil)
	if unauthenticated.status != http.StatusUnauthorized {
		t.Errorf("reveal without credentials: status = %d, want 401", unauthenticated.status)
	}

	request := httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/management/configuration/keys/%d/reveal", int64(id)), strings.NewReader(""))
	request.Header.Set("Authorization", "Bearer management-secret")
	request.Header.Set("Origin", "https://somewhere.else")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Errorf("reveal from another origin: status = %d, want 403", response.Code)
	}
}

func TestRevealingAnUnknownClientKeyIsNotFound(t *testing.T) {
	server, _ := managementServer(t)
	response := callManagement(server, http.MethodPost, "/management/configuration/keys/404/reveal", "management-secret", nil)
	if response.status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", response.status)
	}
	if response.body["error"] == nil {
		t.Errorf("body = %v, want an error payload", response.body)
	}
}
