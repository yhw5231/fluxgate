package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yhw5231/fluxgate/internal/domain"
	"github.com/yhw5231/fluxgate/internal/policy"
	"github.com/yhw5231/fluxgate/internal/probe"
	"github.com/yhw5231/fluxgate/internal/store"
)

// fakePinStore records what a discovery wrote back, so the endpoint contract can
// be checked without a database.
type fakePinStore struct {
	mu        sync.Mutex
	calls     int
	siteID    int64
	model     string
	pipeline  string
	providers []string
	last      string
}

func (f *fakePinStore) SavePinProbe(_ context.Context, siteID int64, model, pipeline string, providers []string, lastProvider string, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.siteID = siteID
	f.model = model
	f.pipeline = pipeline
	f.providers = append([]string(nil), providers...)
	f.last = lastProvider
	return nil
}

// pinServer assembles a gateway with one enabled line on upstream 7 and an
// injectable probe service.
func pinServer(t *testing.T, service *fakeProbes) (*Server, *fakePinStore) {
	t.Helper()
	channel := domain.Channel{
		ID: "line-7", Name: "Example", BaseURL: "https://api.example.com/v1",
		APIKey: "sk-example", SiteID: 7, Enabled: true,
	}
	routes := testRoutes(channel)
	pinStore := &fakePinStore{}
	server := &Server{
		ManagementToken: "management-secret",
		Probes:          service,
		PinStore:        pinStore,
		PolicyDefaults:  policy.Default(),
	}
	server.SetConfiguration(store.Configuration{Routes: routes, Channels: []domain.Channel{channel}})
	return server, pinStore
}

func pinRequest(method, path, body string) *http.Request {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer management-secret")
	request.Header.Set("Content-Type", "application/json")
	return request
}

func TestPinProbeDiscoversAndSavesWhatItFound(t *testing.T) {
	service := &fakeProbes{discovery: probe.PinDiscovery{
		OK:           true,
		Pipeline:     domain.PinPipelinePlanner,
		Providers:    []string{"alibaba", "baseten"},
		LastProvider: "baseten",
		Status:       200,
	}}
	server, pinStore := pinServer(t, service)

	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, pinRequest(http.MethodPost, "/management/pins/probe",
		`{"site_id":7,"model":"gpt-4.1"}`))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", response.Code, response.Body.String())
	}
	var answer struct {
		Pipeline     string   `json:"pipeline"`
		Providers    []string `json:"providers"`
		LastProvider string   `json:"last_provider"`
		Saved        bool     `json:"saved"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode answer: %v", err)
	}
	if answer.Pipeline != domain.PinPipelinePlanner || len(answer.Providers) != 2 || answer.LastProvider != "baseten" || !answer.Saved {
		t.Fatalf("answer = %#v", answer)
	}
	if service.discoveryLine.Model != "gpt-4.1" || service.discoveryLine.ChannelID != "line-7" {
		t.Fatalf("probed line = %#v, want the upstream's enabled line with the named model", service.discoveryLine)
	}
	pinStore.mu.Lock()
	defer pinStore.mu.Unlock()
	if pinStore.calls != 1 || pinStore.siteID != 7 || pinStore.pipeline != domain.PinPipelinePlanner || pinStore.last != "baseten" {
		t.Fatalf("saved = %#v, want the discovery written back", pinStore)
	}
}

func TestPinProbeReportsAnUpstreamThatDidNotAnswer(t *testing.T) {
	service := &fakeProbes{discovery: probe.PinDiscovery{OK: false, Status: 404, Detail: "model not found"}}
	server, pinStore := pinServer(t, service)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, pinRequest(http.MethodPost, "/management/pins/probe",
		`{"site_id":7,"model":"no-such-model"}`))
	if response.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: %s", response.Code, response.Body.String())
	}
	pinStore.mu.Lock()
	defer pinStore.mu.Unlock()
	if pinStore.calls != 0 {
		t.Fatal("a failed discovery wrote itself back onto the pin")
	}
}

func TestPinProbeRefusesAnUpstreamWithNoLine(t *testing.T) {
	server, _ := pinServer(t, &fakeProbes{})
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, pinRequest(http.MethodPost, "/management/pins/probe",
		`{"site_id":99,"model":"gpt-4.1"}`))
	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", response.Code)
	}
}

func TestPinValidateChecksEachProvider(t *testing.T) {
	service := &fakeProbes{validations: []probe.PinValidation{
		{Provider: "alibaba", OK: true, Reason: probe.ReasonOK},
		{Provider: "baseten", Reason: probe.ReasonRateLimited},
	}}
	server, _ := pinServer(t, service)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, pinRequest(http.MethodPost, "/management/pins/validate",
		`{"site_id":7,"model":"gpt-4.1","providers":["alibaba","baseten"]}`))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", response.Code, response.Body.String())
	}
	var answer struct {
		Results []probe.PinValidation `json:"results"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode answer: %v", err)
	}
	if len(answer.Results) != 2 || !answer.Results[0].OK || answer.Results[1].Reason != probe.ReasonRateLimited {
		t.Fatalf("results = %#v", answer.Results)
	}
	if len(service.validatedProviders) != 2 {
		t.Fatalf("validated providers = %#v", service.validatedProviders)
	}
}

func TestPinValidateRequiresProviders(t *testing.T) {
	server, _ := pinServer(t, &fakeProbes{})
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, pinRequest(http.MethodPost, "/management/pins/validate",
		`{"site_id":7,"model":"gpt-4.1","providers":[]}`))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", response.Code)
	}
}

func TestPinProbeEndpointsRequireAuthentication(t *testing.T) {
	server, _ := pinServer(t, &fakeProbes{})
	for _, path := range []string{"/management/pins/probe", "/management/pins/validate"} {
		request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"site_id":7,"model":"m"}`))
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("%s status = %d, want 401", path, response.Code)
		}
	}
}
