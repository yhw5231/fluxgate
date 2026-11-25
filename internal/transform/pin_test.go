package transform

import (
	"encoding/json"
	"testing"

	"github.com/yhw5231/fluxgate/internal/domain"
)

func decodePinned(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatalf("decode pinned body: %v", err)
	}
	return document
}

// A strict attempt names the one provider it is pinned to, in both shapes when
// the pipeline is not known yet.
func TestApplyModelPinStrictWritesOnly(t *testing.T) {
	pin := domain.ModelPin{Model: "gpt-4.1", Mode: domain.PinModeStrict, Providers: []string{"alibaba", "baseten"}}
	updated, err := ApplyModelPin([]byte(`{"model":"gpt-4.1","messages":[]}`), pin, "baseten")
	if err != nil {
		t.Fatalf("ApplyModelPin() error = %v", err)
	}
	document := decodePinned(t, updated)

	provider, _ := document["provider"].(map[string]any)
	only, _ := provider["only"].([]any)
	if len(only) != 1 || only[0] != "baseten" {
		t.Fatalf("provider.only = %#v, want [baseten]", provider["only"])
	}
	options, _ := document["providerOptions"].(map[string]any)
	gateway, _ := options["gateway"].(map[string]any)
	if only, _ := gateway["only"].([]any); len(only) != 1 || only[0] != "baseten" {
		t.Fatalf("providerOptions.gateway.only = %#v", gateway["only"])
	}
	if document["messages"] == nil {
		t.Fatal("the pin lost the request's own fields")
	}
}

// A preferred pin sends the whole order, and maps the sort spelling onto what the
// direct pipeline understands.
func TestApplyModelPinPreferredWritesOrderAndSort(t *testing.T) {
	pin := domain.ModelPin{Mode: domain.PinModePreferred, Providers: []string{"a", "b"}, Sort: "cost"}
	updated, err := ApplyModelPin([]byte(`{"model":"m"}`), pin, "")
	if err != nil {
		t.Fatalf("ApplyModelPin() error = %v", err)
	}
	document := decodePinned(t, updated)
	provider, _ := document["provider"].(map[string]any)
	order, _ := provider["order"].([]any)
	if len(order) != 2 || order[0] != "a" || order[1] != "b" {
		t.Fatalf("provider.order = %#v", provider["order"])
	}
	if provider["sort"] != "price" {
		t.Fatalf("provider.sort = %v, want price", provider["sort"])
	}
	options, _ := document["providerOptions"].(map[string]any)
	gateway, _ := options["gateway"].(map[string]any)
	if gateway["sort"] != "cost" {
		t.Fatalf("gateway.sort = %v, want cost", gateway["sort"])
	}
}

// Once the pipeline is known, only its own shape is written.
func TestApplyModelPinWritesOnlyTheKnownPipeline(t *testing.T) {
	direct := domain.ModelPin{Mode: domain.PinModePreferred, Providers: []string{"a"}, Pipeline: domain.PinPipelineDirect}
	updated, err := ApplyModelPin([]byte(`{"model":"m"}`), direct, "")
	if err != nil {
		t.Fatalf("ApplyModelPin() error = %v", err)
	}
	if document := decodePinned(t, updated); document["providerOptions"] != nil {
		t.Fatalf("a direct pin wrote providerOptions: %v", document["providerOptions"])
	}

	planner := domain.ModelPin{Mode: domain.PinModePreferred, Providers: []string{"a"}, Pipeline: domain.PinPipelinePlanner}
	updated, err = ApplyModelPin([]byte(`{"model":"m"}`), planner, "")
	if err != nil {
		t.Fatalf("ApplyModelPin() error = %v", err)
	}
	if document := decodePinned(t, updated); document["provider"] != nil {
		t.Fatalf("a planner pin wrote provider: %v", document["provider"])
	}
}

// Excluding a provider removes it from the list the gateway writes.
func TestApplyModelPinSkipsExcludedProviders(t *testing.T) {
	pin := domain.ModelPin{Mode: domain.PinModePreferred, Providers: []string{"a", "b", "c"}, Exclude: []string{"b"}}
	updated, err := ApplyModelPin([]byte(`{"model":"m"}`), pin, "")
	if err != nil {
		t.Fatalf("ApplyModelPin() error = %v", err)
	}
	provider, _ := decodePinned(t, updated)["provider"].(map[string]any)
	order, _ := provider["order"].([]any)
	if len(order) != 2 || order[1] != "c" {
		t.Fatalf("provider.order = %#v, want a and c", provider["order"])
	}
}

// The pin merges into what the caller already sent rather than replacing it.
func TestApplyModelPinPreservesExistingProviderFields(t *testing.T) {
	body := `{"model":"m","provider":{"require_parameters":true,"order":["old"]},"providerOptions":{"gateway":{"zeroDataRetention":true}}}`
	pin := domain.ModelPin{Mode: domain.PinModePreferred, Providers: []string{"new"}}
	updated, err := ApplyModelPin([]byte(body), pin, "")
	if err != nil {
		t.Fatalf("ApplyModelPin() error = %v", err)
	}
	document := decodePinned(t, updated)
	provider, _ := document["provider"].(map[string]any)
	if provider["require_parameters"] != true {
		t.Fatalf("provider.require_parameters lost: %#v", provider)
	}
	options, _ := document["providerOptions"].(map[string]any)
	gateway, _ := options["gateway"].(map[string]any)
	if gateway["zeroDataRetention"] != true {
		t.Fatalf("gateway.zeroDataRetention lost: %#v", gateway)
	}
}

// A pin asks for nothing, or the body cannot carry one: the request stays exactly
// as it was.
func TestApplyModelPinLeavesWhatItCannotPinAlone(t *testing.T) {
	for _, testcase := range []struct {
		name string
		body string
		pin  domain.ModelPin
	}{
		{"no providers", `{"model":"m"}`, domain.ModelPin{Mode: domain.PinModeStrict}},
		{"empty body", ``, domain.ModelPin{Mode: domain.PinModeStrict, Providers: []string{"a"}}},
		{"all excluded", `{"model":"m"}`, domain.ModelPin{Mode: domain.PinModeStrict, Providers: []string{"a"}, Exclude: []string{"a"}}},
		{"not json", `plain text`, domain.ModelPin{Mode: domain.PinModePreferred, Providers: []string{"a"}}},
		{"not an object", `[1,2]`, domain.ModelPin{Mode: domain.PinModePreferred, Providers: []string{"a"}}},
	} {
		updated, err := ApplyModelPin([]byte(testcase.body), testcase.pin, "")
		if err != nil {
			t.Fatalf("%s: ApplyModelPin() error = %v", testcase.name, err)
		}
		if string(updated) != testcase.body {
			t.Fatalf("%s: body changed to %q", testcase.name, updated)
		}
	}
}
