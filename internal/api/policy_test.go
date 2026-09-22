package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yhw5231/fluxgate/internal/breaker"
	"github.com/yhw5231/fluxgate/internal/policy"
	"github.com/yhw5231/fluxgate/internal/store"
	"github.com/yhw5231/fluxgate/internal/upstream"
)

// A console write to the runtime policy has to be stored, reported back as the
// value in force, and readable again the way a restart would read it.
func TestPolicyUpdateStoresReportsAndClearsValues(t *testing.T) {
	server, persistent := managementServer(t)

	response := callManagement(server, http.MethodPut, "/management/policy", "management-secret", map[string]any{
		"settings": map[string]any{
			policy.KeyRetryMaxAttempts:           4,
			policy.KeyRetryMaxAttemptsPerChannel: 2,
			policy.KeyBreakerBaseCooldownSeconds: 120,
			policy.KeyFailoverCrossUpstream:      false,
		},
	})
	if response.status != http.StatusOK {
		t.Fatalf("PUT /management/policy status = %d, body = %v", response.status, response.body)
	}

	stored, err := persistent.LoadSettings(context.Background())
	if err != nil {
		t.Fatalf("LoadSettings() error = %v", err)
	}
	if stored[policy.KeyRetryMaxAttempts] != "4" || stored[policy.KeyBreakerBaseCooldownSeconds] != "120" {
		t.Errorf("stored settings = %v, want the submitted values", stored)
	}

	applied := policyFromResponse(t, response)
	if value, ok := applied[policy.KeyRetryMaxAttempts].(float64); !ok || value != 4 {
		t.Errorf("policy.max_attempts = %v, want 4", applied[policy.KeyRetryMaxAttempts])
	}
	if value, ok := applied[policy.KeyFailoverCrossUpstream].(bool); !ok || value {
		t.Errorf("policy.failover.cross_upstream = %v, want false", applied[policy.KeyFailoverCrossUpstream])
	}

	fields, ok := response.body["configuration"].(map[string]any)["policy_fields"].([]any)
	if !ok || len(fields) == 0 {
		t.Fatalf("the configuration response carries no policy fields: %v", response.body["configuration"])
	}

	// Clearing a value removes the override instead of storing an empty one, so
	// the process default applies again.
	response = callManagement(server, http.MethodPut, "/management/policy", "management-secret", map[string]any{
		"settings": map[string]any{policy.KeyRetryMaxAttempts: nil},
	})
	if response.status != http.StatusOK {
		t.Fatalf("PUT /management/policy (clear) status = %d, body = %v", response.status, response.body)
	}
	stored, err = persistent.LoadSettings(context.Background())
	if err != nil {
		t.Fatalf("LoadSettings() error = %v", err)
	}
	if _, present := stored[policy.KeyRetryMaxAttempts]; present {
		t.Errorf("the cleared override is still stored: %v", stored[policy.KeyRetryMaxAttempts])
	}
	if value := policyFromResponse(t, response)[policy.KeyRetryMaxAttempts]; value != float64(policy.Default().Retry.MaxAttempts) {
		t.Errorf("policy.max_attempts = %v, want the default back", value)
	}
}

// A policy combination that cannot work is refused with the field to change, and
// nothing is stored.
func TestPolicyUpdateRejectsAContradiction(t *testing.T) {
	server, persistent := managementServer(t)
	response := callManagement(server, http.MethodPut, "/management/policy", "management-secret", map[string]any{
		"settings": map[string]any{
			policy.KeyRetryMaxAttempts:           2,
			policy.KeyRetryMaxAttemptsPerChannel: 5,
		},
	})
	if response.status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %v", response.status, response.body)
	}
	if field := errorField(response.body, "field"); field != policy.KeyRetryMaxAttemptsPerChannel {
		t.Errorf("error field = %v, want %s", field, policy.KeyRetryMaxAttemptsPerChannel)
	}
	stored, err := persistent.LoadSettings(context.Background())
	if err != nil {
		t.Fatalf("LoadSettings() error = %v", err)
	}
	if len(stored) != 0 {
		t.Errorf("a rejected policy was stored anyway: %v", stored)
	}

	// A value outside a field's own range is refused too.
	response = callManagement(server, http.MethodPut, "/management/policy", "management-secret", map[string]any{
		"settings": map[string]any{policy.KeyBreakerMode: "occasionally"},
	})
	if response.status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for an unknown mode; body = %v", response.status, response.body)
	}
	if field := errorField(response.body, "field"); field != policy.KeyBreakerMode {
		t.Errorf("error field = %v, want %s", field, policy.KeyBreakerMode)
	}
}

func policyFromResponse(t *testing.T, response managementResponse) map[string]any {
	t.Helper()
	configuration, ok := response.body["configuration"].(map[string]any)
	if !ok {
		t.Fatalf("response carries no configuration: %v", response.body)
	}
	applied, ok := configuration["policy"].(map[string]any)
	if !ok {
		t.Fatalf("configuration carries no policy: %v", configuration)
	}
	return applied
}

// recordingProber stands in for the upstream: these tests cover the request the
// API builds, not the outbound HTTP call.
type recordingProber struct {
	request upstream.Request
	result  upstream.Result
	err     error
	calls   int
}

func (p *recordingProber) Models(_ context.Context, request upstream.Request) (upstream.Result, error) {
	p.calls++
	p.request = request
	return p.result, p.err
}

// The console only ever sees a masked key, so a probe that carries the mask has
// to be made with the key the gateway stored.
func TestUpstreamModelProbeUsesTheStoredKeyForAMaskedRequest(t *testing.T) {
	server, _ := managementServer(t)
	prober := &recordingProber{result: upstream.Result{Models: []string{"gpt-4.1", "claude-3"}, Endpoint: "https://api.example.com/v1/models"}}
	server.Prober = prober

	created := callManagement(server, http.MethodPost, "/management/configuration/upstreams", "management-secret", map[string]any{
		"name": "Example", "url": "https://api.example.com",
		"keys": []string{"secret-primary", "secret-second"}, "models": []string{"gpt-4.1"},
	})
	if created.status != http.StatusCreated {
		t.Fatalf("create upstream status = %d, body = %v", created.status, created.body)
	}
	row, ok := created.body["row"].(map[string]any)
	if !ok {
		t.Fatalf("create response carries no row: %v", created.body)
	}
	upstreamID := int64(row["id"].(float64))
	masked := row["keys"].([]any)[0].(string)
	if !store.IsMaskedSecret(masked) {
		t.Fatalf("the created upstream answered with its key in the clear: %v", masked)
	}

	response := callManagement(server, http.MethodPost, "/management/upstreams/models", "management-secret", map[string]any{
		"id": upstreamID, "url": "https://api.example.com", "key": masked,
		"headers": map[string]any{"X-Org": "acme"},
	})
	if response.status != http.StatusOK {
		t.Fatalf("probe status = %d, body = %v", response.status, response.body)
	}
	if prober.request.APIKey != "secret-primary" {
		t.Errorf("probe key = %q, want the stored key rather than the mask", prober.request.APIKey)
	}
	if prober.request.Headers["X-Org"] != "acme" {
		t.Errorf("probe headers = %v, want the submitted headers", prober.request.Headers)
	}
	models, ok := response.body["models"].([]any)
	if !ok || len(models) != 2 {
		t.Fatalf("probe models = %v, want the two the upstream reported", response.body["models"])
	}
	if strings.Contains(strings.Join([]string{masked}, ""), "secret-primary") {
		t.Error("the response echoed a credential")
	}
}

// A key typed into the form is what the probe presents; nothing is read from the
// store when the request already carries one.
func TestUpstreamModelProbePrefersTheSubmittedKey(t *testing.T) {
	server, _ := managementServer(t)
	prober := &recordingProber{result: upstream.Result{Models: []string{"gpt-4.1"}}}
	server.Prober = prober

	response := callManagement(server, http.MethodPost, "/management/upstreams/models", "management-secret", map[string]any{
		"url": "https://api.example.com", "key": "typed-key",
	})
	if response.status != http.StatusOK {
		t.Fatalf("probe status = %d, body = %v", response.status, response.body)
	}
	if prober.request.APIKey != "typed-key" {
		t.Errorf("probe key = %q, want the submitted key", prober.request.APIKey)
	}
}

// A probe that the gateway could not make — an unusable address, a rejected
// credential — is answered as a client mistake, not as a gateway fault, and the
// failure carries a stable reason the console renders in its own language.
func TestUpstreamModelProbeReportsItsOwnRefusals(t *testing.T) {
	cases := []struct {
		name       string
		failure    upstream.Failure
		wantStatus int
		wantReason string
	}{
		{
			name:       "unusable address",
			failure:    upstream.Failure{Reason: upstream.ReasonInvalidRequest, Message: "an API address is required"},
			wantStatus: http.StatusBadRequest,
			wantReason: upstream.ReasonInvalidRequest,
		},
		{
			name:       "rejected credential",
			failure:    upstream.Failure{Reason: upstream.ReasonCredentialRejected, Status: http.StatusUnauthorized, Message: "the upstream rejected the credential"},
			wantStatus: http.StatusBadRequest,
			wantReason: upstream.ReasonCredentialRejected,
		},
		{
			name:       "no model endpoint",
			failure:    upstream.Failure{Reason: upstream.ReasonNoModelEndpoint, Status: http.StatusNotFound, Message: "the upstream answered 404"},
			wantStatus: http.StatusBadRequest,
			wantReason: upstream.ReasonNoModelEndpoint,
		},
		{
			name:       "unreachable upstream",
			failure:    upstream.Failure{Reason: upstream.ReasonUnreachable, Message: "connection refused"},
			wantStatus: http.StatusBadGateway,
			wantReason: upstream.ReasonUnreachable,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			server, _ := managementServer(t)
			server.Prober = &recordingProber{err: testCase.failure}

			response := callManagement(server, http.MethodPost, "/management/upstreams/models", "management-secret", map[string]any{
				"url": "https://api.example.com", "key": "typed-key",
			})
			if response.status != testCase.wantStatus {
				t.Fatalf("status = %d, want %d; body = %v", response.status, testCase.wantStatus, response.body)
			}
			if reason := errorField(response.body, "reason"); reason != testCase.wantReason {
				t.Errorf("reason = %v, want %s", reason, testCase.wantReason)
			}
		})
	}
}

// A probe goes out the way a real request to that upstream would: through the
// proxy the form carries. The form is where an operator sets it, so a probe that
// ignored it would fail on exactly the upstreams that need one.
func TestUpstreamModelProbeCarriesTheFormProxy(t *testing.T) {
	server, _ := managementServer(t)
	prober := &recordingProber{result: upstream.Result{Models: []string{"gpt-4.1"}}}
	server.Prober = prober

	response := callManagement(server, http.MethodPost, "/management/upstreams/models", "management-secret", map[string]any{
		"url": "https://api.example.com", "key": "typed-key", "proxy_url": "http://127.0.0.1:7890",
	})
	if response.status != http.StatusOK {
		t.Fatalf("probe status = %d, body = %v", response.status, response.body)
	}
	if prober.request.ProxyURL != "http://127.0.0.1:7890" {
		t.Errorf("probe proxy = %q, want the one from the form", prober.request.ProxyURL)
	}
}

// A form that leaves the proxy open probes the way the routing engine would
// reach that upstream: through the default proxy profile.
func TestUpstreamModelProbeFallsBackToTheDefaultProxy(t *testing.T) {
	server, _ := managementServer(t)
	prober := &recordingProber{result: upstream.Result{Models: []string{"gpt-4.1"}}}
	server.Prober = prober

	created := callManagement(server, http.MethodPost, "/management/configuration/proxies", "management-secret", map[string]any{
		"name": "默认出口", "protocol": "http", "url": "http://127.0.0.1:7891", "is_default": true,
	})
	if created.status != http.StatusCreated {
		t.Fatalf("create proxy status = %d, body = %v", created.status, created.body)
	}

	response := callManagement(server, http.MethodPost, "/management/upstreams/models", "management-secret", map[string]any{
		"url": "https://api.example.com", "key": "typed-key",
	})
	if response.status != http.StatusOK {
		t.Fatalf("probe status = %d, body = %v", response.status, response.body)
	}
	if prober.request.ProxyURL != "http://127.0.0.1:7891" {
		t.Errorf("probe proxy = %q, want the default profile", prober.request.ProxyURL)
	}

	// A proxy typed into the form is the more specific choice and wins.
	response = callManagement(server, http.MethodPost, "/management/upstreams/models", "management-secret", map[string]any{
		"url": "https://api.example.com", "key": "typed-key", "proxy_url": "direct",
	})
	if response.status != http.StatusOK {
		t.Fatalf("probe status = %d, body = %v", response.status, response.body)
	}
	if prober.request.ProxyURL != "direct" {
		t.Errorf("probe proxy = %q, want the form's choice", prober.request.ProxyURL)
	}
}

// A failed probe answers with what it tried and how it left, so the console can
// tell an operator which address and which proxy to look at.
func TestUpstreamModelProbeReportsWhereItWentAndHow(t *testing.T) {
	server, _ := managementServer(t)
	server.Prober = &recordingProber{err: upstream.Failure{
		Reason:  upstream.ReasonUnreachable,
		Message: "dial tcp: connection refused",
		Params: map[string]any{
			"endpoint":     "https://api.example.com/v1/models",
			"proxy_source": "system",
		},
	}}

	response := callManagement(server, http.MethodPost, "/management/upstreams/models", "management-secret", map[string]any{
		"url": "https://api.example.com", "key": "typed-key",
	})
	if response.status != http.StatusBadGateway {
		t.Fatalf("probe status = %d, want 502; body = %v", response.status, response.body)
	}
	params, _ := errorField(response.body, "params").(map[string]any)
	if params["endpoint"] != "https://api.example.com/v1/models" {
		t.Errorf("params = %v, want the address that was tried", params)
	}
	if params["proxy_source"] != "system" {
		t.Errorf("params = %v, want how the probe left", params)
	}
	if params["detail"] != "dial tcp: connection refused" {
		t.Errorf("params = %v, want the underlying error", params)
	}
}

// Clearing a circuit is how an operator brings a cooled-down or disabled channel
// back before its cooldown would have expired.
func TestBreakerResetClearsTheRecordedCircuit(t *testing.T) {
	server, persistent := managementServer(t)
	if err := persistent.EnsureBreakerSchema(context.Background()); err != nil {
		t.Fatalf("EnsureBreakerSchema() error = %v", err)
	}
	adapter, err := store.NewBreakerAdapter(context.Background(), persistent)
	if err != nil {
		t.Fatalf("NewBreakerAdapter() error = %v", err)
	}
	server.BreakerReset = adapter
	server.BreakerSnapshotter = adapter

	scope := breaker.Scope{ChannelID: "7"}
	adapter.Update(scope, func(state breaker.State) breaker.State {
		state.ConsecutiveFailures = 2
		state.Disabled = true
		return state
	})
	if _, known := adapter.Load(scope); !known {
		t.Fatal("the circuit was not recorded")
	}

	response := callManagement(server, http.MethodPost, "/management/breakers/reset", "management-secret", map[string]any{
		"scope": "channel", "channel_id": "7",
	})
	if response.status != http.StatusOK {
		t.Fatalf("reset status = %d, body = %v", response.status, response.body)
	}
	if reset, _ := response.body["reset"].(float64); reset != 1 {
		t.Errorf("reset = %v, want one cleared circuit", response.body["reset"])
	}
	if _, known := adapter.Load(scope); known {
		t.Error("the circuit is still recorded after the reset")
	}
}

func TestBreakerResetRejectsAnUnnamedCircuit(t *testing.T) {
	server, persistent := managementServer(t)
	if err := persistent.EnsureBreakerSchema(context.Background()); err != nil {
		t.Fatalf("EnsureBreakerSchema() error = %v", err)
	}
	adapter, err := store.NewBreakerAdapter(context.Background(), persistent)
	if err != nil {
		t.Fatalf("NewBreakerAdapter() error = %v", err)
	}
	server.BreakerReset = adapter

	response := callManagement(server, http.MethodPost, "/management/breakers/reset", "management-secret", map[string]any{
		"scope": "channel",
	})
	if response.status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a circuit with no identifier; body = %v", response.status, response.body)
	}
}

// The probe goes through the same origin check as every other console write.
func TestUpstreamModelProbeRejectsACrossOriginRequest(t *testing.T) {
	server, _ := managementServer(t)
	prober := &recordingProber{result: upstream.Result{Models: []string{"gpt-4.1"}}}
	server.Prober = prober

	encoded, err := json.Marshal(map[string]any{"url": "https://api.example.com", "key": "typed-key"})
	if err != nil {
		t.Fatalf("marshal probe request: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/management/upstreams/models", strings.NewReader(string(encoded)))
	request.Header.Set("Authorization", "Bearer management-secret")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "https://evil.example")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", response.Code)
	}
	if prober.calls != 0 {
		t.Error("a cross-origin probe still reached the upstream")
	}
}

// A gateway that manages no configuration cannot store a policy either.
func TestPolicyUpdateWithoutAConfigurationStore(t *testing.T) {
	server := &Server{ManagementToken: "management-secret"}
	response := callManagement(server, http.MethodPut, "/management/policy", "management-secret", map[string]any{
		"settings": map[string]any{policy.KeyRetryMaxAttempts: 2},
	})
	if response.status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", response.status)
	}
}
