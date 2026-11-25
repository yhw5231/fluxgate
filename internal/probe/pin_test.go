package probe

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
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
		answer := "OK"
		if computed, ok := questionAnswer(promptOfBody(raw)); ok {
			answer = fmt.Sprintf("The answer is %d.", computed)
		}
		encoded, _ := json.Marshal(answer)
		var reply string
		switch pipeline {
		case "planner":
			reply = `{"choices":[{"message":{"content":` + string(encoded) + `},"provider_metadata":{"gateway":{"routing":{"finalProvider":"` + served + `","fallbacksAvailable":["alibaba"]}}}}]}`
		default:
			reply = `{"provider":"` + served + `","choices":[{"message":{"content":` + string(encoded) + `}}]}`
		}
		_, _ = w.Write([]byte(reply))
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
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		mu.Lock()
		seen = append(seen, body)
		mu.Unlock()
		provider := pinnedProbeProvider(body, "direct")
		w.Header().Set("Content-Type", "application/json")
		switch provider {
		case "alibaba":
			answer, _ := questionAnswer(promptOfBody(raw))
			_, _ = w.Write([]byte(chatAnswer(fmt.Sprintf("The answer is %d.", answer))))
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

// A provider that serves the request but states the wrong result is not healthy:
// the validation asks a question whose answer proves the provider did the work,
// rather than a hello any relay can hand back.
func TestValidatePinsReportsAWrongAnswer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		answer, _ := questionAnswer(promptOfBody(raw))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(chatAnswer(fmt.Sprintf("The answer is %d.", answer+7))))
	}))
	defer server.Close()

	results := (&Prober{Client: server.Client()}).ValidatePins(context.Background(), Line{
		BaseURL: server.URL + "/v1", APIKey: "sk", Model: "gpt-4.1",
	}, []string{"alibaba"}, "direct")

	if len(results) != 1 || results[0].OK || results[0].Reason != ReasonWrongAnswer {
		t.Fatalf("results = %#v, want the wrong answer reported", results)
	}
}

// The pin probe asks with the smallest body too, so a model that spells its output
// bound differently is not reported as a provider that is down.
func TestValidatePinsSendsTheSmallestRequestBody(t *testing.T) {
	var raw []byte
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		raw = body
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(chatAnswer("done")))
	}))
	defer server.Close()

	(&Prober{Client: server.Client()}).ValidatePins(context.Background(), Line{
		BaseURL: server.URL + "/v1", APIKey: "sk", Model: "gpt-4.1",
	}, []string{"alibaba"}, "direct")

	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("decode pin probe request: %v", err)
	}
	for _, name := range []string{"max_tokens", "max_completion_tokens", "max_output_tokens", "temperature", "top_p"} {
		if _, present := payload[name]; present {
			t.Errorf("pin probe body carries %q; a model that spells it differently would answer 400", name)
		}
	}
}
