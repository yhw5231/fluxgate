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

	"github.com/yhw5231/fluxgate/internal/policy"
	"github.com/yhw5231/fluxgate/internal/probe"
)

// fakeProbes is a probe service that answers from memory, so the endpoint
// contract can be tested without an upstream.
type fakeProbes struct {
	mu      sync.Mutex
	runs    []probe.Result
	allRuns int
	results []probe.Result
	channel string
	busy    bool
	cleared int64
	started bool
	// pin probing: what the fake answers and what it was asked.
	discovery          probe.PinDiscovery
	validations        []probe.PinValidation
	discoveryLine      probe.Line
	validatedProviders []string
	validatedPipeline  string
}

func (f *fakeProbes) ProbeAll(context.Context) []probe.Result {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = true
	return f.results
}

func (f *fakeProbes) ProbeAllAsync(context.Context) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.busy {
		return false
	}
	f.busy = true
	f.started = true
	return true
}

func (f *fakeProbes) ProbeChannel(_ context.Context, channelID string) []probe.Result {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.channel = channelID
	selected := make([]probe.Result, 0, 1)
	for _, result := range f.results {
		if result.ChannelID == channelID {
			selected = append(selected, result)
		}
	}
	return selected
}

func (f *fakeProbes) ListRuns(_ context.Context, limit int) ([]probe.Result, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	runs := f.runs
	if limit > 0 && len(runs) > limit {
		runs = runs[:limit]
	}
	return append([]probe.Result(nil), runs...), int64(f.allRuns), nil
}

func (f *fakeProbes) ClearRuns(context.Context) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cleared = int64(len(f.runs))
	f.runs = nil
	f.allRuns = 0
	return f.cleared, nil
}

func (f *fakeProbes) DiscoverPin(_ context.Context, line probe.Line) probe.PinDiscovery {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.discoveryLine = line
	return f.discovery
}

func (f *fakeProbes) ValidatePins(_ context.Context, line probe.Line, providers []string, pipeline string) []probe.PinValidation {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.discoveryLine = line
	f.validatedProviders = append([]string(nil), providers...)
	f.validatedPipeline = pipeline
	return f.validations
}

// probeServer assembles a gateway that keeps probe records.
func probeServer(t *testing.T, service *fakeProbes) *Server {
	t.Helper()
	defaults := policy.Default()
	if defaults.Probe.Keep <= 0 {
		defaults.Probe.Keep = 2000
	}
	return &Server{
		ManagementToken: "management-secret",
		Probes:          service,
		PolicyDefaults:  defaults,
	}
}

func TestProbesEndpointReportsWhatWasFound(t *testing.T) {
	service := &fakeProbes{
		runs: []probe.Result{{
			At:          time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC),
			Kind:        probe.KindManual,
			ChannelID:   "12",
			ChannelName: "relay",
			Model:       "gpt-4.1",
			Status:      200,
			LatencyMS:   120,
			OK:          true,
			Reason:      probe.ReasonOK,
		}},
		allRuns: 1,
	}
	server := probeServer(t, service)

	request := httptest.NewRequest(http.MethodGet, "/management/probes", nil)
	request.Header.Set("Authorization", "Bearer management-secret")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", response.Code, response.Body.String())
	}

	var answer struct {
		Probes    []probe.Result `json:"probes"`
		Total     int64          `json:"total"`
		Retention map[string]any `json:"retention"`
		Policy    map[string]any `json:"policy"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode answer: %v", err)
	}
	if len(answer.Probes) != 1 || answer.Total != 1 {
		t.Fatalf("answer = %s, want the recorded probe", response.Body.String())
	}
	if answer.Probes[0].ChannelName != "relay" || !answer.Probes[0].OK {
		t.Fatalf("probe = %#v", answer.Probes[0])
	}
	if answer.Retention["keep"] == nil {
		t.Fatalf("retention = %#v, want the configured bound", answer.Retention)
	}
	if answer.Policy["startup"] == nil {
		t.Fatalf("policy = %#v, want the probe schedule settings", answer.Policy)
	}
}

func TestProbeRunChecksOneUpstream(t *testing.T) {
	service := &fakeProbes{results: []probe.Result{
		{ChannelID: "12", ChannelName: "relay", OK: true, Reason: probe.ReasonOK},
		{ChannelID: "13", ChannelName: "other", OK: true, Reason: probe.ReasonOK},
	}}
	server := probeServer(t, service)
	request := httptest.NewRequest(http.MethodPost, "/management/probes/run", strings.NewReader(`{"channel_id":"12"}`))
	request.Header.Set("Authorization", "Bearer management-secret")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", response.Code, response.Body.String())
	}
	if service.channel != "12" {
		t.Fatalf("probed channel %q, want 12", service.channel)
	}
	var answer struct {
		Probes []probe.Result `json:"probes"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode answer: %v", err)
	}
	if len(answer.Probes) != 1 || answer.Probes[0].ChannelID != "12" {
		t.Fatalf("answer = %#v, want only the named upstream", answer.Probes)
	}
}

func TestProbeRunRefusesAnUnknownUpstream(t *testing.T) {
	server := probeServer(t, &fakeProbes{})
	request := httptest.NewRequest(http.MethodPost, "/management/probes/run", strings.NewReader(`{"channel_id":"nope"}`))
	request.Header.Set("Authorization", "Bearer management-secret")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", response.Code)
	}
}

func TestProbeRunStartsASweepInTheBackground(t *testing.T) {
	service := &fakeProbes{}
	server := probeServer(t, service)
	request := httptest.NewRequest(http.MethodPost, "/management/probes/run", nil)
	request.Header.Set("Authorization", "Bearer management-secret")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", response.Code, response.Body.String())
	}
	if !service.started {
		t.Fatal("the sweep was not started")
	}

	// A sweep already running is reported rather than queued.
	service.busy = true
	request = httptest.NewRequest(http.MethodPost, "/management/probes/run", nil)
	request.Header.Set("Authorization", "Bearer management-secret")
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", response.Code)
	}
}

func TestProbeClearEmptiesTheRecord(t *testing.T) {
	service := &fakeProbes{runs: []probe.Result{{ChannelID: "12"}}, allRuns: 1}
	server := probeServer(t, service)
	request := httptest.NewRequest(http.MethodDelete, "/management/probes", nil)
	request.Header.Set("Authorization", "Bearer management-secret")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", response.Code, response.Body.String())
	}
	var answer struct {
		Cleared int64 `json:"cleared"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode answer: %v", err)
	}
	if answer.Cleared != 1 {
		t.Fatalf("cleared = %d, want 1", answer.Cleared)
	}
}

func TestProbeEndpointsRequireAuthentication(t *testing.T) {
	server := probeServer(t, &fakeProbes{})
	for _, target := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/management/probes"},
		{http.MethodPost, "/management/probes/run"},
		{http.MethodDelete, "/management/probes"},
	} {
		request := httptest.NewRequest(target.method, target.path, nil)
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s status = %d, want 401", target.method, target.path, response.Code)
		}
	}
}

func TestProbeEndpointsReportAGatewayWithoutASchedule(t *testing.T) {
	server := &Server{ManagementToken: "management-secret"}
	request := httptest.NewRequest(http.MethodGet, "/management/probes", nil)
	request.Header.Set("Authorization", "Bearer management-secret")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", response.Code)
	}
}
