// Package policy holds the gateway's runtime traffic policy: how a failed
// upstream request is retried, whether another channel may be tried, and how a
// failing channel is cooled down before it is used again.
//
// The policy has two sources. Every value starts from a process default that
// comes from the environment (FLUXGATE_MAX_ATTEMPTS and its neighbours), and an
// operator can override any of them from the console. An override is stored in
// the configuration database's settings table under a gateway.-prefixed key, so
// it is additive to whatever else that shared table holds, it never collides
// with another writer's keys, and deleting the row restores the environment
// default.
package policy

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/yhw5231/fluxgate/internal/breaker"
	"github.com/yhw5231/fluxgate/internal/domain"
)

// Setting keys. Each one is both the stored key and the field name the
// management API reads and writes, so a console form and a hand-written setting
// row mean the same thing.
const (
	KeyFailoverEnabled       = "gateway.failover.enabled"
	KeyFailoverCrossUpstream = "gateway.failover.cross_upstream"

	KeyRetryMaxAttempts           = "gateway.retry.max_attempts"
	KeyRetryMaxAttemptsPerChannel = "gateway.retry.max_attempts_per_channel"
	KeyRetryStatuses              = "gateway.retry.statuses"
	KeyRetryBaseBackoffMS         = "gateway.retry.base_backoff_ms"
	KeyRetryMaxBackoffMS          = "gateway.retry.max_backoff_ms"

	KeyBreakerMode                = "gateway.breaker.mode"
	KeyBreakerThreshold           = "gateway.breaker.threshold"
	KeyBreakerBaseCooldownSeconds = "gateway.breaker.base_cooldown_seconds"
	KeyBreakerMaxCooldownSeconds  = "gateway.breaker.max_cooldown_seconds"
	KeyBreakerCooldownMultiplier  = "gateway.breaker.cooldown_multiplier"

	KeyRequestLogEnabled = "gateway.request_log.enabled"
	KeyRequestLogKeep    = "gateway.request_log.keep"

	maxAttemptsCeiling           = 64
	maxAttemptsPerChannelCeiling = 64
	maxBackoffCeilingMS          = int64(10 * 60 * 1000)
	maxCooldownCeilingSeconds    = int64(7 * 24 * 60 * 60)
	maxRequestLogKeep            = int64(100000)
)

// Kind classifies an editable value so a console can pick a widget and the
// management API can validate without knowing anything else about the field.
type Kind string

const (
	KindBool    Kind = "bool"
	KindInt     Kind = "int"
	KindFloat   Kind = "float"
	KindIntList Kind = "int_list"
	KindChoice  Kind = "choice"
)

// Sections group the fields the way the console presents them.
const (
	SectionFailover   = "failover"
	SectionRetry      = "retry"
	SectionBreaker    = "breaker"
	SectionRequestLog = "request_log"
)

// Field describes one editable policy value.
type Field struct {
	Key     string   `json:"key"`
	Section string   `json:"section"`
	Kind    Kind     `json:"kind"`
	Unit    string   `json:"unit,omitempty"`
	Min     *float64 `json:"min,omitempty"`
	Max     *float64 `json:"max,omitempty"`
	Choices []string `json:"choices,omitempty"`
}

func number(value float64) *float64 { return &value }

// Fields lists the editable policy values in the order the console shows them.
func Fields() []Field {
	return []Field{
		{Key: KeyFailoverEnabled, Section: SectionFailover, Kind: KindBool},
		{Key: KeyFailoverCrossUpstream, Section: SectionFailover, Kind: KindBool},

		{Key: KeyRetryMaxAttempts, Section: SectionRetry, Kind: KindInt, Unit: "count", Min: number(1), Max: number(maxAttemptsCeiling)},
		{Key: KeyRetryMaxAttemptsPerChannel, Section: SectionRetry, Kind: KindInt, Unit: "count", Min: number(1), Max: number(maxAttemptsPerChannelCeiling)},
		{Key: KeyRetryBaseBackoffMS, Section: SectionRetry, Kind: KindInt, Unit: "ms", Min: number(0), Max: number(float64(maxBackoffCeilingMS))},
		{Key: KeyRetryMaxBackoffMS, Section: SectionRetry, Kind: KindInt, Unit: "ms", Min: number(0), Max: number(float64(maxBackoffCeilingMS))},
		{Key: KeyRetryStatuses, Section: SectionRetry, Kind: KindIntList, Unit: "status", Min: number(100), Max: number(599)},

		{Key: KeyBreakerMode, Section: SectionBreaker, Kind: KindChoice, Choices: []string{
			string(breaker.ModeCooldown),
			string(breaker.ModeDisable),
			string(breaker.ModeKeyCooldown),
			string(breaker.ModeKeyModelCooldown),
		}},
		{Key: KeyBreakerThreshold, Section: SectionBreaker, Kind: KindInt, Unit: "count", Min: number(1), Max: number(1000)},
		{Key: KeyBreakerBaseCooldownSeconds, Section: SectionBreaker, Kind: KindInt, Unit: "s", Min: number(1), Max: number(float64(maxCooldownCeilingSeconds))},
		{Key: KeyBreakerMaxCooldownSeconds, Section: SectionBreaker, Kind: KindInt, Unit: "s", Min: number(1), Max: number(float64(maxCooldownCeilingSeconds))},
		{Key: KeyBreakerCooldownMultiplier, Section: SectionBreaker, Kind: KindFloat, Unit: "x", Min: number(1), Max: number(100)},

		{Key: KeyRequestLogEnabled, Section: SectionRequestLog, Kind: KindBool},
		{Key: KeyRequestLogKeep, Section: SectionRequestLog, Kind: KindInt, Unit: "count", Min: number(1), Max: number(float64(maxRequestLogKeep))},
	}
}

// Policy is the complete runtime traffic policy.
type Policy struct {
	Retry      domain.RetryPolicy
	Failover   domain.FailoverPolicy
	Breaker    breaker.Policy
	RequestLog RequestLogPolicy
}

// RequestLogPolicy bounds the gateway's record of the requests it serves. What
// the record holds is what explains a failure after the fact, so it is on by
// default; a gateway that serves enough traffic for the writes to matter can
// switch it off or shorten how far back it reaches.
type RequestLogPolicy struct {
	Enabled bool
	// Keep is how many records the log holds. The newest ones are kept and the
	// rest are dropped as new ones are written.
	Keep int
}

// Default is the policy a gateway runs with when nothing else configures it.
// The environment-backed settings the config package reads start from these
// values, and they are what a value returns to when its override is cleared.
func Default() Policy {
	return Policy{
		Retry: domain.RetryPolicy{
			MaxAttempts:           defaultMaxAttempts,
			MaxAttemptsPerChannel: defaultMaxAttemptsPerChannel,
			RetryStatuses: map[int]struct{}{
				408: {}, 409: {}, 425: {}, 429: {},
				500: {}, 502: {}, 503: {}, 504: {},
			},
			BaseBackoff: defaultBaseBackoff,
			MaxBackoff:  defaultMaxBackoff,
		},
		Failover: domain.FailoverPolicy{Enabled: true, CrossUpstream: true},
		Breaker: breaker.Policy{
			Mode:         breaker.ModeCooldown,
			Threshold:    defaultBreakerThreshold,
			BaseCooldown: defaultBreakerBaseCooldown,
			MaxCooldown:  defaultBreakerMaxCooldown,
			Multiplier:   defaultCooldownMultiplier,
		},
		RequestLog: RequestLogPolicy{Enabled: defaultRequestLogEnabled, Keep: defaultRequestLogKeep},
	}
}

// Built-in defaults. They are deliberately not exported as constants: a caller
// reads them through Default, so changing one cannot leave a caller that
// hard-coded it behind.
const (
	defaultMaxAttempts           = 8
	defaultMaxAttemptsPerChannel = 2
	defaultBreakerThreshold      = 3
	defaultCooldownMultiplier    = 2.0
	defaultRequestLogEnabled     = true
	defaultRequestLogKeep        = 1000
)

var (
	defaultBaseBackoff         = 50 * time.Millisecond
	defaultMaxBackoff          = time.Second
	defaultBreakerBaseCooldown = 30 * time.Second
	defaultBreakerMaxCooldown  = 15 * time.Minute
)

// Values renders the effective policy as the field map the management API
// answers with, keyed by setting key.
func (p Policy) Values() map[string]any {
	return map[string]any{
		KeyFailoverEnabled:       p.Failover.Enabled,
		KeyFailoverCrossUpstream: p.Failover.CrossUpstream,

		KeyRetryMaxAttempts:           int64(p.Retry.MaxAttempts),
		KeyRetryMaxAttemptsPerChannel: int64(p.Retry.MaxAttemptsPerChannel),
		KeyRetryStatuses:              retryStatuses(p.Retry),
		KeyRetryBaseBackoffMS:         p.Retry.BaseBackoff.Milliseconds(),
		KeyRetryMaxBackoffMS:          p.Retry.MaxBackoff.Milliseconds(),

		KeyBreakerMode:                string(p.Breaker.Mode),
		KeyBreakerThreshold:           int64(p.Breaker.Threshold),
		KeyBreakerBaseCooldownSeconds: int64(p.Breaker.BaseCooldown / time.Second),
		KeyBreakerMaxCooldownSeconds:  int64(p.Breaker.MaxCooldown / time.Second),
		KeyBreakerCooldownMultiplier:  p.Breaker.Multiplier,

		KeyRequestLogEnabled: p.RequestLog.Enabled,
		KeyRequestLogKeep:    int64(p.RequestLog.Keep),
	}
}

func retryStatuses(policy domain.RetryPolicy) []int64 {
	statuses := make([]int64, 0, len(policy.RetryStatuses))
	for status := range policy.RetryStatuses {
		statuses = append(statuses, int64(status))
	}
	sort.Slice(statuses, func(i, j int) bool { return statuses[i] < statuses[j] })
	return statuses
}

// FromSettings applies the stored overrides to a policy, which the caller
// builds from the process defaults.
//
// Only keys in a namespace this package owns are read. The settings table is
// shared — the gateway keeps its own bookkeeping in it too, under the same
// gateway. prefix — so a key that is not a policy setting is not this package's
// business and is passed over in silence. A key that *is* in an owned namespace
// and cannot be read is reported and skipped, so one hand-edited row cannot take
// the gateway down at startup.
func FromSettings(settings map[string]string, base Policy) (Policy, []string) {
	applied := base
	warnings := make([]string, 0)
	owned := ownedNamespaces()
	keys := make([]string, 0, len(settings))
	for key := range settings {
		if _, mine := owned[namespace(key)]; mine {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		text := strings.TrimSpace(settings[key])
		if text == "" {
			continue
		}
		next, err := applyText(applied, key, text)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("ignoring setting %s: %s", key, err.Error()))
			continue
		}
		applied = next
	}
	return applied, warnings
}

// ownedNamespaces is the set of key namespaces this package reads. It is derived
// from the fields it declares, so adding a policy setting needs no second
// registration here.
func ownedNamespaces() map[string]struct{} {
	fields := Fields()
	namespaces := make(map[string]struct{}, len(fields))
	for _, field := range fields {
		namespaces[namespace(field.Key)] = struct{}{}
	}
	return namespaces
}

// namespace is the part of a key up to and including its last dot:
// gateway.retry.max_attempts belongs to gateway.retry, and a key with no dot
// beyond the shared prefix — gateway.managed_routes — belongs to no namespace of
// its own.
func namespace(key string) string {
	if index := strings.LastIndex(key, "."); index >= 0 {
		return key[:index+1]
	}
	return key
}

// Apply validates one management value and returns the policy it produces
// together with the text to store. A null or empty value asks for the process
// default, which is stored as an empty string and read back as "no override".
func Apply(current Policy, key string, value any) (Policy, string, error) {
	if _, known := fieldFor(key); !known {
		return current, "", fmt.Errorf("unknown policy setting %q", key)
	}
	text, err := encodeValue(key, value)
	if err != nil {
		return current, "", err
	}
	if text == "" {
		return current, "", nil
	}
	next, err := applyText(current, key, text)
	if err != nil {
		return current, "", err
	}
	return next, text, nil
}

// ConstraintError reports a policy whose fields cannot all hold at once, such
// as a per-channel attempt budget above the total one. It names the field the
// operator has to change.
type ConstraintError struct {
	Key     string
	Message string
}

func (e ConstraintError) Error() string { return e.Message }

// Validate reports whether a policy is internally consistent. It is what keeps
// a combination that can never do what it says — a per-channel attempt budget
// above the total budget, a cooldown ceiling below its own floor — out of the
// database.
func Validate(p Policy) error {
	switch {
	case p.Retry.MaxAttempts < 1 || p.Retry.MaxAttempts > maxAttemptsCeiling:
		return ConstraintError{KeyRetryMaxAttempts, fmt.Sprintf("must be between 1 and %d", maxAttemptsCeiling)}
	case p.Retry.MaxAttemptsPerChannel < 1 || p.Retry.MaxAttemptsPerChannel > maxAttemptsPerChannelCeiling:
		return ConstraintError{KeyRetryMaxAttemptsPerChannel, fmt.Sprintf("must be between 1 and %d", maxAttemptsPerChannelCeiling)}
	case p.Retry.MaxAttemptsPerChannel > p.Retry.MaxAttempts:
		return ConstraintError{KeyRetryMaxAttemptsPerChannel, fmt.Sprintf("must not exceed %s", KeyRetryMaxAttempts)}
	case p.Retry.BaseBackoff < 0 || p.Retry.MaxBackoff < 0:
		return ConstraintError{KeyRetryBaseBackoffMS, "must not be negative"}
	case p.Retry.MaxBackoff < p.Retry.BaseBackoff:
		return ConstraintError{KeyRetryMaxBackoffMS, fmt.Sprintf("must not be less than %s", KeyRetryBaseBackoffMS)}
	case p.Breaker.Threshold < 1:
		return ConstraintError{KeyBreakerThreshold, "must be at least 1"}
	case p.Breaker.BaseCooldown <= 0:
		return ConstraintError{KeyBreakerBaseCooldownSeconds, "must be greater than zero"}
	case p.Breaker.MaxCooldown < p.Breaker.BaseCooldown:
		return ConstraintError{KeyBreakerMaxCooldownSeconds, fmt.Sprintf("must not be less than %s", KeyBreakerBaseCooldownSeconds)}
	case p.Breaker.Multiplier < 1:
		return ConstraintError{KeyBreakerCooldownMultiplier, "must be at least 1"}
	case p.RequestLog.Keep < 1 || p.RequestLog.Keep > int(maxRequestLogKeep):
		return ConstraintError{KeyRequestLogKeep, fmt.Sprintf("must be between 1 and %d", maxRequestLogKeep)}
	case !validBreakerMode(p.Breaker.Mode):
		return ConstraintError{KeyBreakerMode, "must be one of " + strings.Join(fieldChoices(KeyBreakerMode), ", ")}
	}
	return nil
}

// fieldFor resolves a setting key to its description.
func fieldFor(key string) (Field, bool) {
	return FieldByKey(key)
}

// FieldByKey resolves a setting key to its description, which is how a caller
// checks or renders a key it did not define itself.
func FieldByKey(key string) (Field, bool) {
	for _, field := range Fields() {
		if field.Key == key {
			return field, true
		}
	}
	return Field{}, false
}

func fieldChoices(key string) []string {
	field, ok := fieldFor(key)
	if !ok {
		return nil
	}
	return field.Choices
}

// encodeValue validates one value against its field and renders the text stored
// in the settings table. An absent value is rendered as the empty string, which
// means "use the process default".
func encodeValue(key string, value any) (string, error) {
	field, _ := fieldFor(key)
	if value == nil {
		return "", nil
	}
	// A JSON body carries the numbers it can hold as float64, and automation may
	// send a number as a string; both are accepted where the field is numeric.
	switch field.Kind {
	case KindBool:
		flag, err := toBool(value)
		if err != nil {
			return "", fmt.Errorf("%s must be true or false", key)
		}
		return strconv.FormatBool(flag), nil
	case KindInt:
		number, err := toFloat(value)
		if err != nil || number != float64(int64(number)) {
			return "", fmt.Errorf("%s must be a whole number", key)
		}
		return strconv.FormatInt(int64(number), 10), nil
	case KindFloat:
		number, err := toFloat(value)
		if err != nil {
			return "", fmt.Errorf("%s must be a number", key)
		}
		return strconv.FormatFloat(number, 'f', -1, 64), nil
	case KindIntList:
		list, err := toIntList(value)
		if err != nil {
			return "", fmt.Errorf("%s must be a list of whole numbers", key)
		}
		encoded, err := json.Marshal(list)
		if err != nil {
			return "", fmt.Errorf("%s must be a list of whole numbers", key)
		}
		return string(encoded), nil
	case KindChoice:
		text, ok := value.(string)
		if !ok {
			return "", fmt.Errorf("%s must be one of %s", key, strings.Join(field.Choices, ", "))
		}
		text = strings.TrimSpace(text)
		if text == "" {
			return "", nil
		}
		for _, allowed := range field.Choices {
			if strings.EqualFold(allowed, text) {
				return allowed, nil
			}
		}
		return "", fmt.Errorf("%s must be one of %s", key, strings.Join(field.Choices, ", "))
	default:
		return "", fmt.Errorf("unsupported policy field %q", key)
	}
}

// applyText sets one field from the text stored in the settings table.
func applyText(current Policy, key, text string) (Policy, error) {
	field, ok := fieldFor(key)
	if !ok {
		return current, fmt.Errorf("unknown policy setting %q", key)
	}
	value, err := encodeValue(key, rawValue(field.Kind, text))
	if err != nil {
		return current, err
	}
	if value == "" {
		return current, nil
	}
	return setValue(current, key, value)
}

// rawValue converts one stored text into the shape encodeValue validates, so a
// stored row and a management value travel through exactly the same checks.
func rawValue(kind Kind, text string) any {
	switch kind {
	case KindBool:
		flag, err := strconv.ParseBool(strings.TrimSpace(text))
		if err != nil {
			return text
		}
		return flag
	case KindInt:
		number, err := strconv.ParseInt(strings.TrimSpace(text), 10, 64)
		if err != nil {
			return text
		}
		return float64(number)
	case KindFloat:
		number, err := strconv.ParseFloat(strings.TrimSpace(text), 64)
		if err != nil {
			return text
		}
		return number
	case KindIntList:
		var decoded []any
		if err := json.Unmarshal([]byte(text), &decoded); err != nil {
			return text
		}
		return decoded
	default:
		return text
	}
}

// setValue writes one validated field into the policy.
func setValue(current Policy, key, text string) (Policy, error) {
	next := current
	switch key {
	case KeyFailoverEnabled, KeyFailoverCrossUpstream:
		flag, err := strconv.ParseBool(text)
		if err != nil {
			return current, err
		}
		if key == KeyFailoverEnabled {
			next.Failover.Enabled = flag
		} else {
			next.Failover.CrossUpstream = flag
		}
	case KeyRetryMaxAttempts, KeyRetryMaxAttemptsPerChannel:
		number, err := strconv.Atoi(text)
		if err != nil {
			return current, err
		}
		if key == KeyRetryMaxAttempts {
			next.Retry.MaxAttempts = number
		} else {
			next.Retry.MaxAttemptsPerChannel = number
		}
	case KeyRetryStatuses:
		list, err := toIntList(rawValue(KindIntList, text))
		if err != nil {
			return current, err
		}
		next.Retry.RetryStatuses = make(map[int]struct{}, len(list))
		for _, status := range list {
			next.Retry.RetryStatuses[int(status)] = struct{}{}
		}
	case KeyRetryBaseBackoffMS, KeyRetryMaxBackoffMS:
		milliseconds, err := strconv.ParseInt(text, 10, 64)
		if err != nil {
			return current, err
		}
		if key == KeyRetryBaseBackoffMS {
			next.Retry.BaseBackoff = time.Duration(milliseconds) * time.Millisecond
		} else {
			next.Retry.MaxBackoff = time.Duration(milliseconds) * time.Millisecond
		}
	case KeyBreakerMode:
		next.Breaker.Mode = breaker.Mode(text)
	case KeyBreakerThreshold:
		number, err := strconv.Atoi(text)
		if err != nil {
			return current, err
		}
		next.Breaker.Threshold = number
	case KeyBreakerBaseCooldownSeconds, KeyBreakerMaxCooldownSeconds:
		seconds, err := strconv.ParseInt(text, 10, 64)
		if err != nil {
			return current, err
		}
		if key == KeyBreakerBaseCooldownSeconds {
			next.Breaker.BaseCooldown = time.Duration(seconds) * time.Second
		} else {
			next.Breaker.MaxCooldown = time.Duration(seconds) * time.Second
		}
	case KeyBreakerCooldownMultiplier:
		multiplier, err := strconv.ParseFloat(text, 64)
		if err != nil {
			return current, err
		}
		next.Breaker.Multiplier = multiplier
	case KeyRequestLogEnabled:
		flag, err := strconv.ParseBool(text)
		if err != nil {
			return current, err
		}
		next.RequestLog.Enabled = flag
	case KeyRequestLogKeep:
		number, err := strconv.Atoi(text)
		if err != nil {
			return current, err
		}
		next.RequestLog.Keep = number
	default:
		return current, fmt.Errorf("unknown policy setting %q", key)
	}
	return next, nil
}

// fieldBounds returns the numeric range a field accepts.
func fieldBounds(field Field) (float64, float64) {
	minimum, maximum := 0.0, 0.0
	if field.Min != nil {
		minimum = *field.Min
	}
	if field.Max != nil {
		maximum = *field.Max
	}
	return minimum, maximum
}

func toBool(value any) (bool, error) {
	switch typed := value.(type) {
	case bool:
		return typed, nil
	case string:
		return strconv.ParseBool(strings.TrimSpace(typed))
	case float64:
		return typed != 0, nil
	case int64:
		return typed != 0, nil
	default:
		return false, errors.New("not a boolean")
	}
}

func toFloat(value any) (float64, error) {
	switch typed := value.(type) {
	case float64:
		return typed, nil
	case int64:
		return float64(typed), nil
	case int:
		return float64(typed), nil
	case json.Number:
		return typed.Float64()
	case string:
		return strconv.ParseFloat(strings.TrimSpace(typed), 64)
	default:
		return 0, errors.New("not a number")
	}
}

// toIntList accepts a JSON array of numbers (as the API receives it), a
// comma-separated list of codes (as a console input sends it), or the text of
// either form (as the settings table stores it).
func toIntList(value any) ([]int64, error) {
	switch typed := value.(type) {
	case string:
		trimmed := strings.TrimSpace(typed)
		if trimmed == "" {
			return []int64{}, nil
		}
		if strings.HasPrefix(trimmed, "[") {
			var decoded []any
			if err := json.Unmarshal([]byte(trimmed), &decoded); err != nil {
				return nil, err
			}
			return toIntList(decoded)
		}
		parts := strings.Split(trimmed, ",")
		list := make([]int64, 0, len(parts))
		for _, part := range parts {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			number, err := strconv.ParseInt(part, 10, 64)
			if err != nil {
				return nil, err
			}
			list = append(list, number)
		}
		return validateStatuses(list)
	case []any:
		list := make([]int64, 0, len(typed))
		for _, entry := range typed {
			number, err := toFloat(entry)
			if err != nil || number != float64(int64(number)) {
				return nil, errors.New("not a whole number")
			}
			list = append(list, int64(number))
		}
		return validateStatuses(list)
	case []int64:
		return validateStatuses(typed)
	default:
		return nil, errors.New("not a list")
	}
}

func validateStatuses(statuses []int64) ([]int64, error) {
	field, _ := fieldFor(KeyRetryStatuses)
	minimum, maximum := fieldBounds(field)
	seen := make(map[int64]struct{}, len(statuses))
	unique := make([]int64, 0, len(statuses))
	for _, status := range statuses {
		if float64(status) < minimum || float64(status) > maximum {
			return nil, fmt.Errorf("status %d is outside %d-%d", status, int64(minimum), int64(maximum))
		}
		if _, duplicate := seen[status]; duplicate {
			continue
		}
		seen[status] = struct{}{}
		unique = append(unique, status)
	}
	sort.Slice(unique, func(i, j int) bool { return unique[i] < unique[j] })
	return unique, nil
}

func validBreakerMode(mode breaker.Mode) bool {
	switch mode {
	case breaker.ModeCooldown, breaker.ModeDisable, breaker.ModeKeyCooldown, breaker.ModeKeyModelCooldown:
		return true
	default:
		return false
	}
}
