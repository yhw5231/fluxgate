package probe

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/yhw5231/fluxgate/internal/clientprofile"
	"github.com/yhw5231/fluxgate/internal/domain"
	"github.com/yhw5231/fluxgate/internal/transform"
)

// Discovering and verifying the internal providers behind one upstream model.
//
// A pin the operator writes by hand names providers the upstream has never been
// asked about. This file answers two questions about one model on one upstream:
//
//   - which internal providers it actually has, and which pipeline the gateway
//     speaks (so the pin is written in a shape it reads);
//   - whether each of those providers answers right now.
//
// The first question is answered by asking the upstream twice. A normal small
// request comes back with the provider that served it, and a planner or direct
// gateway names the provider in its routing metadata. A second request pins a
// provider that does not exist on purpose: a gateway that cannot use it answers
// with an error that names the providers it could have used. That list, merged
// with whatever the routing metadata mentioned, is the discovery result.
//
// The second question is a loop: one small request per provider, pinned to it
// exclusively, classified by what the upstream answered.

const (
	probeHarvestTokens = 8
	pinAskTokens       = 16
	// pinnedProbeWord is what a validation asks for: cheap, and enough to make the
	// upstream route the request for real.
	pinnedProbeWord = "Reply with the word OK"
)

// PinDiscovery is what one model's internal providers look like after a probe.
type PinDiscovery struct {
	// OK reports whether the upstream answered a real request. A discovery that
	// failed leaves the pin as it was.
	OK bool `json:"ok"`
	// Pipeline is the shape the upstream speaks: "direct", "planner", or empty
	// when the answer carried no routing metadata.
	Pipeline string `json:"pipeline"`
	// Providers is what the upstream named it could have used, in the order the
	// evidence was collected.
	Providers []string `json:"providers"`
	// LastProvider is the one that actually served the probe, which is the
	// strongest single piece of evidence.
	LastProvider string `json:"last_provider,omitempty"`
	// Status is the HTTP status of the request that answered.
	Status int `json:"status"`
	// Detail explains a failed discovery in the upstream's own words.
	Detail string `json:"detail,omitempty"`
}

// PinValidation is one provider's answer when it is pinned exclusively.
type PinValidation struct {
	Provider  string `json:"provider"`
	Status    int    `json:"status"`
	LatencyMS int64  `json:"latency_ms"`
	OK        bool   `json:"ok"`
	Reason    string `json:"reason"`
	Detail    string `json:"detail,omitempty"`
}

// DiscoverPin asks one upstream model which internal providers it has and which
// pipeline it speaks.
func (p *Prober) DiscoverPin(ctx context.Context, line Line) PinDiscovery {
	body, err := pinRequestbody(line.Model, pinnedProbeWord, pinAskTokens)
	if err != nil {
		return PinDiscovery{Detail: err.Error()}
	}
	status, answer, err := p.pinSend(ctx, line, body)
	if err != nil {
		return PinDiscovery{Detail: err.Error()}
	}
	if message, failed := upstreamErrorMessage(answer); failed {
		return PinDiscovery{Status: status, Detail: message}
	}
	routing := parsePinRouting(answer)

	// Harvest the providers the upstream would have allowed. The probe name is not
	// a real one, so a gateway that honors the field cannot route the request and
	// says who it could have routed to.
	pipelines := []string{routing.Pipeline}
	if routing.Pipeline == "" {
		pipelines = []string{domain.PinPipelinePlanner, domain.PinPipelineDirect}
	}
	var harvested []string
	for _, pipeline := range pipelines {
		injected, injectErr := transform.ApplyModelPin(body, domain.ModelPin{
			Mode: domain.PinModeStrict, Providers: []string{"__probe__"}, Pipeline: pipeline,
		}, "__probe__")
		if injectErr != nil || len(injected) == 0 {
			continue
		}
		_, harvestAnswer, harvestErr := p.pinSend(ctx, line, injected)
		if harvestErr != nil {
			continue
		}
		harvested = append(harvested, extractAvailableProviders(harvestAnswer, pipeline)...)
		if len(harvested) > 0 {
			break
		}
	}

	// The order is the confidence order: the providers the upstream named when asked
	// who it could use come first for a planner (it enumerated them for this
	// request), while a direct gateway's fallback list is its own routing order and
	// leads there.
	var providers []string
	if routing.Pipeline == domain.PinPipelineDirect {
		providers = mergeUnique(routing.Fallbacks, harvested, routing.Tier0)
	} else {
		providers = mergeUnique(harvested, routing.Fallbacks, routing.Tier0)
	}
	return PinDiscovery{
		OK:           true,
		Pipeline:     routing.Pipeline,
		Providers:    providers,
		LastProvider: routing.LastProvider,
		Status:       status,
	}
}

// ValidatePins asks each named provider, exclusively, whether it answers now.
func (p *Prober) ValidatePins(ctx context.Context, line Line, providers []string, pipeline string) []PinValidation {
	results := make([]PinValidation, 0, len(providers))
	for _, provider := range providers {
		started := p.now()
		body, err := pinRequestbody(line.Model, pinnedProbeWord, pinAskTokens)
		if err != nil {
			results = append(results, PinValidation{Provider: provider, Reason: ReasonBadStatus, Detail: err.Error()})
			continue
		}
		injected, err := transform.ApplyModelPin(body, domain.ModelPin{
			Mode: domain.PinModeStrict, Providers: []string{provider}, Pipeline: pipeline,
		}, provider)
		if err != nil || len(injected) == 0 {
			results = append(results, PinValidation{Provider: provider, Reason: ReasonBadStatus, Detail: "the request body could not be pinned"})
			continue
		}
		status, answer, sendErr := p.pinSend(ctx, line, injected)
		result := PinValidation{
			Provider:  provider,
			Status:    status,
			LatencyMS: p.now().Sub(started).Milliseconds(),
		}
		switch {
		case sendErr != nil:
			result.Reason = ReasonUnreachable
			result.Detail = sendErr.Error()
		case status >= 200 && status < 300:
			result.OK = true
			result.Reason = ReasonOK
		case status == http.StatusUnauthorized || status == http.StatusForbidden:
			result.Reason = ReasonAuthRejected
			result.Detail = fmt.Sprintf("the upstream rejected the credential (HTTP %d)", status)
		case status == http.StatusTooManyRequests:
			result.Reason = ReasonRateLimited
			result.Detail = "the upstream rate limited this provider"
		case status >= 500:
			result.Reason = ReasonUpstreamError
			result.Detail = snippet(answer)
		default:
			result.Reason = ReasonBadStatus
			result.Detail = fmt.Sprintf("HTTP %d: %s", status, snippet(answer))
		}
		results = append(results, result)
	}
	return results
}

// now is the prober's clock, shared with the health probe.
func (p *Prober) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// pinSend posts one probe body to the upstream's chat-completions endpoint and
// reads a bounded answer. The request is bounded on its own, so a provider that
// trickles bytes cannot hold a validation loop open.
func (p *Prober) pinSend(ctx context.Context, line Line, body []byte) (int, []byte, error) {
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = defaultProbeTimeout
	}
	requestContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	target, request, err := p.pinRequest(requestContext, line, body)
	if err != nil {
		return 0, nil, err
	}
	client, err := p.client(line, target)
	if err != nil {
		return 0, nil, err
	}
	response, err := client.Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	answer, _ := io.ReadAll(io.LimitReader(response.Body, maxReadBodyBytes(p.MaxBodyBytes)))
	return response.StatusCode, answer, nil
}

// pinRequest builds one pinned-probe request with the line's own credential,
// fingerprint and configured headers.
func (p *Prober) pinRequest(ctx context.Context, line Line, body []byte) (*url.URL, *http.Request, error) {
	base, err := url.Parse(strings.TrimRight(strings.TrimSpace(line.BaseURL), "/"))
	if err != nil {
		return nil, nil, fmt.Errorf("parse the upstream address: %w", err)
	}
	target := *base
	target.Path = strings.TrimRight(base.Path, "/") + "/chat/completions"

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), bytes.NewReader(body))
	if err != nil {
		return nil, nil, fmt.Errorf("build the pin probe request: %w", err)
	}
	request.Header = transform.ApplyHeadersWithProfile(
		nil,
		clientprofile.Headers(line.ClientProfile),
		line.Transform,
		line.APIKey,
		"application/json",
	)
	return &target, request, nil
}

// pinRequestbody is the small chat request both discovery and validation send.
func pinRequestbody(model, prompt string, maxTokens int) ([]byte, error) {
	body := map[string]any{
		"model":       model,
		"messages":    []map[string]string{{"role": "user", "content": prompt}},
		"stream":      false,
		"temperature": 0,
	}
	if maxTokens > 0 {
		body["max_tokens"] = maxTokens
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encode the pin probe request: %w", err)
	}
	return encoded, nil
}

// pinRouting is what one answer says about the provider that served it.
type pinRouting struct {
	Pipeline     string
	LastProvider string
	Fallbacks    []string
	Tier0        []string
}

// routingMeta is the provider_metadata.gateway.routing block a planner gateway
// attaches to its answer.
type routingMeta struct {
	Gateway struct {
		Routing struct {
			FinalProvider      string   `json:"finalProvider"`
			FallbacksAvailable []string `json:"fallbacksAvailable"`
			PlanningReasoning  string   `json:"planningReasoning"`
		} `json:"routing"`
	} `json:"gateway"`
}

// pinEnvelope is the chat response as these gateways shape it: the routing block
// sits at the top level, or on the first choice, or on its message.
type pinEnvelope struct {
	Data     json.RawMessage `json:"data"`
	Provider string          `json:"provider"`
	Model    string          `json:"model"`
	Choices  []struct {
		Message struct {
			ProviderMetadata *routingMeta `json:"provider_metadata"`
		} `json:"message"`
		ProviderMetadata *routingMeta `json:"provider_metadata"`
	} `json:"choices"`
	ProviderMetadata *routingMeta `json:"provider_metadata"`
}

// parsePinRouting reads the pipeline and provider an answer points at. A planner
// gateway names its provider in provider_metadata.gateway.routing; a direct
// (OpenRouter-shaped) one names it in a top-level provider string. Neither is
// conclusive on its own, so an answer with no routing information yields an empty
// pipeline rather than a guess.
func parsePinRouting(body []byte) pinRouting {
	var envelope pinEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		return pinRouting{}
	}
	if len(envelope.Data) > 0 {
		var inner pinEnvelope
		if json.Unmarshal(envelope.Data, &inner) == nil && len(inner.Choices) > 0 {
			envelope = inner
		}
	}

	routing := pinRouting{}
	meta := envelope.ProviderMetadata
	if len(envelope.Choices) > 0 {
		if message := envelope.Choices[0].Message.ProviderMetadata; message != nil {
			meta = message
		} else if choice := envelope.Choices[0].ProviderMetadata; choice != nil {
			meta = choice
		}
	}
	if meta != nil {
		block := meta.Gateway.Routing
		routing.LastProvider = cleanProviderName(block.FinalProvider)
		routing.Fallbacks = cleanProviderNames(block.FallbacksAvailable)
		routing.Tier0 = parseTier0(block.PlanningReasoning)
	}

	if routing.LastProvider != "" {
		routing.Pipeline = domain.PinPipelinePlanner
		return routing
	}
	if direct := cleanProviderName(envelope.Provider); direct != "" {
		routing.Pipeline = domain.PinPipelineDirect
		routing.LastProvider = direct
	}
	return routing
}

// availableProvidersPattern finds the planner's error sentence that names the
// providers it could have used.
var availableProvidersPattern = regexp.MustCompile(`(?i)available providers are:\s*([^.]+)`)

// providerSplitPattern splits that list: commas, or " and " before the last one.
var providerSplitPattern = regexp.MustCompile(`\s*,\s*|\s+and\s+`)

// plannerSlugPattern is the clean shape a provider name has to have before it is
// kept: the sentence is cut out of a JSON error body, so unguarded it would
// collect fragments such as `","type":"invalid_request_error"`.
var plannerSlugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// tier0Pattern finds the providers a planner considered for tier 0, which are
// providers that certainly exist.
var tier0Pattern = regexp.MustCompile(`([\w-]+) won tier 0 over ([^."]+)`)

// extractAvailableProviders reads the provider list out of an upstream error: a
// planner spells it in the error text, a direct gateway lists it under
// error.metadata.available_providers.
func extractAvailableProviders(body []byte, pipeline string) []string {
	text := string(body)
	if pipeline == domain.PinPipelinePlanner || pipeline == "" {
		if match := availableProvidersPattern.FindStringSubmatch(text); match != nil {
			if providers := plannerProviders(providerSplitPattern.Split(match[1], -1)); len(providers) > 0 {
				return providers
			}
		}
	}
	if pipeline == domain.PinPipelineDirect || pipeline == "" {
		var envelope struct {
			Error struct {
				Metadata struct {
					AvailableProviders []string `json:"available_providers"`
				} `json:"metadata"`
			} `json:"error"`
		}
		if start := strings.Index(text, "{"); start >= 0 && json.Unmarshal([]byte(text[start:]), &envelope) == nil {
			if providers := cleanProviderNames(envelope.Error.Metadata.AvailableProviders); len(providers) > 0 {
				return providers
			}
		}
	}
	return nil
}

// upstreamErrorMessage reports whether an answer is an upstream error rather than
// a served completion, and returns the text that explains it.
func upstreamErrorMessage(body []byte) (string, bool) {
	var payload struct {
		Error   json.RawMessage   `json:"error"`
		Choices []json.RawMessage `json:"choices"`
	}
	if json.Unmarshal(body, &payload) != nil || len(payload.Choices) > 0 {
		return "", false
	}
	raw := payload.Error
	if len(raw) == 0 || string(raw) == "null" {
		return "", false
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		if trimmed := strings.TrimSpace(text); trimmed != "" {
			return trimmed, true
		}
		return "", false
	}
	var described struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &described) == nil {
		if trimmed := strings.TrimSpace(described.Message); trimmed != "" {
			return trimmed, true
		}
	}
	return "", false
}

// parseTier0 reads the providers a planner weighed for tier 0 out of its
// reasoning sentence: "alibaba won tier 0 over baseten and novita".
func parseTier0(reasoning string) []string {
	match := tier0Pattern.FindStringSubmatch(reasoning)
	if match == nil {
		return nil
	}
	tokens := append([]string{match[1]}, providerSplitPattern.Split(match[2], -1)...)
	return plannerProviders(tokens)
}

// plannerProviders renders the tokens of a provider sentence into names, keeping
// only those that are a clean provider name.
func plannerProviders(tokens []string) []string {
	providers := make([]string, 0, len(tokens))
	seen := make(map[string]bool, len(tokens))
	for _, token := range tokens {
		name := cleanProviderName(token)
		if name == "" || seen[name] || !plannerSlugPattern.MatchString(name) {
			continue
		}
		seen[name] = true
		providers = append(providers, name)
	}
	return providers
}

// cleanProviderName folds a provider name into the shape a gateway uses.
func cleanProviderName(value string) string {
	name := strings.ToLower(strings.TrimSpace(value))
	name = strings.TrimSuffix(name, "/")
	return name
}

// cleanProviderNames cleans a list, dropping empties while keeping the order.
func cleanProviderNames(values []string) []string {
	names := make([]string, 0, len(values))
	for _, value := range values {
		if name := cleanProviderName(value); name != "" {
			names = append(names, name)
		}
	}
	return names
}

// mergeUnique joins lists in order, dropping duplicates.
func mergeUnique(lists ...[]string) []string {
	merged := make([]string, 0)
	seen := make(map[string]bool)
	for _, list := range lists {
		for _, value := range list {
			name := cleanProviderName(value)
			if name == "" || seen[name] {
				continue
			}
			seen[name] = true
			merged = append(merged, name)
		}
	}
	return merged
}
