package domain

import (
	"net/http"
	"time"
)

// Channel identifies one upstream route candidate.
type Channel struct {
	ID                 string
	Name               string
	BaseURL            string
	APIKey             string
	Enabled            bool
	Priority           int
	Weight             int
	RoutingStrategy    string
	BreakerMode        string
	ProxyURL           string
	UseSystemProxy     bool
	SiteProxyURL       string
	SiteUseSystemProxy bool
	ModelMapping       map[string]string
	Transform          TransformRules
	RequestTimeout     time.Duration
}

// TransformRules describes protocol-neutral request mutations applied before dispatch.
type TransformRules struct {
	RemoveHeaders []string
	SetHeaders    http.Header
	DeleteJSON    []string
	OverrideJSON  map[string]any
}

// RetryPolicy bounds server-side retries and channel failover.
type RetryPolicy struct {
	MaxAttempts           int
	MaxAttemptsPerChannel int
	RetryStatuses         map[int]struct{}
	BaseBackoff           time.Duration
	MaxBackoff            time.Duration
}

// Request is the protocol-neutral input passed through selection and dispatch.
type Request struct {
	Method  string
	Path    string
	Headers http.Header
	Body    []byte
	Model   string
}

// Attempt describes a single upstream dispatch for observability and selectors.
type Attempt struct {
	Number       int
	ChannelID    string
	ChannelIndex int
	KeyID        string
	Model        string
	BreakerMode  string
	StartedAt    time.Time
}

// Failure captures a failed upstream attempt without exposing it to the downstream client.
type Failure struct {
	Attempt    Attempt
	StatusCode int
	Err        error
	Retryable  bool
}

// Selection records a selected channel and the model sent to that channel.
type Selection struct {
	Channel Channel
	Model   string
}

// Selector supplies failover candidates while excluding channels already exhausted.
type Selector interface {
	Select(model string, excluded map[string]struct{}) (Selection, error)
}

// FailureObserver receives attempt outcomes so future selectors can implement cooldowns.
type FailureObserver interface {
	RecordFailure(failure Failure)
	RecordSuccess(attempt Attempt)
}
