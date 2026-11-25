package probe

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestParsePinRoutingReadsTheShapesGatewaysUse(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		pipeline string
		last     string
	}{
		{
			name:     "planner at the top level",
			body:     `{"choices":[],"provider_metadata":{"gateway":{"routing":{"finalProvider":"baseten","fallbacksAvailable":["alibaba"]}}}}`,
			pipeline: "planner",
			last:     "baseten",
		},
		{
			name:     "planner on the first choice",
			body:     `{"choices":[{"provider_metadata":{"gateway":{"routing":{"finalProvider":"alibaba"}}}}]}`,
			pipeline: "planner",
			last:     "alibaba",
		},
		{
			name:     "planner on the first message",
			body:     `{"choices":[{"message":{"provider_metadata":{"gateway":{"routing":{"finalProvider":"novita"}}}}}]}`,
			pipeline: "planner",
			last:     "novita",
		},
		{
			name:     "planner inside a data envelope",
			body:     `{"data":{"choices":[{"provider_metadata":{"gateway":{"routing":{"finalProvider":"groq"}}}}]}}`,
			pipeline: "planner",
			last:     "groq",
		},
		{
			name:     "direct names the provider at the top level",
			body:     `{"provider":"baseten","choices":[{"message":{"content":"ok"}}]}`,
			pipeline: "direct",
			last:     "baseten",
		},
		{
			name:     "no routing information",
			body:     `{"choices":[{"message":{"content":"ok"}}]}`,
			pipeline: "",
			last:     "",
		},
	}
	for _, testcase := range cases {
		routing := parsePinRouting([]byte(testcase.body))
		if routing.Pipeline != testcase.pipeline || routing.LastProvider != testcase.last {
			t.Fatalf("%s: routing = %#v, want %q / %q", testcase.name, routing, testcase.pipeline, testcase.last)
		}
	}
}

func TestParsePinRoutingReadsFallbacksAndTier0(t *testing.T) {
	body := `{"provider_metadata":{"gateway":{"routing":{
		"finalProvider":"alibaba",
		"fallbacksAvailable":["baseten","novita"],
		"planningReasoning":"alibaba won tier 0 over groq and cerebras"
	}}}}`
	routing := parsePinRouting([]byte(body))
	if len(routing.Fallbacks) != 2 || routing.Fallbacks[1] != "novita" {
		t.Fatalf("fallbacks = %#v", routing.Fallbacks)
	}
	if len(routing.Tier0) != 3 || routing.Tier0[0] != "alibaba" || routing.Tier0[2] != "cerebras" {
		t.Fatalf("tier0 = %#v", routing.Tier0)
	}
}

func TestExtractAvailableProvidersReadsBothErrorShapes(t *testing.T) {
	planner := `{"error":{"message":"invalid provider: __probe__. Available providers are: alibaba, baseten and novita. Pick one of those.","type":"invalid_request_error"}}`
	if providers := extractAvailableProviders([]byte(planner), "planner"); len(providers) != 3 || providers[2] != "novita" {
		t.Fatalf("planner providers = %#v", providers)
	}
	// The same sentence is read when the pipeline is not known yet.
	if providers := extractAvailableProviders([]byte(planner), ""); len(providers) != 3 {
		t.Fatalf("unknown-pipeline providers = %#v", providers)
	}

	direct := `{"error":{"message":"no such provider","metadata":{"available_providers":["Alibaba","baseten"]}}}`
	if providers := extractAvailableProviders([]byte(direct), "direct"); len(providers) != 2 || providers[0] != "alibaba" {
		t.Fatalf("direct providers = %#v", providers)
	}
	if providers := extractAvailableProviders([]byte(`{"error":"nope"}`), ""); providers != nil {
		t.Fatalf("providers = %#v, want none", providers)
	}
}

func TestMergeUniqueKeepsOrderAndDropsDuplicates(t *testing.T) {
	merged := mergeUnique([]string{"b", "a"}, []string{"a", "c"}, []string{"C", ""})
	want := []string{"b", "a", "c"}
	if len(merged) != len(want) {
		t.Fatalf("merged = %#v, want %#v", merged, want)
	}
	for index := range want {
		if merged[index] != want[index] {
			t.Fatalf("merged = %#v, want %#v", merged, want)
		}
	}
}

// discoveryUpstream answers a normal request with the routing it served from, and
// a request pinned to a provider that does not exist with the list it could have
// used.
func discoveryUpstream(t *testing.T, pipeline, served, errorBody string, seen *[]map[string]any) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if seen != nil {
			mu.Lock()
			*seen = append(*seen, body)
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		if pinnedProbeProvider(body, pipeline) == "__probe__" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(errorBody))
			return
		}
		var answer string
		switch pipeline {
		case "planner":
			answer = `{"choices":[{"message":{"content":"OK"},"provider_metadata":{"gateway":{"routing":{"finalProvider":"` + served + `","fallbacksAvailable":["alibaba"]}}}}]}`
		default:
			answer = `{"provider":"` + served + `","choices":[{"message":{"content":"OK"}}]}`
		}
		_, _ = w.Write([]byte(answer))
	}))
}

// pinnedProbeProvider reads the provider a request pins, whichever shape it uses.
func pinnedProbeProvider(body map[string]any, pipeline string) string {
	if pipeline != "direct" {
		if options, ok := body["providerOptions"].(map[string]any); ok {
			if gateway, ok := options["gateway"].(map[string]any); ok {
				if only, ok := gateway["only"].([]any); ok && len(only) == 1 {
					return only[0].(string)
				}
			}
		}
	}
	if provider, ok := body["provider"].(map[string]any); ok {
		if only, ok := provider["only"].([]any); ok && len(only) == 1 {
			return only[0].(string)
		}
	}
	return ""
}

func TestDiscoverPinWithAPlannerUpstream(t *testing.T) {
	server := discoveryUpstream(t, "planner", "baseten",
		`{"error":{"message":"invalid provider: __probe__. Available providers are: alibaba, baseten and novita."}}`, nil)
	defer server.Close()

	discovery := (&Prober{Client: server.Client()}).DiscoverPin(context.Background(), Line{
		ChannelID: "1", ChannelName: "relay", BaseURL: server.URL + "/v1", APIKey: "sk", Model: "gpt-4.1",
	})
	if !discovery.OK {
		t.Fatalf("discovery = %#v, want a healthy upstream", discovery)
	}
	if discovery.Pipeline != "planner" || discovery.LastProvider != "baseten" {
		t.Fatalf("discovery = %#v", discovery)
	}
	// A planner enumerated the list for this request, so the harvested order leads;
	// the fallbacks follow without duplicates.
	want := []string{"alibaba", "baseten", "novita"}
	if len(discovery.Providers) != len(want) {
		t.Fatalf("providers = %#v, want %#v", discovery.Providers, want)
	}
	for index := range want {
		if discovery.Providers[index] != want[index] {
			t.Fatalf("providers = %#v, want %#v", discovery.Providers, want)
		}
	}
}

func TestDiscoverPinWithADirectUpstream(t *testing.T) {
	server := discoveryUpstream(t, "direct", "baseten",
		`{"error":{"message":"no such provider","metadata":{"available_providers":["alibaba","baseten"]}}}`, nil)
	defer server.Close()

	discovery := (&Prober{Client: server.Client()}).DiscoverPin(context.Background(), Line{
		ChannelID: "1", ChannelName: "relay", BaseURL: server.URL + "/v1", APIKey: "sk", Model: "gpt-4.1",
	})
	if !discovery.OK || discovery.Pipeline != "direct" || discovery.LastProvider != "baseten" {
		t.Fatalf("discovery = %#v", discovery)
	}
	if len(discovery.Providers) != 2 {
		t.Fatalf("providers = %#v", discovery.Providers)
	}
}

func TestDiscoverPinReportsAnUpstreamError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"message":"model not found"}}`))
	}))
	defer server.Close()

	discovery := (&Prober{Client: server.Client()}).DiscoverPin(context.Background(), Line{
		BaseURL: server.URL + "/v1", APIKey: "sk", Model: "missing",
	})
	if discovery.OK {
		t.Fatalf("discovery = %#v, want a failure", discovery)
	}
	if !strings.Contains(discovery.Detail, "model not found") {
		t.Fatalf("detail = %q, want the upstream's own words", discovery.Detail)
	}
}

func TestValidatePinsChecksEachProviderExclusively(t *testing.T) {
	var seen []map[string]any
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		seen = append(seen, body)
		mu.Unlock()
		provider := pinnedProbeProvider(body, "direct")
		w.Header().Set("Content-Type", "application/json")
		switch provider {
		case "alibaba":
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"OK"}}]}`))
		case "baseten":
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"rate limited"}`))
		case "novita":
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
		default:
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"error":"down"}`))
		}
	}))
	defer server.Close()

	results := (&Prober{Client: server.Client()}).ValidatePins(context.Background(), Line{
		BaseURL: server.URL + "/v1", APIKey: "sk", Model: "gpt-4.1",
	}, []string{"alibaba", "baseten", "novita", "groq"}, "direct")

	if len(results) != 4 {
		t.Fatalf("results = %#v, want one per provider", results)
	}
	if !results[0].OK || results[0].Reason != ReasonOK {
		t.Fatalf("alibaba = %#v, want healthy", results[0])
	}
	if results[1].Reason != ReasonRateLimited {
		t.Fatalf("baseten = %#v, want rate limited", results[1])
	}
	if results[2].Reason != ReasonAuthRejected {
		t.Fatalf("novita = %#v, want the credential rejected", results[2])
	}
	if results[3].Reason != ReasonUpstreamError {
		t.Fatalf("groq = %#v, want an upstream error", results[3])
	}
	mu.Lock()
	defer mu.Unlock()
	for index, provider := range []string{"alibaba", "baseten", "novita", "groq"} {
		if pinned := pinnedProbeProvider(seen[index], "direct"); pinned != provider {
			t.Fatalf("request %d pinned %q, want %q", index, pinned, provider)
		}
	}
}
