package api

import (
	"errors"
	"net/http"
	"sort"

	"github.com/yhw5231/fluxgate/internal/policy"
	"github.com/yhw5231/fluxgate/internal/store"
)

// Runtime policy.
//
//	PUT /management/policy
//
// The policy is what the gateway does when an upstream fails: how often it
// retries, whether it moves to another channel or another upstream, and how long
// a failing channel is held out of rotation. It lives in the settings table, so
// a change is stored the same way a configuration row is, and a change takes
// effect for the next request rather than at the next restart.
//
// Only the values present in a request change. A value sent as null or as an
// empty string clears its override, which puts the process default back.

// handlePolicyUpdate stores the submitted runtime policy values.
func (s *Server) handlePolicyUpdate(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeConfigurationWrite(w, r) {
		return
	}
	body, ok := decodeConfigurationBody(w, r)
	if !ok {
		return
	}
	submitted, ok := policyPayload(w, body)
	if !ok {
		return
	}
	if len(submitted) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_configuration", "the request does not set any policy value")
		return
	}

	current := s.currentPolicy()
	stored := make(map[string]string, len(submitted))
	// The keys are sorted so a request that sets several values is validated and
	// applied in a stable order: a cross-field rule such as "per channel
	// attempts must not exceed total attempts" can then be reported against the
	// same field every time.
	keys := make([]string, 0, len(submitted))
	for key := range submitted {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if _, known := policy.FieldByKey(key); !known {
			writeConfigurationError(w, http.StatusBadRequest, "invalid_configuration", "unknown policy setting", map[string]any{
				"field":  key,
				"reason": store.ReasonUnknownField,
				"params": map[string]any{"resource": "policy"},
			})
			return
		}
		updated, text, err := policy.Apply(current, key, submitted[key])
		if err != nil {
			writePolicyFieldError(w, key, err)
			return
		}
		stored[key] = text
		if text != "" {
			current = updated
		}
	}
	if err := policy.Validate(current); err != nil {
		writePolicyFieldError(w, "", err)
		return
	}
	if err := s.ConfigStore.PutSettings(r.Context(), stored); err != nil {
		s.writeConfigurationFailure(w, r, err)
		return
	}
	s.logConfigurationChange(r, "console_policy_updated", "policy", 0)
	s.respondWithWrite(w, r, http.StatusOK, map[string]any{"policy": current.Values()})
}

// policyPayload reads the "settings" object of a policy request.
func policyPayload(w http.ResponseWriter, body map[string]any) (map[string]any, bool) {
	raw, present := body["settings"]
	if !present {
		// A flat body is accepted too, so a caller that sends the values at the
		// top level does not have to wrap them.
		return withoutReservedKeys(body), true
	}
	settings, ok := raw.(map[string]any)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_configuration", "settings must be a JSON object")
		return nil, false
	}
	return settings, true
}

// withoutReservedKeys drops the envelope keys a flat body may carry.
func withoutReservedKeys(body map[string]any) map[string]any {
	values := make(map[string]any, len(body))
	for key, value := range body {
		if key == "settings" || key == "id" {
			continue
		}
		values[key] = value
	}
	return values
}

// writePolicyFieldError answers a rejected policy value, naming the setting it
// belongs to so the console can point at the input.
func writePolicyFieldError(w http.ResponseWriter, key string, err error) {
	var constraint policy.ConstraintError
	if errors.As(err, &constraint) {
		key = constraint.Key
	}
	var invalid store.ValidationError
	if errors.As(err, &invalid) {
		writeConfigurationError(w, http.StatusBadRequest, "invalid_configuration", invalid.Message, map[string]any{
			"field":  invalid.Field,
			"reason": invalid.Reason,
			"params": invalid.Params,
		})
		return
	}
	details := map[string]any{}
	if key != "" {
		details["field"] = key
		if field, known := policy.FieldByKey(key); known && len(field.Choices) > 0 {
			details["reason"] = store.ReasonNotAllowed
			details["params"] = map[string]any{"choices": field.Choices}
		}
	}
	writeConfigurationError(w, http.StatusBadRequest, "invalid_configuration", err.Error(), details)
}
