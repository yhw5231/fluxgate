package store

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yhw5231/fluxgate/internal/breaker"
	"github.com/yhw5231/fluxgate/internal/domain"
	_ "modernc.org/sqlite"
)

var ErrUnauthorized = errors.New("invalid or expired downstream API key")

// SQLiteStore reads the existing application configuration and owns only the
// gateway_breaker_states table. A process-local mutex serializes callback-based
// breaker updates; the SQL transaction makes each resulting upsert atomic.
type SQLiteStore struct {
	db *sql.DB
	mu sync.Mutex
}

func OpenSQLite(path string) (*SQLiteStore, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("SQLite database path is required")
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open SQLite database: %w", err)
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping SQLite database: %w", err)
	}
	return &SQLiteStore{db: db}, nil
}

func (s *SQLiteStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *SQLiteStore) EnsureBreakerSchema(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS gateway_breaker_states (
		scope_key TEXT PRIMARY KEY,
		channel_id TEXT NOT NULL DEFAULT '',
		key_id TEXT NOT NULL DEFAULT '',
		model TEXT NOT NULL DEFAULT '',
		consecutive_failures INTEGER NOT NULL DEFAULT 0,
		cooldown_level INTEGER NOT NULL DEFAULT 0,
		blocked_until TEXT,
		disabled INTEGER NOT NULL DEFAULT 0,
		updated_at TEXT NOT NULL
	)`)
	if err != nil {
		return fmt.Errorf("create gateway_breaker_states: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS gateway_breaker_states_blocked_until_idx ON gateway_breaker_states(blocked_until)`)
	if err != nil {
		return fmt.Errorf("create breaker expiry index: %w", err)
	}
	return nil
}

func (s *SQLiteStore) LoadBreakerStates(ctx context.Context) (map[breaker.Scope]breaker.State, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT channel_id, key_id, model, consecutive_failures, cooldown_level, blocked_until, disabled FROM gateway_breaker_states`)
	if err != nil {
		return nil, fmt.Errorf("load breaker states: %w", err)
	}
	defer rows.Close()
	states := make(map[breaker.Scope]breaker.State)
	for rows.Next() {
		var scope breaker.Scope
		var state breaker.State
		var blocked sql.NullString
		var disabled int
		if err := rows.Scan(&scope.ChannelID, &scope.KeyID, &scope.Model, &state.ConsecutiveFailures, &state.CooldownLevel, &blocked, &disabled); err != nil {
			return nil, fmt.Errorf("scan breaker state: %w", err)
		}
		state.Disabled = disabled != 0
		if blocked.Valid && blocked.String != "" {
			state.BlockedUntil, err = time.Parse(time.RFC3339Nano, blocked.String)
			if err != nil {
				return nil, fmt.Errorf("parse breaker blocked_until: %w", err)
			}
		}
		states[scope] = state
	}
	return states, rows.Err()
}

func (s *SQLiteStore) UpdateBreakerState(ctx context.Context, scope breaker.Scope, update func(breaker.State) breaker.State) (breaker.State, error) {
	if update == nil {
		return breaker.State{}, errors.New("breaker update callback is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return breaker.State{}, fmt.Errorf("begin breaker update: %w", err)
	}
	defer tx.Rollback()
	current, err := loadBreakerState(ctx, tx, scope)
	if err != nil {
		return breaker.State{}, err
	}
	next := update(current)
	var blocked any
	if !next.BlockedUntil.IsZero() {
		blocked = next.BlockedUntil.UTC().Format(time.RFC3339Nano)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO gateway_breaker_states (
		scope_key, channel_id, key_id, model, consecutive_failures, cooldown_level, blocked_until, disabled, updated_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(scope_key) DO UPDATE SET
		channel_id=excluded.channel_id,
		key_id=excluded.key_id,
		model=excluded.model,
		consecutive_failures=excluded.consecutive_failures,
		cooldown_level=excluded.cooldown_level,
		blocked_until=excluded.blocked_until,
		disabled=excluded.disabled,
		updated_at=excluded.updated_at`, scopeKey(scope), scope.ChannelID, scope.KeyID, scope.Model,
		next.ConsecutiveFailures, next.CooldownLevel, blocked, boolInt(next.Disabled), time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return breaker.State{}, fmt.Errorf("upsert breaker state: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return breaker.State{}, fmt.Errorf("commit breaker update: %w", err)
	}
	return next, nil
}

func loadBreakerState(ctx context.Context, tx *sql.Tx, scope breaker.Scope) (breaker.State, error) {
	var state breaker.State
	var blocked sql.NullString
	var disabled int
	err := tx.QueryRowContext(ctx, `SELECT consecutive_failures, cooldown_level, blocked_until, disabled FROM gateway_breaker_states WHERE scope_key = ?`, scopeKey(scope)).Scan(&state.ConsecutiveFailures, &state.CooldownLevel, &blocked, &disabled)
	if errors.Is(err, sql.ErrNoRows) {
		return state, nil
	}
	if err != nil {
		return state, fmt.Errorf("load breaker state for update: %w", err)
	}
	state.Disabled = disabled != 0
	if blocked.Valid && blocked.String != "" {
		state.BlockedUntil, err = time.Parse(time.RFC3339Nano, blocked.String)
		if err != nil {
			return state, fmt.Errorf("parse breaker state for update: %w", err)
		}
	}
	return state, nil
}

func (s *SQLiteStore) CleanupBreakerStates(ctx context.Context, before time.Time) (int64, error) {
	result, err := s.db.ExecContext(ctx, `DELETE FROM gateway_breaker_states WHERE disabled = 0 AND consecutive_failures = 0 AND (blocked_until IS NULL OR blocked_until < ?)`, before.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return 0, fmt.Errorf("cleanup breaker states: %w", err)
	}
	return result.RowsAffected()
}

func (s *SQLiteStore) AuthenticateDownstreamKey(ctx context.Context, candidate string, now time.Time) (DownstreamAPIKey, error) {
	candidate = strings.TrimSpace(candidate)
	if candidate == "" {
		return DownstreamAPIKey{}, ErrUnauthorized
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, key, enabled, expires_at, max_cost, used_cost, max_requests, used_requests, supported_models, allowed_route_ids, site_weight_multipliers, excluded_site_ids, excluded_credential_refs FROM downstream_api_keys WHERE enabled = 1`)
	if err != nil {
		return DownstreamAPIKey{}, fmt.Errorf("query downstream API keys: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		key, err := scanDownstreamKey(rows)
		if err != nil {
			return DownstreamAPIKey{}, err
		}
		if subtle.ConstantTimeCompare([]byte(key.Key), []byte(candidate)) != 1 {
			continue
		}
		if key.ExpiresAt != nil && !now.Before(*key.ExpiresAt) {
			return DownstreamAPIKey{}, ErrUnauthorized
		}
		if key.MaxRequests != nil && key.UsedRequests >= *key.MaxRequests {
			return DownstreamAPIKey{}, ErrUnauthorized
		}
		if key.MaxCost != nil && key.UsedCost >= *key.MaxCost {
			return DownstreamAPIKey{}, ErrUnauthorized
		}
		return key, nil
	}
	if err := rows.Err(); err != nil {
		return DownstreamAPIKey{}, fmt.Errorf("iterate downstream API keys: %w", err)
	}
	return DownstreamAPIKey{}, ErrUnauthorized
}

func (s *SQLiteStore) LoadConfiguration(ctx context.Context) (Configuration, error) {
	configuration := Configuration{Settings: make(map[string]string), LoadedAt: time.Now().UTC()}
	if err := s.loadSettings(ctx, &configuration); err != nil {
		return Configuration{}, err
	}
	if err := s.loadProxyProfiles(ctx, &configuration); err != nil {
		return Configuration{}, err
	}
	if err := s.loadDownstreamKeys(ctx, &configuration); err != nil {
		return Configuration{}, err
	}
	if err := s.loadChannels(ctx, &configuration); err != nil {
		return Configuration{}, err
	}
	return configuration, nil
}

func (s *SQLiteStore) loadSettings(ctx context.Context, configuration *Configuration) error {
	rows, err := s.db.QueryContext(ctx, `SELECT key, COALESCE(value, '') FROM settings`)
	if err != nil {
		return fmt.Errorf("load settings: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return fmt.Errorf("scan setting: %w", err)
		}
		configuration.Settings[key] = value
	}
	return rows.Err()
}

func (s *SQLiteStore) loadProxyProfiles(ctx context.Context, configuration *Configuration) error {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, protocol, url, COALESCE(is_default, 0), COALESCE(enabled, 1) FROM proxy_profiles`)
	if err != nil {
		return fmt.Errorf("load proxy profiles: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var profile ProxyProfile
		var isDefault, enabled int
		if err := rows.Scan(&profile.ID, &profile.Name, &profile.Protocol, &profile.URL, &isDefault, &enabled); err != nil {
			return fmt.Errorf("scan proxy profile: %w", err)
		}
		profile.IsDefault = isDefault != 0
		profile.Enabled = enabled != 0
		configuration.ProxyProfiles = append(configuration.ProxyProfiles, profile)
	}
	return rows.Err()
}

func (s *SQLiteStore) loadDownstreamKeys(ctx context.Context, configuration *Configuration) error {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, key, enabled, expires_at, max_cost, used_cost, max_requests, used_requests, supported_models, allowed_route_ids, site_weight_multipliers, excluded_site_ids, excluded_credential_refs FROM downstream_api_keys`)
	if err != nil {
		return fmt.Errorf("load downstream API keys: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		key, err := scanDownstreamKey(rows)
		if err != nil {
			return err
		}
		configuration.DownstreamKeys = append(configuration.DownstreamKeys, key)
	}
	return rows.Err()
}

func scanDownstreamKey(scanner interface{ Scan(...any) error }) (DownstreamAPIKey, error) {
	var key DownstreamAPIKey
	var enabled int
	var expires sql.NullString
	var maxCost sql.NullFloat64
	var usedCost sql.NullFloat64
	var maxRequests sql.NullInt64
	var usedRequests sql.NullInt64
	var supportedModels, allowedRoutes, multipliers, excludedSites, excludedCredentials sql.NullString
	if err := scanner.Scan(&key.ID, &key.Name, &key.Key, &enabled, &expires, &maxCost, &usedCost, &maxRequests, &usedRequests, &supportedModels, &allowedRoutes, &multipliers, &excludedSites, &excludedCredentials); err != nil {
		return key, fmt.Errorf("scan downstream API key: %w", err)
	}
	key.Enabled = enabled != 0
	key.UsedCost = usedCost.Float64
	key.UsedRequests = usedRequests.Int64
	if maxCost.Valid {
		key.MaxCost = &maxCost.Float64
	}
	if maxRequests.Valid {
		key.MaxRequests = &maxRequests.Int64
	}
	if expires.Valid && expires.String != "" {
		parsed, err := parseTime(expires.String)
		if err != nil {
			return key, fmt.Errorf("parse downstream key expiry: %w", err)
		}
		key.ExpiresAt = &parsed
	}
	if err := decodeJSON(supportedModels.String, &key.SupportedModels); err != nil {
		return key, fmt.Errorf("parse supported_models: %w", err)
	}
	if err := decodeJSON(allowedRoutes.String, &key.AllowedRouteIDs); err != nil {
		return key, fmt.Errorf("parse allowed_route_ids: %w", err)
	}
	if err := decodeJSON(excludedSites.String, &key.ExcludedSiteIDs); err != nil {
		return key, fmt.Errorf("parse excluded_site_ids: %w", err)
	}
	if err := decodeJSON(excludedCredentials.String, &key.ExcludedCredentials); err != nil {
		return key, fmt.Errorf("parse excluded_credential_refs: %w", err)
	}
	var rawMultipliers map[string]float64
	if err := decodeJSON(multipliers.String, &rawMultipliers); err != nil {
		return key, fmt.Errorf("parse site_weight_multipliers: %w", err)
	}
	if len(rawMultipliers) > 0 {
		key.SiteMultipliers = make(map[int64]float64, len(rawMultipliers))
		for rawID, multiplier := range rawMultipliers {
			id, err := strconv.ParseInt(rawID, 10, 64)
			if err != nil {
				return key, fmt.Errorf("parse site multiplier ID: %w", err)
			}
			key.SiteMultipliers[id] = multiplier
		}
	}
	return key, nil
}

func (s *SQLiteStore) loadChannels(ctx context.Context, configuration *Configuration) error {
	rows, err := s.db.QueryContext(ctx, `SELECT
		rc.id, rc.route_id, rc.account_id, rc.token_id, rc.source_model,
		COALESCE(rc.priority, 0), COALESCE(rc.weight, 10), COALESCE(rc.enabled, 1),
		tr.model_pattern, tr.model_mapping, COALESCE(tr.routing_strategy, 'weighted'), COALESCE(tr.enabled, 1),
		a.access_token, a.api_token, a.extra_config, COALESCE(a.status, 'active'),
		s.id, s.name, s.url, s.platform, s.forced_upstream_endpoint, s.proxy_url,
		COALESCE(s.use_system_proxy, 0), s.custom_headers, s.status,
		at.token, at.proxy_url, COALESCE(at.use_system_proxy, 0), COALESCE(at.enabled, 1)
	FROM route_channels rc
	JOIN token_routes tr ON tr.id = rc.route_id
	JOIN accounts a ON a.id = rc.account_id
	JOIN sites s ON s.id = a.site_id
	LEFT JOIN account_tokens at ON at.id = rc.token_id`)
	if err != nil {
		return fmt.Errorf("load route channels: %w", err)
	}
	defer rows.Close()
	modelSet := make(map[string]struct{})
	for rows.Next() {
		var channel domain.Channel
		var channelID, routeID, accountID, siteID int64
		var tokenID sql.NullInt64
		var sourceModel, modelMapping, apiToken, accountExtraConfig, forcedEndpoint, siteProxy, customHeaders, accountToken, tokenProxy sql.NullString
		var priority, weight, channelEnabled, routeEnabled, siteSystemProxy, tokenSystemProxy, tokenEnabled int
		var modelPattern, routingStrategy, accessToken, accountStatus, siteName, siteURL, platform, siteStatus string
		if err := rows.Scan(&channelID, &routeID, &accountID, &tokenID, &sourceModel, &priority, &weight, &channelEnabled,
			&modelPattern, &modelMapping, &routingStrategy, &routeEnabled, &accessToken, &apiToken, &accountExtraConfig, &accountStatus,
			&siteID, &siteName, &siteURL, &platform, &forcedEndpoint, &siteProxy, &siteSystemProxy,
			&customHeaders, &siteStatus, &accountToken, &tokenProxy, &tokenSystemProxy, &tokenEnabled); err != nil {
			return fmt.Errorf("scan route channel: %w", err)
		}
		credential := accessToken
		if apiToken.Valid && apiToken.String != "" {
			credential = apiToken.String
		}
		if tokenID.Valid && accountToken.Valid && accountToken.String != "" {
			credential = accountToken.String
		}
		channel.ID = strconv.FormatInt(channelID, 10)
		channel.Name = siteName
		channel.BaseURL = siteURL
		channel.APIKey = credential
		channel.Priority = priority
		channel.Weight = weight
		channel.RoutingStrategy = routingStrategy
		switch routingStrategy {
		case "round_robin":
			channel.BreakerMode = string(breaker.ModeKeyModelCooldown)
		case "stable_first":
			channel.BreakerMode = string(breaker.ModeKeyCooldown)
		default:
			channel.BreakerMode = string(breaker.ModeCooldown)
		}
		channel.SiteProxyURL = siteProxy.String
		channel.SiteUseSystemProxy = siteSystemProxy != 0
		var accountProxy struct {
			ProxyURL       string `json:"proxyUrl"`
			UseSystemProxy bool   `json:"useSystemProxy"`
		}
		if accountExtraConfig.Valid && strings.TrimSpace(accountExtraConfig.String) != "" {
			if err := json.Unmarshal([]byte(accountExtraConfig.String), &accountProxy); err != nil {
				return fmt.Errorf("parse account %d extra_config: %w", accountID, err)
			}
		}
		channel.ProxyURL = accountProxy.ProxyURL
		channel.UseSystemProxy = accountProxy.UseSystemProxy
		if tokenID.Valid {
			if tokenProxy.Valid && strings.TrimSpace(tokenProxy.String) != "" {
				channel.ProxyURL = tokenProxy.String
				channel.UseSystemProxy = false
			}
			if tokenSystemProxy != 0 {
				channel.ProxyURL = ""
				channel.UseSystemProxy = true
			}
		}
		channel.Enabled = channelEnabled != 0 && routeEnabled != 0 && tokenEnabled != 0 && accountStatus == "active" && siteStatus == "active"
		channel.ModelMapping = make(map[string]string)
		if modelMapping.Valid && modelMapping.String != "" {
			if err := json.Unmarshal([]byte(modelMapping.String), &channel.ModelMapping); err != nil {
				return fmt.Errorf("parse route %d model_mapping: %w", routeID, err)
			}
		}
		if sourceModel.Valid && sourceModel.String != "" {
			channel.ModelMapping[modelPattern] = sourceModel.String
		}
		if customHeaders.Valid && customHeaders.String != "" {
			var headers map[string]string
			if err := json.Unmarshal([]byte(customHeaders.String), &headers); err != nil {
				return fmt.Errorf("parse site %d custom_headers: %w", siteID, err)
			}
			channel.Transform.SetHeaders = make(map[string][]string, len(headers))
			for name, value := range headers {
				channel.Transform.SetHeaders.Set(name, value)
			}
		}
		configuration.Channels = append(configuration.Channels, channel)
		if modelPattern != "" {
			modelSet[modelPattern] = struct{}{}
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	configuration.Models = make([]string, 0, len(modelSet))
	for model := range modelSet {
		configuration.Models = append(configuration.Models, model)
	}
	sort.Strings(configuration.Models)
	return nil
}

func decodeJSON(value string, destination any) error {
	if strings.TrimSpace(value) == "" || value == "null" {
		return nil
	}
	return json.Unmarshal([]byte(value), destination)
}

func parseTime(value string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05", "2006-01-02T15:04:05"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed, nil
		}
	}
	return time.Time{}, fmt.Errorf("unsupported time value %q", value)
}

func scopeKey(scope breaker.Scope) string {
	return scope.ChannelID + "\x00" + scope.KeyID + "\x00" + scope.Model
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
