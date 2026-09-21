package policy

import (
	"strings"
	"testing"
	"time"

	"github.com/yhw5231/fluxgate/internal/breaker"
)

// A console write overrides one value and leaves everything else at the process
// default, and clearing it puts the default back.
func TestApplyOverridesOneValueAndClearingRestoresTheDefault(t *testing.T) {
	base := Default()
	if base.Retry.MaxAttempts != 8 || !base.Failover.Enabled || base.Breaker.Threshold != 3 {
		t.Fatalf("Default() = %+v, want the documented defaults", base)
	}

	updated, text, err := Apply(base, KeyRetryMaxAttempts, float64(3))
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if text != "3" {
		t.Errorf("stored text = %q, want 3", text)
	}
	if updated.Retry.MaxAttempts != 3 {
		t.Errorf("MaxAttempts = %d, want 3", updated.Retry.MaxAttempts)
	}
	if updated.Retry.MaxAttemptsPerChannel != base.Retry.MaxAttemptsPerChannel {
		t.Error("an unrelated value changed")
	}

	// An empty value asks for the process default, which is stored as no row.
	if _, text, err := Apply(updated, KeyRetryMaxAttempts, nil); err != nil || text != "" {
		t.Errorf("Apply(nil) = %q, %v; want an empty value that clears the override", text, err)
	}
}

// The stored text is what a restart reads back, so a value has to survive the
// round trip through the settings table.
func TestFromSettingsReadsBackEveryValue(t *testing.T) {
	stored := map[string]string{
		KeyFailoverEnabled:            "false",
		KeyFailoverCrossUpstream:      "false",
		KeyRetryMaxAttempts:           "6",
		KeyRetryMaxAttemptsPerChannel: "2",
		KeyRetryStatuses:              "[500,502,429]",
		KeyRetryBaseBackoffMS:         "150",
		KeyRetryMaxBackoffMS:          "3000",
		KeyBreakerMode:                "key_model_cooldown",
		KeyBreakerThreshold:           "5",
		KeyBreakerBaseCooldownSeconds: "45",
		KeyBreakerMaxCooldownSeconds:  "600",
		KeyBreakerCooldownMultiplier:  "1.5",
	}
	applied, warnings := FromSettings(stored, Default())
	if len(warnings) != 0 {
		t.Fatalf("FromSettings() warnings = %v, want none", warnings)
	}
	if applied.Failover.Enabled || applied.Failover.CrossUpstream {
		t.Error("failover overrides were not applied")
	}
	if applied.Retry.MaxAttempts != 6 || applied.Retry.MaxAttemptsPerChannel != 2 {
		t.Errorf("attempts = %d/%d, want 6/2", applied.Retry.MaxAttempts, applied.Retry.MaxAttemptsPerChannel)
	}
	if _, ok := applied.Retry.RetryStatuses[429]; !ok {
		t.Errorf("retry statuses = %v, want 429 among them", applied.Retry.RetryStatuses)
	}
	if applied.Retry.BaseBackoff != 150*time.Millisecond || applied.Retry.MaxBackoff != 3*time.Second {
		t.Errorf("backoff = %v/%v, want 150ms/3s", applied.Retry.BaseBackoff, applied.Retry.MaxBackoff)
	}
	if applied.Breaker.Mode != breaker.ModeKeyModelCooldown ||
		applied.Breaker.Threshold != 5 ||
		applied.Breaker.BaseCooldown != 45*time.Second ||
		applied.Breaker.MaxCooldown != 10*time.Minute ||
		applied.Breaker.Multiplier != 1.5 {
		t.Errorf("breaker = %+v, want the stored values", applied.Breaker)
	}

	// A hand-edited row that cannot be read is reported and skipped rather than
	// taking the gateway down at startup.
	applied, warnings = FromSettings(map[string]string{KeyRetryMaxAttempts: "lots"}, Default())
	if len(warnings) != 1 {
		t.Fatalf("warnings = %v, want one report about the unreadable value", warnings)
	}
	if applied.Retry.MaxAttempts != 8 {
		t.Errorf("MaxAttempts = %d, want the default kept", applied.Retry.MaxAttempts)
	}
	if !strings.Contains(warnings[0], KeyRetryMaxAttempts) {
		t.Errorf("warning %q does not name the setting", warnings[0])
	}
}

// Settings that are not the gateway's are left alone: the table is shared.
func TestFromSettingsIgnoresForeignKeys(t *testing.T) {
	applied, warnings := FromSettings(map[string]string{
		"some_other_tool": "1",
		"site_url":        "https://example.test",
	}, Default())
	if len(warnings) != 0 {
		t.Errorf("warnings = %v, want none for foreign keys", warnings)
	}
	if applied.Retry.MaxAttempts != Default().Retry.MaxAttempts {
		t.Error("a foreign key changed the policy")
	}
}

// A combination that cannot work is refused, and reported against the field the
// operator has to change.
func TestValidateReportsTheFieldToChange(t *testing.T) {
	invalid := Default()
	invalid.Retry.MaxAttemptsPerChannel = invalid.Retry.MaxAttempts + 1
	err := Validate(invalid)
	if err == nil {
		t.Fatal("Validate() accepted a per-channel budget above the total budget")
	}
	constraint, ok := err.(ConstraintError)
	if !ok {
		t.Fatalf("Validate() error = %T, want a ConstraintError", err)
	}
	if constraint.Key != KeyRetryMaxAttemptsPerChannel {
		t.Errorf("ConstraintError.Key = %q, want %q", constraint.Key, KeyRetryMaxAttemptsPerChannel)
	}

	cooldown := Default()
	cooldown.Breaker.MaxCooldown = cooldown.Breaker.BaseCooldown - time.Second
	if err := Validate(cooldown); err == nil {
		t.Error("Validate() accepted a cooldown ceiling below its floor")
	}
	multiplier := Default()
	multiplier.Breaker.Multiplier = 0.5
	if err := Validate(multiplier); err == nil {
		t.Error("Validate() accepted a cooldown multiplier below 1")
	}
	mode := Default()
	mode.Breaker.Mode = "sometimes"
	if err := Validate(mode); err == nil {
		t.Error("Validate() accepted an unknown breaker mode")
	}
}

func TestValuesAreLabelledWithTheirSettingKeys(t *testing.T) {
	values := Default().Values()
	for _, field := range Fields() {
		if _, present := values[field.Key]; !present {
			t.Errorf("Values() has no entry for %s", field.Key)
		}
	}
	statuses, ok := values[KeyRetryStatuses].([]int64)
	if !ok || len(statuses) != 8 || statuses[0] != 408 {
		t.Errorf("retry statuses = %v, want the eight defaults in order", values[KeyRetryStatuses])
	}
}

// A console input sends the status codes as a comma-separated string, which has
// to be accepted as readily as the JSON array automation sends.
func TestStatusListAcceptsBothForms(t *testing.T) {
	array, _, err := Apply(Default(), KeyRetryStatuses, []any{float64(500), float64(429)})
	if err != nil {
		t.Fatalf("Apply(array) error = %v", err)
	}
	text, _, err := Apply(Default(), KeyRetryStatuses, "500, 429")
	if err != nil {
		t.Fatalf("Apply(text) error = %v", err)
	}
	if len(array.Retry.RetryStatuses) != len(text.Retry.RetryStatuses) {
		t.Errorf("the two forms disagree: %v and %v", array.Retry.RetryStatuses, text.Retry.RetryStatuses)
	}
	if _, _, err := Apply(Default(), KeyRetryStatuses, "500, abc"); err == nil {
		t.Error("Apply() accepted a non-numeric status code")
	}
	if _, _, err := Apply(Default(), KeyRetryStatuses, float64(99)); err == nil {
		t.Error("Apply() accepted a status code outside the HTTP range")
	}
}

func TestApplyRejectsAnUnknownKey(t *testing.T) {
	if _, _, err := Apply(Default(), "gateway.nope", true); err == nil {
		t.Error("Apply() accepted an unknown setting")
	}
}
