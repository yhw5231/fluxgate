package store

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yhw5231/fluxgate/internal/breaker"
	"github.com/yhw5231/fluxgate/internal/domain"
	"github.com/yhw5231/fluxgate/internal/pattern"
	_ "modernc.org/sqlite"
)

var ErrUnauthorized = errors.New("invalid or expired downstream API key")

// routeQuery loads every configured route. Disabled routes and group routes
// that own no channels directly are included so the configuration snapshot and
// group resolution see the complete picture.
const routeQuery = `SELECT
	id, model_pattern, model_mapping, display_name, route_mode,
	COALESCE(routing_strategy, 'weighted'), COALESCE(enabled, 1)
FROM token_routes
ORDER BY id`

// channelQuery loads every route channel with the account, site and token it
// dispatches through.
const channelQuery = `SELECT
	rc.route_id, rc.id, COALESCE(rc.priority, 0), COALESCE(rc.weight, 10), COALESCE(rc.enabled, 1), rc.source_model, rc.token_id,
	a.id, a.access_token, a.api_token, a.extra_config, COALESCE(a.status, 'active'),
	s.id, s.name, s.url, s.platform, s.forced_upstream_endpoint, s.proxy_url,
	COALESCE(s.use_system_proxy, 0), s.custom_headers, s.status, COALESCE(s.global_weight, 1),
	at.token, at.proxy_url, COALESCE(at.use_system_proxy, 0), COALESCE(at.enabled, 1)
FROM route_channels rc
JOIN accounts a ON a.id = rc.account_id
JOIN sites s ON s.id = a.site_id
LEFT JOIN account_tokens at ON at.id = rc.token_id
ORDER BY rc.route_id, rc.id`

// SQLiteStore reads the existing application configuration and owns only the
// gateway_breaker_states table. A process-local mutex serializes callback-based
// breaker updates; the SQL transaction makes each resulting upsert atomic.
type SQLiteStore struct {
	db *sql.DB
	mu sync.Mutex
}

// sqliteDSN adds a busy timeout so the gateway waits briefly instead of failing
// outright when another process holds a write lock on the configuration
// database. Paths that already look like URIs, or that contain characters this
// builder cannot safely quote, are passed through unchanged.
func sqliteDSN(path string) string {
	if strings.HasPrefix(path, "file:") || strings.ContainsAny(path, "?#") {
		return path
	}
	return "file:" + filepath.ToSlash(path) + "?_pragma=busy_timeout(10000)"
}

// OpenSQLite opens the configuration database and verifies the connection is
// usable. A missing parent directory is created so a first-time deployment only
// needs the configured path. SQLite reports every "cannot open the file"
// condition as the opaque "unable to open database file: out of memory (14)"
// message, so a failed ping is re-diagnosed and reported with the real cause.
func OpenSQLite(path string) (*SQLiteStore, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("SQLite database path is required")
	}
	if err := ensureSQLiteDirectory(path); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", sqliteDSN(path))
	if err != nil {
		return nil, fmt.Errorf("open SQLite database: %w", err)
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		if diagnosis := diagnoseSQLiteOpenFailure(path); diagnosis != "" {
			return nil, fmt.Errorf("ping SQLite database %s: %w: %s", path, err, diagnosis)
		}
		return nil, fmt.Errorf("ping SQLite database %s: %w", path, err)
	}
	return &SQLiteStore{db: db}, nil
}

// ensureSQLiteDirectory creates the database's parent directory so a fresh
// deployment pointed at ../data/hub.db does not fail before SQLite is reached.
func ensureSQLiteDirectory(path string) error {
	dir := filepath.Dir(path)
	if dir == "" || dir == "." {
		return nil
	}
	info, err := os.Stat(dir)
	if err == nil {
		if !info.IsDir() {
			return fmt.Errorf("SQLite database directory %s exists but is not a directory", dir)
		}
		return nil
	}
	if !os.IsNotExist(err) {
		return fmt.Errorf("inspect SQLite database directory %s: %w", dir, err)
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create SQLite database directory %s: %w", dir, err)
	}
	return nil
}

// diagnoseSQLiteOpenFailure explains why SQLite could not open the database
// file. The pure-Go driver reports missing directories, permission problems,
// and path mix-ups all as "unable to open database file: out of memory (14)",
// which hides the real cause, so the filesystem condition is probed here. An
// empty result means the directory and file are accessible and the original
// error should stand on its own.
func diagnoseSQLiteOpenFailure(path string) string {
	info, statErr := os.Stat(path)
	if statErr == nil && info.IsDir() {
		return fmt.Sprintf("%s is a directory, not a SQLite database file", path)
	}
	if statErr == nil {
		file, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			return fmt.Sprintf("the database file %s exists but cannot be opened for reading and writing: %v", path, err)
		}
		_ = file.Close()
		return ""
	}
	dir := filepath.Dir(path)
	probe, err := os.CreateTemp(dir, ".fluxgate-write-check-*")
	if err != nil {
		diagnosis := fmt.Sprintf("the data directory %s is not writable by the current process", dir)
		if uid := os.Getuid(); uid >= 0 {
			diagnosis += fmt.Sprintf(" (uid=%d gid=%d)", uid, os.Getgid())
		}
		diagnosis += "; grant the process write access, e.g. chown 10001:10001 <data directory> for the default Docker image"
		return diagnosis
	}
	name := probe.Name()
	_ = probe.Close()
	_ = os.Remove(name)
	return ""
}

func (s *SQLiteStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// VerifyIntegrity runs a fast consistency check so a damaged database is
// reported at startup instead of being served silently. Concurrent writers
// reaching the same file through a container file share can corrupt SQLite, and
// the affected indexes would otherwise produce subtly wrong routing.
func (s *SQLiteStore) VerifyIntegrity(ctx context.Context) error {
	var result string
	if err := s.db.QueryRowContext(ctx, `PRAGMA quick_check`).Scan(&result); err != nil {
		return fmt.Errorf("check SQLite integrity: %w", err)
	}
	if !strings.EqualFold(strings.TrimSpace(result), "ok") {
		return fmt.Errorf("SQLite database failed its integrity check: %s", result)
	}
	return nil
}

// requiredUpstreamTables lists the configuration tables the gateway reads at
// startup. gateway_breaker_states is excluded because the gateway owns and
// creates that one itself.
var requiredUpstreamTables = []string{
	"settings",
	"proxy_profiles",
	"downstream_api_keys",
	"sites",
	"accounts",
	"account_tokens",
	"token_routes",
	"route_channels",
	"route_group_sources",
}

// VerifyUpstreamSchema rejects a configuration database that does not carry the
// upstream schema. Without this check the gateway would surface the missing
// tables one query at a time as opaque "no such table" SQL errors; here the
// absent tables and the tables that do exist are named, so a wrong or empty
// file placed at the configured path is reported with its fix at startup.
func (s *SQLiteStore) VerifyUpstreamSchema(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'table'`)
	if err != nil {
		return fmt.Errorf("list configuration database tables: %w", err)
	}
	defer rows.Close()
	found := make(map[string]bool)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return fmt.Errorf("scan configuration database table name: %w", err)
		}
		found[name] = true
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("list configuration database tables: %w", err)
	}

	missing := make([]string, 0, len(requiredUpstreamTables))
	for _, table := range requiredUpstreamTables {
		if !found[table] {
			missing = append(missing, table)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	present := make([]string, 0, len(found))
	for name := range found {
		present = append(present, name)
	}
	sort.Strings(present)
	return fmt.Errorf(
		"the configuration database is missing the upstream tables %s but contains %s; place the management server's SQLite database (hub.db) at the configured FLUXGATE_DATABASE_PATH",
		strings.Join(missing, ", "), strings.Join(present, ", "))
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
	if err := s.loadRoutes(ctx, &configuration); err != nil {
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
	// supported_models is an exclusion list of model patterns.
	if err := decodeJSON(supportedModels.String, &key.SupportedModels); err != nil {
		return key, fmt.Errorf("parse supported_models: %w", err)
	}
	if err := decodeJSON(allowedRoutes.String, &key.AllowedRouteIDs); err != nil {
		return key, fmt.Errorf("parse allowed_route_ids: %w", err)
	}
	if err := decodeJSON(excludedSites.String, &key.ExcludedSiteIDs); err != nil {
		return key, fmt.Errorf("parse excluded_site_ids: %w", err)
	}
	credentials, err := parseExcludedCredentials(excludedCredentials.String)
	if err != nil {
		return key, err
	}
	key.ExcludedCredentials = credentials

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
			if id <= 0 || !(multiplier > 0) {
				continue
			}
			key.SiteMultipliers[id] = multiplier
		}
	}
	return key, nil
}

// parseExcludedCredentials decodes the excluded_credential_refs column. Only
// account_token references with all three positive identifiers are accepted, so
// a malformed entry can never widen or narrow selection by accident.
func parseExcludedCredentials(raw string) ([]domain.ExcludedCredential, error) {
	type rawCredential struct {
		Kind      string   `json:"kind"`
		SiteID    *float64 `json:"siteId"`
		AccountID *float64 `json:"accountId"`
		TokenID   *float64 `json:"tokenId"`
	}
	var decoded []rawCredential
	if err := decodeJSON(raw, &decoded); err != nil {
		return nil, fmt.Errorf("parse excluded_credential_refs: %w", err)
	}
	credentials := make([]domain.ExcludedCredential, 0, len(decoded))
	seen := make(map[domain.ExcludedCredential]struct{}, len(decoded))
	for _, entry := range decoded {
		if strings.TrimSpace(entry.Kind) != "account_token" {
			continue
		}
		siteID, siteOK := positiveInt64(entry.SiteID)
		accountID, accountOK := positiveInt64(entry.AccountID)
		tokenID, tokenOK := positiveInt64(entry.TokenID)
		if !siteOK || !accountOK || !tokenOK {
			continue
		}
		credential := domain.ExcludedCredential{
			Kind:      "account_token",
			SiteID:    siteID,
			AccountID: accountID,
			TokenID:   tokenID,
		}
		if _, duplicate := seen[credential]; duplicate {
			continue
		}
		seen[credential] = struct{}{}
		credentials = append(credentials, credential)
		if len(credentials) >= 1000 {
			break
		}
	}
	return credentials, nil
}

func positiveInt64(value *float64) (int64, bool) {
	if value == nil {
		return 0, false
	}
	truncated := int64(*value)
	if truncated <= 0 || float64(truncated) != *value {
		return 0, false
	}
	return truncated, true
}

// loadRoutes builds one domain.Route per token_routes row, with the channels
// that route may serve from. Routes and channels are read separately because a
// group route owns no channels of its own: it draws them from the source routes
// listed in route_group_sources.
func (s *SQLiteStore) loadRoutes(ctx context.Context, configuration *Configuration) error {
	groupSources, err := s.loadGroupSources(ctx)
	if err != nil {
		return err
	}

	routes, err := s.loadRouteRows(ctx, groupSources)
	if err != nil {
		return err
	}
	routeIndex := make(map[int64]int, len(routes))
	for index, route := range routes {
		routeIndex[route.ID] = index
	}

	channels, err := s.loadChannelRows(ctx, routes, routeIndex)
	if err != nil {
		return err
	}

	configuration.Routes = routes
	configuration.Channels = channels
	configuration.Models = domain.ExposedModels(routes)
	return nil
}

func (s *SQLiteStore) loadRouteRows(ctx context.Context, groupSources map[int64][]int64) ([]domain.Route, error) {
	rows, err := s.db.QueryContext(ctx, routeQuery)
	if err != nil {
		return nil, fmt.Errorf("load routes: %w", err)
	}
	defer rows.Close()

	routes := make([]domain.Route, 0)
	for rows.Next() {
		var (
			routeID                              int64
			modelPattern, routingStrategy        string
			modelMapping, displayName, routeMode sql.NullString
			routeEnabled                         int
		)
		if err := rows.Scan(&routeID, &modelPattern, &modelMapping, &displayName, &routeMode, &routingStrategy, &routeEnabled); err != nil {
			return nil, fmt.Errorf("scan route: %w", err)
		}
		mapping, err := parseModelMapping(modelMapping.String)
		if err != nil {
			return nil, fmt.Errorf("parse route %d model_mapping: %w", routeID, err)
		}
		mode := domain.RouteModePattern
		if strings.TrimSpace(routeMode.String) == domain.RouteModeExplicitGroup {
			mode = domain.RouteModeExplicitGroup
		}
		routes = append(routes, domain.Route{
			ID:              routeID,
			ModelPattern:    modelPattern,
			DisplayName:     strings.TrimSpace(displayName.String),
			Mode:            mode,
			ModelMapping:    mapping,
			RoutingStrategy: routingStrategy,
			Enabled:         routeEnabled != 0,
			SourceRouteIDs:  groupSources[routeID],
		})
	}
	return routes, rows.Err()
}

func (s *SQLiteStore) loadChannelRows(ctx context.Context, routes []domain.Route, routeIndex map[int64]int) ([]domain.Channel, error) {
	rows, err := s.db.QueryContext(ctx, channelQuery)
	if err != nil {
		return nil, fmt.Errorf("load route channels: %w", err)
	}
	defer rows.Close()

	channels := make([]domain.Channel, 0)
	for rows.Next() {
		var (
			routeID, channelID, accountID, siteID     int64
			tokenID                                   sql.NullInt64
			priority, weight, channelEnabled          int
			sourceModel, apiToken, accountExtraConfig sql.NullString
			accessToken, accountStatus                string
			siteName, siteURL, platform, siteStatus   string
			forcedEndpoint, siteProxy, customHeaders  sql.NullString
			siteSystemProxy                           int
			siteGlobalWeight                          float64
			accountToken, tokenProxy                  sql.NullString
			tokenSystemProxy, tokenEnabled            int
		)
		if err := rows.Scan(
			&routeID, &channelID, &priority, &weight, &channelEnabled, &sourceModel, &tokenID,
			&accountID, &accessToken, &apiToken, &accountExtraConfig, &accountStatus,
			&siteID, &siteName, &siteURL, &platform, &forcedEndpoint, &siteProxy,
			&siteSystemProxy, &customHeaders, &siteStatus, &siteGlobalWeight,
			&accountToken, &tokenProxy, &tokenSystemProxy, &tokenEnabled,
		); err != nil {
			return nil, fmt.Errorf("scan route channel: %w", err)
		}

		position, known := routeIndex[routeID]
		if !known {
			continue
		}
		route := routes[position]

		credential := accessToken
		if apiToken.Valid && apiToken.String != "" {
			credential = apiToken.String
		}
		if tokenID.Valid && accountToken.Valid && accountToken.String != "" {
			credential = accountToken.String
		}

		channel := domain.Channel{
			ID:                 strconv.FormatInt(channelID, 10),
			Name:               siteName,
			BaseURL:            siteURL,
			APIKey:             credential,
			Priority:           priority,
			Weight:             weight,
			RoutingStrategy:    route.RoutingStrategy,
			SiteProxyURL:       siteProxy.String,
			SiteUseSystemProxy: siteSystemProxy != 0,
			SiteID:             siteID,
			AccountID:          accountID,
			SiteGlobalWeight:   siteGlobalWeight,
			SourceModel:        resolveSourceModel(sourceModel.String, route.ModelPattern),
		}
		switch route.RoutingStrategy {
		case "round_robin":
			channel.BreakerMode = string(breaker.ModeKeyModelCooldown)
		case "stable_first":
			channel.BreakerMode = string(breaker.ModeKeyCooldown)
		default:
			channel.BreakerMode = string(breaker.ModeCooldown)
		}
		if tokenID.Valid {
			id := tokenID.Int64
			channel.TokenID = &id
		}

		var accountProxy struct {
			ProxyURL       string `json:"proxyUrl"`
			UseSystemProxy bool   `json:"useSystemProxy"`
		}
		if strings.TrimSpace(accountExtraConfig.String) != "" {
			if err := json.Unmarshal([]byte(accountExtraConfig.String), &accountProxy); err != nil {
				return nil, fmt.Errorf("parse account %d extra_config: %w", accountID, err)
			}
		}
		channel.ProxyURL = accountProxy.ProxyURL
		channel.UseSystemProxy = accountProxy.UseSystemProxy
		if tokenID.Valid {
			if strings.TrimSpace(tokenProxy.String) != "" {
				channel.ProxyURL = tokenProxy.String
				channel.UseSystemProxy = false
			}
			if tokenSystemProxy != 0 {
				channel.ProxyURL = ""
				channel.UseSystemProxy = true
			}
		}
		channel.Enabled = channelEnabled != 0 && route.Enabled && tokenEnabled != 0 && accountStatus == "active" && siteStatus == "active"

		if strings.TrimSpace(customHeaders.String) != "" {
			var headers map[string]string
			if err := json.Unmarshal([]byte(customHeaders.String), &headers); err != nil {
				return nil, fmt.Errorf("parse site %d custom_headers: %w", siteID, err)
			}
			channel.Transform.SetHeaders = make(map[string][]string, len(headers))
			for name, value := range headers {
				channel.Transform.SetHeaders.Set(name, value)
			}
		}

		routes[position].Channels = append(routes[position].Channels, channel)
		channels = append(channels, channel)
	}
	return channels, rows.Err()
}

// resolveSourceModel mirrors the upstream fallback: a channel on an exact-model
// route inherits that model as its source when the column is empty.
func resolveSourceModel(raw, modelPattern string) string {
	if trimmed := strings.TrimSpace(raw); trimmed != "" {
		return trimmed
	}
	if pattern.IsExact(modelPattern) {
		return strings.TrimSpace(modelPattern)
	}
	return ""
}

func (s *SQLiteStore) loadGroupSources(ctx context.Context) (map[int64][]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT group_route_id, source_route_id FROM route_group_sources ORDER BY group_route_id, source_route_id`)
	if err != nil {
		return nil, fmt.Errorf("load route group sources: %w", err)
	}
	defer rows.Close()
	sources := make(map[int64][]int64)
	for rows.Next() {
		var groupID, sourceID int64
		if err := rows.Scan(&groupID, &sourceID); err != nil {
			return nil, fmt.Errorf("scan route group source: %w", err)
		}
		sources[groupID] = append(sources[groupID], sourceID)
	}
	return sources, rows.Err()
}

// parseModelMapping decodes model_mapping while preserving key order, so
// overlapping patterns resolve deterministically.
func parseModelMapping(raw string) (domain.ModelMapping, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || trimmed == "null" {
		return nil, nil
	}
	decoder := json.NewDecoder(strings.NewReader(trimmed))
	opening, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	if delimiter, ok := opening.(json.Delim); !ok || delimiter != '{' {
		return nil, errors.New("model_mapping must be a JSON object")
	}
	mapping := make(domain.ModelMapping, 0)
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		key, ok := keyToken.(string)
		if !ok {
			return nil, errors.New("model_mapping keys must be strings")
		}
		var value any
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		target, ok := value.(string)
		if !ok || strings.TrimSpace(target) == "" {
			continue
		}
		mapping = append(mapping, domain.ModelMappingEntry{Pattern: key, Target: target})
	}
	return mapping, nil
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
