package transform

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/yhw5231/fluxgate/internal/domain"
)

// Writing an internal-provider pin into a chat-completions body.
//
// Some upstream gateways serve one model from several internal providers and
// pick between them per request. An operator who needs one of them says so, and
// the gateway expresses that in the body the upstream reads:
//
//   - the OpenRouter-shaped gateway ("direct") reads provider.only / order /
//     sort at the top level;
//   - the Vercel-AI-gateway-shaped one ("planner") reads
//     providerOptions.gateway.only / order / sort.
//
// A pipeline that has not been discovered yet gets both shapes written, because
// each gateway ignores the fields it does not know. Nothing else about the body
// changes: fields the caller sent stay as they were, and the provider objects
// are merged rather than replaced.

// openRouterSort maps the operator's names onto the direct pipeline's.
var openRouterSort = map[string]string{"cost": "price", "ttft": "latency", "tps": "throughput"}

// ApplyModelPin writes one pin into a request body. A strict attempt names the
// one provider it is pinned to; a preferred attempt sends the ordered list. An
// empty body, a body that is not a JSON object, or a pin that asks for nothing
// leaves the request untouched, which is what an unpinned line does.
func ApplyModelPin(raw []byte, pin domain.ModelPin, strictProvider string) ([]byte, error) {
	if len(raw) == 0 {
		return raw, nil
	}
	sortValue := normalizePinSort(pin.Sort)
	order := pin.ActiveProviders()
	if strictProvider == "" && len(order) == 0 && sortValue == "" {
		return raw, nil
	}

	var document map[string]json.RawMessage
	if err := json.Unmarshal(raw, &document); err != nil || document == nil {
		// Not a JSON object: a pin has nowhere to go, and a body that only looked
		// like JSON must still reach the upstream as the caller sent it.
		return raw, nil
	}

	direct := pin.Pipeline == "" || pin.Pipeline == domain.PinPipelineDirect
	planner := pin.Pipeline == "" || pin.Pipeline == domain.PinPipelinePlanner

	// The fields both pipelines share, filled per pipeline below because the sort
	// spelling differs between them.
	fill := func(target map[string]any) {
		switch {
		case strictProvider != "":
			target["only"] = []string{strictProvider}
		case len(order) > 0 && pin.Mode == domain.PinModePreferred:
			target["order"] = order
		case len(order) > 0:
			// A strict pin asked without a provider to pin: the ordered list is the
			// closest truthful expression of what the operator wrote, and it still
			// keeps the request inside the list.
			target["only"] = order
		}
	}

	if planner {
		gateway := map[string]any{}
		fill(gateway)
		if sortValue != "" {
			gateway["sort"] = sortValue
		}
		if len(gateway) > 0 {
			merged, err := mergeJSONObject(document["providerOptions"], "gateway", gateway)
			if err != nil {
				return nil, err
			}
			document["providerOptions"] = merged
		}
	}
	if direct {
		provider := map[string]any{}
		fill(provider)
		if sortValue != "" {
			provider["sort"] = sortValue
			if mapped := openRouterSort[sortValue]; mapped != "" {
				provider["sort"] = mapped
			}
		}
		if len(provider) > 0 {
			merged, err := mergeJSONObject(document["provider"], "", provider)
			if err != nil {
				return nil, err
			}
			document["provider"] = merged
		}
	}

	encoded, err := json.Marshal(document)
	if err != nil {
		return nil, fmt.Errorf("encode the pinned request: %w", err)
	}
	return encoded, nil
}

// normalizePinSort renders the operator's sort choice, treating an empty value
// and "none" as "leave the upstream's order alone".
func normalizePinSort(value string) string {
	trimmed := strings.ToLower(strings.TrimSpace(value))
	if trimmed == "none" {
		return ""
	}
	return trimmed
}

// mergeJSONObject merges fields into an existing JSON object in the document,
// either the document's own key or one nested inside it (the gateway object
// inside providerOptions). Keys already there are kept unless the pin sets them.
func mergeJSONObject(existing json.RawMessage, nested string, fields map[string]any) (json.RawMessage, error) {
	merged := map[string]any{}
	if len(existing) > 0 {
		var decoded map[string]any
		if err := json.Unmarshal(existing, &decoded); err == nil && decoded != nil {
			merged = decoded
		}
	}
	target := merged
	if nested != "" {
		inner := map[string]any{}
		if value, ok := merged[nested].(map[string]any); ok && value != nil {
			inner = value
		}
		for key, value := range fields {
			inner[key] = value
		}
		merged[nested] = inner
	} else {
		for key, value := range fields {
			merged[key] = value
		}
	}
	encoded, err := json.Marshal(target)
	if err != nil {
		return nil, fmt.Errorf("encode the pinned field: %w", err)
	}
	return encoded, nil
}
