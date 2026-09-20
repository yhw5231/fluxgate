package store

import (
	"context"
	"time"

	"github.com/yhw5231/fluxgate/internal/breaker"
	"github.com/yhw5231/fluxgate/internal/domain"
)

// Store provides read-only access to the existing application configuration
// and lifecycle operations for the Go-owned circuit-breaker state.
type Store interface {
	LoadConfiguration(ctx context.Context) (Configuration, error)
	AuthenticateDownstreamKey(ctx context.Context, key string, now time.Time) (DownstreamAPIKey, error)
	EnsureBreakerSchema(ctx context.Context) error
	LoadBreakerStates(ctx context.Context) (map[breaker.Scope]breaker.State, error)
	UpdateBreakerState(ctx context.Context, scope breaker.Scope, update func(breaker.State) breaker.State) (breaker.State, error)
	CleanupBreakerStates(ctx context.Context, before time.Time) (int64, error)
	Close() error
}

// Configuration is an immutable snapshot loaded from the existing SQLite
// tables. The Go gateway does not migrate or mutate these application tables.
type Configuration struct {
	Routes         []domain.Route
	Channels       []domain.Channel
	Models         []string
	DownstreamKeys []DownstreamAPIKey
	ProxyProfiles  []ProxyProfile
	Settings       map[string]string
	LoadedAt       time.Time
}

// DownstreamAPIKey represents an enabled client credential and its routing
// restrictions without exposing the secret in logs or management responses.
type DownstreamAPIKey struct {
	ID                  int64
	Name                string
	Key                 string
	Enabled             bool
	ExpiresAt           *time.Time
	MaxCost             *float64
	UsedCost            float64
	MaxRequests         *int64
	UsedRequests        int64
	SupportedModels     []string
	AllowedRouteIDs     []int64
	ExcludedSiteIDs     []int64
	SiteMultipliers     map[int64]float64
	ExcludedCredentials []domain.ExcludedCredential
}

// Policy converts the stored columns into the routing policy used for
// selection. supported_models is an exclusion list, so its entries become deny
// patterns.
func (k DownstreamAPIKey) Policy() domain.RoutingPolicy {
	return domain.RoutingPolicy{
		DeniedModelPatterns: k.SupportedModels,
		AllowedRouteIDs:     k.AllowedRouteIDs,
		ExcludedSiteIDs:     k.ExcludedSiteIDs,
		SiteMultipliers:     k.SiteMultipliers,
		ExcludedCredentials: k.ExcludedCredentials,
	}
}

// ProxyProfile represents one configured proxy endpoint.
type ProxyProfile struct {
	ID        int64
	Name      string
	Protocol  string
	URL       string
	IsDefault bool
	Enabled   bool
}
