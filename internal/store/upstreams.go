package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// The console manages an upstream as one thing: a name, an address, a weight,
// request headers, its keys and the models it serves. That is five rows in the
// upstream schema — a site, its account, that account's tokens, the routes the
// models reach, and the channels that bind a route to the account — so this file
// describes one composite resource on top of them.
//
// Nothing about the schema changes: the routing engine, the loader and the
// tables it reads stay exactly as they are. The console writes a different shape
// of row, not a different database.
//
// Two consequences are worth stating, because they are what make the model
// work:
//
//   - A model is a route. Selecting a model for an upstream creates the route
//     that exposes it and one channel per key, which is what "the route matches
//     the model automatically" means here. A route the console created is
//     remembered in the settings table, so a route the operator wrote by hand is
//     never pruned by a later edit.
//   - A key is a channel. The per-key selection mode is expressed with the two
//     mechanisms the selector already has: an upstream that prefers the first
//     available key gives its keys descending priorities, and an upstream that
//     rotates gives every key the same priority and the same weight.

const (
	// managedRoutesSetting names the settings row that records which routes the
	// console created for the models upstreams serve.
	managedRoutesSetting = "gateway.managed_routes"

	// upstreamKeyModeAvailableFirst prefers the first key, moving to the next one
	// only when it is unavailable. upstreamKeyModeRoundRobin spreads requests
	// across all keys, which is the default.
	upstreamKeyModeAvailableFirst = "available_first"
	upstreamKeyModeRoundRobin     = "round_robin"

	// upstreamChannelWeight is the weight a generated channel carries. It only
	// orders keys inside one route; an upstream's own weight is the site's
	// global_weight, which the selector multiplies into every channel weight.
	upstreamChannelWeight = 10

	// defaultPlatform is stored for an upstream created from the console. The
	// column is required by the schema and the gateway does not read it.
	defaultPlatform = "openai"
)

// SecretMask replaces a stored credential in a management response. The bullet
// characters cannot occur in a credential, which is what makes the mask
// recognizable when a client sends a row back unchanged.
const SecretMask = "••••"

// MaskSecret renders a stored credential so a client can show that one is set,
// and which one it is, without receiving the value itself.
func MaskSecret(value string) string {
	if value == "" {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= 4 {
		return SecretMask
	}
	return SecretMask + string(runes[len(runes)-4:])
}

func containsSecretMask(value string) bool {
	return strings.Contains(value, SecretMask)
}

// IsMaskedSecret reports whether a submitted value is the mask a previous read
// returned rather than a new credential. An empty value means the same thing:
// the caller is not changing that field.
func IsMaskedSecret(value string) bool {
	trimmed := strings.TrimSpace(value)
	return trimmed == "" || containsSecretMask(trimmed)
}

// upstreamResource is the console's view of one upstream service.
var upstreamResource = Resource{
	Name: "upstreams", Table: "sites", OrderBy: "id",
	Columns: []Column{
		{Name: "name", Kind: KindText, Required: true, MaxLength: 200},
		{Name: "url", Kind: KindText, Required: true, MaxLength: 2048, Validate: validateHTTPURL},
		{Name: "platform", Kind: KindText, MaxLength: 64, DefaultValue: defaultPlatform},
		{Name: "global_weight", Kind: KindReal, Validate: validatePositive},
		{Name: "custom_headers", Kind: KindJSONObject, MaxLength: 8192, Validate: validateStringMap},
		{Name: "proxy_url", Kind: KindText, MaxLength: 2048, Validate: validateProxyURL},
		{Name: "status", Kind: KindText, MaxLength: 32, Choices: []string{"active", "disabled"}},
		// The key list is the account's access token followed by its tokens, in
		// the order the operator sees them. A value that is empty or still
		// carries the mask keeps the stored key at that position, which is what
		// makes an untouched form safe to submit.
		{Name: "keys", Kind: KindJSONArray, Synthetic: true, MaxLength: 65536, Validate: validateStringList},
		{Name: "key_mode", Kind: KindText, Synthetic: true, MaxLength: 32, Choices: []string{upstreamKeyModeAvailableFirst, upstreamKeyModeRoundRobin}},
		{Name: "models", Kind: KindJSONArray, Synthetic: true, MaxLength: 65536, Validate: validateStringList},
		{Name: "model_mapping", Kind: KindJSONObject, Synthetic: true, MaxLength: 32768, Validate: validateModelMapping},
	},
	CheckRow:      checkUpstreamRow,
	Apply:         applyUpstream,
	Decorate:      decorateUpstreams,
	CascadeDelete: cascadeDeleteUpstream,
}

/* ===== Validation ===== */

// checkUpstreamRow validates the fields that are assembled from several tables.
func checkUpstreamRow(ctx context.Context, tx *sql.Tx, id int64, row map[string]any) error {
	if raw, present := row["keys"]; present && raw != nil {
		keys, err := decodeStringArray(raw)
		if err != nil {
			return invalidField("keys", "%s", err.Error())
		}
		if len(keys) == 0 {
			return invalidValue("keys", ReasonRequired, nil, "at least one key is required")
		}
		if id == 0 && IsMaskedSecret(keys[0]) {
			// A create cannot keep a key that is not there yet.
			return invalidValue("keys", ReasonRequired, nil, "the first key must be filled in")
		}
		for _, key := range keys {
			if len(key) > 4096 {
				return invalidValue("keys", ReasonTooLong, map[string]any{"limit": 4096}, "use at most %d bytes per key", 4096)
			}
		}
	}
	if raw, present := row["models"]; present && raw != nil {
		models, err := decodeStringArray(raw)
		if err != nil {
			return invalidField("models", "%s", err.Error())
		}
		seen := make(map[string]struct{}, len(models))
		for _, model := range models {
			if len(model) > 512 {
				return invalidValue("models", ReasonTooLong, map[string]any{"limit": 512}, "use at most %d bytes per model name", 512)
			}
			if _, duplicate := seen[model]; duplicate {
				return invalidField("models", "the model %q is listed twice", model)
			}
			seen[model] = struct{}{}
		}
		// A mapping that names no selected model would be stored and never used,
		// so it is refused rather than silently ignored.
		if rawMapping, present := row["model_mapping"]; present && rawMapping != nil {
			mapping, err := decodeStringMap(rawMapping)
			if err != nil {
				return invalidField("model_mapping", "%s", err.Error())
			}
			for model := range mapping {
				if _, selected := seen[model]; !selected {
					return invalidField("model_mapping", "%q is not one of the selected models", model)
				}
			}
		}
	}
	return nil
}

/* ===== Write path ===== */

// applyUpstream stores the composite fields of an upstream: its keys, the models
// it serves, and the mapping from an exposed model name to the name this
// upstream knows it by.
func applyUpstream(ctx context.Context, tx *sql.Tx, siteID int64, values map[string]any) error {
	accountID, err := ensureUpstreamAccount(ctx, tx, siteID)
	if err != nil {
		return err
	}
	keysChanged := false
	if raw, present := values["keys"]; present {
		if err := replaceUpstreamKeys(ctx, tx, accountID, raw); err != nil {
			return err
		}
		keysChanged = true
	}
	rawModels, modelsPresent := values["models"]
	if modelsPresent {
		if err := replaceUpstreamModels(ctx, tx, accountID, rawModels, values["model_mapping"], values["key_mode"]); err != nil {
			return err
		}
	} else if keysChanged {
		// A key list that changed without the model selection changes how many
		// lines each model has, so the lines are rebuilt around the new keys.
		if err := refreshUpstreamChannels(ctx, tx, accountID, values["key_mode"]); err != nil {
			return err
		}
	}
	return nil
}

// refreshUpstreamChannels rewrites the lines of the models an upstream already
// serves. The model selection is untouched: only the credentials behind it and
// the priority each of them gets.
func refreshUpstreamChannels(ctx context.Context, tx *sql.Tx, accountID int64, rawMode any) error {
	credentials, err := upstreamChannelCredentials(ctx, tx, accountID)
	if err != nil {
		return err
	}
	served, err := upstreamServedRoutes(ctx, tx, accountID)
	if err != nil {
		return err
	}
	mode := upstreamKeyModeFrom(rawMode)
	if mode == "" {
		mode = upstreamKeyModeOfRoutes(served)
	}
	for routeID, route := range served {
		if err := writeUpstreamChannels(ctx, tx, routeID, accountID, credentials, strings.TrimSpace(route.SourceModel), mode); err != nil {
			return err
		}
	}
	return nil
}

// ensureUpstreamAccount returns the account that holds an upstream's keys,
// creating it on the first write. The lowest account id is the one the console
// shows and edits; a second account added through the API is left alone.
func ensureUpstreamAccount(ctx context.Context, tx *sql.Tx, siteID int64) (int64, error) {
	existing, err := upstreamAccountID(ctx, tx, siteID)
	if err != nil {
		return 0, err
	}
	if existing != 0 {
		return existing, nil
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO accounts (site_id, access_token, status) VALUES (?, '', 'active')`, siteID)
	if err != nil {
		return 0, fmt.Errorf("create upstream account: %w", err)
	}
	accountID, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("read upstream account id: %w", err)
	}
	return accountID, nil
}

// replaceUpstreamKeys rewrites the key list of an account. Position decides
// which row a key belongs to: the first key is the account's own access token
// and every later one is a row of its token table, so a list that grows adds
// keys and a list that shrinks retires them.
func replaceUpstreamKeys(ctx context.Context, tx *sql.Tx, accountID int64, raw any) error {
	keys, err := decodeStringArray(raw)
	if err != nil {
		return invalidField("keys", "%s", err.Error())
	}
	if len(keys) == 0 {
		return invalidValue("keys", ReasonRequired, nil, "at least one key is required")
	}
	stored, err := upstreamKeys(ctx, tx, accountID)
	if err != nil {
		return err
	}

	for index, key := range keys {
		if index < len(stored) {
			if IsMaskedSecret(key) {
				continue
			}
			if err := updateUpstreamKey(ctx, tx, stored[index], key); err != nil {
				return err
			}
			continue
		}
		if IsMaskedSecret(key) {
			return invalidValue("keys", ReasonRequired, nil, "key %d needs a value", index+1)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO account_tokens (account_id, token, enabled) VALUES (?, ?, 1)`, accountID, key); err != nil {
			return fmt.Errorf("add upstream key: %w", err)
		}
	}
	for index := len(keys); index < len(stored); index++ {
		if stored[index].OwnAccount {
			// The account's own credential column cannot be dropped; an empty
			// list is refused above, so this row is always filled in.
			continue
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM account_tokens WHERE id = ?`, stored[index].ID); err != nil {
			return fmt.Errorf("remove upstream key: %w", err)
		}
	}
	return nil
}

func updateUpstreamKey(ctx context.Context, tx *sql.Tx, stored upstreamKeyRow, value string) error {
	query := `UPDATE account_tokens SET token = ? WHERE id = ?`
	if stored.OwnAccount {
		query = `UPDATE accounts SET access_token = ? WHERE id = ?`
	}
	if _, err := tx.ExecContext(ctx, query, value, stored.ID); err != nil {
		return fmt.Errorf("update upstream key: %w", err)
	}
	return nil
}

// replaceUpstreamModels makes the routing table match the selection: one route
// per exposed model, one channel per key, and the mapping written onto the
// channel so this upstream receives the model name it knows.
func replaceUpstreamModels(ctx context.Context, tx *sql.Tx, accountID int64, raw any, rawMapping any, rawMode any) error {
	models, err := decodeStringArray(raw)
	if err != nil {
		return invalidField("models", "%s", err.Error())
	}
	mapping := map[string]string{}
	if rawMapping != nil {
		mapping, err = decodeStringMap(rawMapping)
		if err != nil {
			return invalidField("model_mapping", "%s", err.Error())
		}
	}

	credentials, err := upstreamChannelCredentials(ctx, tx, accountID)
	if err != nil {
		return err
	}
	managed, err := managedRoutes(ctx, tx)
	if err != nil {
		return err
	}
	served, err := upstreamServedRoutes(ctx, tx, accountID)
	if err != nil {
		return err
	}
	mode := upstreamKeyModeFrom(rawMode)
	if mode == "" {
		mode = upstreamKeyModeOfRoutes(served)
	}
	strategy := upstreamRoutingStrategy(mode)

	selected := make(map[string]struct{}, len(models))
	for _, model := range models {
		selected[model] = struct{}{}
		routeID, created, err := findOrCreateRoute(ctx, tx, model)
		if err != nil {
			return err
		}
		if created {
			// Only a route this write created is claimed; a route that was
			// already there — written by hand or by an earlier version of the
			// console — keeps its own configuration and is never pruned.
			managed[routeID] = struct{}{}
		}
		if _, owned := managed[routeID]; owned {
			if _, err := tx.ExecContext(ctx, `UPDATE token_routes SET routing_strategy = ? WHERE id = ?`, strategy, routeID); err != nil {
				return fmt.Errorf("set route strategy: %w", err)
			}
		}
		target := upstreamModelTarget(model, mapping)
		if err := writeUpstreamChannels(ctx, tx, routeID, accountID, credentials, target, mode); err != nil {
			return err
		}
	}

	for routeID, route := range served {
		if _, keep := selected[route.Model]; keep {
			continue
		}
		if err := dropUpstreamChannels(ctx, tx, routeID, accountID); err != nil {
			return err
		}
		if _, owned := managed[routeID]; !owned {
			continue
		}
		empty, err := routeHasNoChannels(ctx, tx, routeID)
		if err != nil {
			return err
		}
		if !empty {
			continue
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM token_routes WHERE id = ?`, routeID); err != nil {
			return fmt.Errorf("delete empty route: %w", err)
		}
		delete(managed, routeID)
	}
	return saveManagedRoutes(ctx, tx, managed)
}

// writeUpstreamChannels replaces this account's channels on one route. The
// channel of a key carries the model name the upstream expects, which is the
// mapping target when there is one.
func writeUpstreamChannels(ctx context.Context, tx *sql.Tx, routeID, accountID int64, credentials []upstreamCredential, target, mode string) error {
	if err := dropUpstreamChannels(ctx, tx, routeID, accountID); err != nil {
		return err
	}
	for index, credential := range credentials {
		priority := 0
		if mode == upstreamKeyModeAvailableFirst {
			priority = len(credentials) - 1 - index
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO route_channels (
			route_id, account_id, token_id, source_model, priority, weight, enabled
		) VALUES (?, ?, ?, ?, ?, ?, 1)`,
			routeID, accountID, credential.TokenID, target, priority, upstreamChannelWeight); err != nil {
			return fmt.Errorf("create upstream channel: %w", err)
		}
	}
	return nil
}

func dropUpstreamChannels(ctx context.Context, tx *sql.Tx, routeID, accountID int64) error {
	// The recorded circuits of the lines being replaced go with them: their
	// channel ids are about to be reused by the rows that replace them, and a
	// stale cooldown would block a line that has never failed.
	removed, err := routeChannelIDs(ctx, tx, routeID, accountID)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM route_channels WHERE route_id = ? AND account_id = ?`, routeID, accountID); err != nil {
		return fmt.Errorf("remove upstream channel: %w", err)
	}
	return deleteBreakerStates(ctx, tx, removed)
}

// routeChannelIDs lists the channels one account holds on one route.
func routeChannelIDs(ctx context.Context, db queryer, routeID, accountID int64) ([]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT id FROM route_channels WHERE route_id = ? AND account_id = ?`, routeID, accountID)
	if err != nil {
		return nil, fmt.Errorf("list upstream channels: %w", err)
	}
	defer rows.Close()
	ids := make([]string, 0)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan upstream channel: %w", err)
		}
		ids = append(ids, fmt.Sprintf("%d", id))
	}
	return ids, rows.Err()
}

// upstreamModelTarget is the name the upstream knows a model by. An empty
// mapping target means the upstream uses the exposed name itself.
func upstreamModelTarget(model string, mapping map[string]string) string {
	target := strings.TrimSpace(mapping[model])
	if target == "" {
		return model
	}
	return target
}

// upstreamRoutingStrategy expresses the key mode with the strategy the selector
// and the breaker already read: preferring the first available key cools a key
// down on its own, while rotating keys cools a key for one model.
func upstreamRoutingStrategy(mode string) string {
	if mode == upstreamKeyModeAvailableFirst {
		return "stable_first"
	}
	return "round_robin"
}

// upstreamKeyModeFrom reads the submitted key mode, if the request carries one.
func upstreamKeyModeFrom(raw any) string {
	text, ok := raw.(string)
	if !ok {
		return ""
	}
	switch strings.ToLower(strings.TrimSpace(text)) {
	case upstreamKeyModeAvailableFirst:
		return upstreamKeyModeAvailableFirst
	case upstreamKeyModeRoundRobin:
		return upstreamKeyModeRoundRobin
	default:
		return ""
	}
}

// upstreamKeyModeOfRoutes derives the key mode from the routes an upstream
// already serves, so an edit that does not mention the mode keeps it.
func upstreamKeyModeOfRoutes(served map[int64]upstreamRoute) string {
	for _, route := range served {
		if route.Strategy == "stable_first" {
			return upstreamKeyModeAvailableFirst
		}
	}
	return upstreamKeyModeRoundRobin
}

// findOrCreateRoute resolves the route that exposes a model, creating it when
// no route serves that name yet. A managed route is preferred over a hand-made
// one so the console keeps editing the route it owns, and the report says
// whether this call is what created the route.
func findOrCreateRoute(ctx context.Context, tx *sql.Tx, model string) (int64, bool, error) {
	managed, err := managedRoutes(ctx, tx)
	if err != nil {
		return 0, false, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id FROM token_routes
		WHERE model_pattern = ? AND COALESCE(route_mode, ?) <> ?
		ORDER BY id`, model, "pattern", "explicit_group")
	if err != nil {
		return 0, false, fmt.Errorf("find route for %s: %w", model, err)
	}
	defer rows.Close()
	routeID := int64(0)
	for rows.Next() {
		var candidate int64
		if err := rows.Scan(&candidate); err != nil {
			return 0, false, fmt.Errorf("scan route for %s: %w", model, err)
		}
		if routeID == 0 {
			routeID = candidate
		}
		if _, owned := managed[candidate]; owned {
			routeID = candidate
			break
		}
	}
	if err := rows.Err(); err != nil {
		return 0, false, fmt.Errorf("find route for %s: %w", model, err)
	}
	if routeID != 0 {
		return routeID, false, nil
	}

	result, err := tx.ExecContext(ctx, `INSERT INTO token_routes (model_pattern, route_mode, routing_strategy, enabled) VALUES (?, ?, ?, 1)`,
		model, "pattern", upstreamRoutingStrategy(upstreamKeyModeRoundRobin))
	if err != nil {
		return 0, false, fmt.Errorf("create route for %s: %w", model, err)
	}
	created, err := result.LastInsertId()
	if err != nil {
		return 0, false, fmt.Errorf("read route id for %s: %w", model, err)
	}
	return created, true, nil
}

func routeHasNoChannels(ctx context.Context, tx *sql.Tx, routeID int64) (bool, error) {
	var count int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM route_channels WHERE route_id = ?`, routeID).Scan(&count); err != nil {
		return false, fmt.Errorf("count route channels: %w", err)
	}
	return count == 0, nil
}

/* ===== Managed route ownership ===== */

// managedRoutes reads the route ids the console created. The list is how a
// delete knows which routes it may prune: a route the list does not mention was
// written by hand and is left alone even when it ends up empty.
func managedRoutes(ctx context.Context, db queryer) (map[int64]struct{}, error) {
	var value string
	err := db.QueryRowContext(ctx, `SELECT COALESCE(value, '') FROM settings WHERE key = ?`, managedRoutesSetting).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return map[int64]struct{}{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read managed routes: %w", err)
	}
	ids := map[int64]struct{}{}
	trimmed := strings.TrimSpace(value)
	if trimmed == "" || trimmed == "null" {
		return ids, nil
	}
	var decoded []any
	if err := json.Unmarshal([]byte(trimmed), &decoded); err != nil {
		return nil, fmt.Errorf("parse managed routes: %w", err)
	}
	for _, entry := range decoded {
		number, ok := entry.(float64)
		if !ok || number != float64(int64(number)) || number <= 0 {
			continue
		}
		ids[int64(number)] = struct{}{}
	}
	return ids, nil
}

func saveManagedRoutes(ctx context.Context, tx *sql.Tx, ids map[int64]struct{}) error {
	sorted := make([]int64, 0, len(ids))
	for id := range ids {
		sorted = append(sorted, id)
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	encoded, err := json.Marshal(sorted)
	if err != nil {
		return fmt.Errorf("encode managed routes: %w", err)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, managedRoutesSetting, string(encoded))
	if err != nil {
		return fmt.Errorf("store managed routes: %w", err)
	}
	return nil
}

/* ===== Read path ===== */

// UpstreamKey returns the first key of a stored upstream.
//
// It exists for one caller: the console's model probe, which has to present a
// credential to the upstream while the console itself is only ever shown a mask.
// The value is used for that one outbound request and is never written into a
// response, which is why this reads the stored value rather than reusing the
// masked listing.
func (s *SQLiteStore) UpstreamKey(ctx context.Context, id int64) (string, error) {
	var exists int64
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sites WHERE id = ?`, id).Scan(&exists); err != nil {
		return "", fmt.Errorf("read upstream: %w", err)
	}
	if exists == 0 {
		return "", fmt.Errorf("%w: upstreams %d", ErrResourceNotFound, id)
	}
	accountID, err := upstreamAccountID(ctx, s.db, id)
	if err != nil {
		return "", err
	}
	if accountID == 0 {
		return "", nil
	}
	keys, err := upstreamKeys(ctx, s.db, accountID)
	if err != nil {
		return "", err
	}
	if len(keys) == 0 {
		return "", nil
	}
	return keys[0].Value, nil
}

// decorateUpstreams fills the fields an upstream assembles from its account,
// its keys and the routes it serves.
func decorateUpstreams(ctx context.Context, db queryer, rows []map[string]any) error {
	for _, row := range rows {
		siteID, ok := row["id"].(int64)
		if !ok {
			continue
		}
		accountID, err := upstreamAccountID(ctx, db, siteID)
		if err != nil {
			return err
		}
		if accountID == 0 {
			row["keys"] = []any{}
			row["models"] = []any{}
			row["model_mapping"] = map[string]any{}
			row["key_mode"] = upstreamKeyModeRoundRobin
			continue
		}
		keys, err := upstreamKeys(ctx, db, accountID)
		if err != nil {
			return err
		}
		masked := make([]any, 0, len(keys))
		for _, key := range keys {
			masked = append(masked, MaskSecret(key.Value))
		}
		row["keys"] = masked

		served, err := upstreamServedRoutes(ctx, db, accountID)
		if err != nil {
			return err
		}
		models := make([]string, 0, len(served))
		mapping := map[string]any{}
		for _, route := range served {
			models = append(models, route.Model)
			if target := strings.TrimSpace(route.SourceModel); target != "" && target != route.Model {
				mapping[route.Model] = target
			}
		}
		sort.Strings(models)
		row["models"] = models
		row["model_mapping"] = mapping
		row["key_mode"] = upstreamKeyModeOfRoutes(served)
	}
	return nil
}

func upstreamAccountID(ctx context.Context, db queryer, siteID int64) (int64, error) {
	var accountID int64
	err := db.QueryRowContext(ctx, `SELECT id FROM accounts WHERE site_id = ? ORDER BY id LIMIT 1`, siteID).Scan(&accountID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("find upstream account: %w", err)
	}
	return accountID, nil
}

// upstreamKeyRow is one stored key of an upstream.
type upstreamKeyRow struct {
	ID int64
	// OwnAccount marks the account's own credential column rather than a token
	// row, which is what tells a rewrite which table to update.
	OwnAccount bool
	Value      string
}

// upstreamKeys lists the keys of an account in the order the console presents
// them: the account credential first, then its tokens by id.
func upstreamKeys(ctx context.Context, db queryer, accountID int64) ([]upstreamKeyRow, error) {
	var accessToken string
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(access_token, '') FROM accounts WHERE id = ?`, accountID).Scan(&accessToken); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("read upstream account: %w", err)
	}
	keys := []upstreamKeyRow{{ID: accountID, OwnAccount: true, Value: accessToken}}
	rows, err := db.QueryContext(ctx, `SELECT id, COALESCE(token, '') FROM account_tokens WHERE account_id = ? ORDER BY id`, accountID)
	if err != nil {
		return nil, fmt.Errorf("list upstream keys: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var key upstreamKeyRow
		if err := rows.Scan(&key.ID, &key.Value); err != nil {
			return nil, fmt.Errorf("scan upstream key: %w", err)
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}

// upstreamCredential identifies the credential one generated channel
// dispatches with. A nil token is the account's own credential.
type upstreamCredential struct {
	TokenID *int64
}

// upstreamChannelCredentials lists the credentials an upstream's channels are
// generated for, in the same order as its keys.
func upstreamChannelCredentials(ctx context.Context, db queryer, accountID int64) ([]upstreamCredential, error) {
	keys, err := upstreamKeys(ctx, db, accountID)
	if err != nil {
		return nil, err
	}
	credentials := make([]upstreamCredential, 0, len(keys))
	for _, key := range keys {
		if key.OwnAccount {
			credentials = append(credentials, upstreamCredential{})
			continue
		}
		id := key.ID
		credentials = append(credentials, upstreamCredential{TokenID: &id})
	}
	return credentials, nil
}

// upstreamRoute is one route an upstream serves.
type upstreamRoute struct {
	RouteID     int64
	Model       string
	SourceModel string
	Strategy    string
}

// upstreamServedRoutes lists the models an account reaches today, keyed by the
// route that exposes them.
func upstreamServedRoutes(ctx context.Context, db queryer, accountID int64) (map[int64]upstreamRoute, error) {
	rows, err := db.QueryContext(ctx, `SELECT rc.route_id, COALESCE(r.model_pattern, ''), COALESCE(r.display_name, ''),
			COALESCE(rc.source_model, ''), COALESCE(r.routing_strategy, 'weighted')
		FROM route_channels rc
		JOIN token_routes r ON r.id = rc.route_id
		WHERE rc.account_id = ?
		ORDER BY rc.route_id`, accountID)
	if err != nil {
		return nil, fmt.Errorf("list upstream routes: %w", err)
	}
	defer rows.Close()
	served := map[int64]upstreamRoute{}
	for rows.Next() {
		var route upstreamRoute
		var pattern, displayName string
		if err := rows.Scan(&route.RouteID, &pattern, &displayName, &route.SourceModel, &route.Strategy); err != nil {
			return nil, fmt.Errorf("scan upstream route: %w", err)
		}
		route.Model = strings.TrimSpace(displayName)
		if route.Model == "" {
			route.Model = strings.TrimSpace(pattern)
		}
		served[route.RouteID] = route
	}
	return served, rows.Err()
}

/* ===== Delete path ===== */

// cascadeDeleteUpstream removes everything that belonged to an upstream: its
// channels, its account and keys, and the routes that no longer serve anything.
func cascadeDeleteUpstream(ctx context.Context, tx *sql.Tx, siteID int64) (map[string]int64, error) {
	accounts, err := siteAccountIDs(ctx, tx, siteID)
	if err != nil {
		return nil, err
	}
	removed := map[string]int64{}
	if len(accounts) == 0 {
		return removed, nil
	}
	channelIDs, err := siteChannelIDs(ctx, tx, accounts)
	if err != nil {
		return nil, err
	}
	channels, err := tx.ExecContext(ctx, `DELETE FROM route_channels WHERE account_id IN (SELECT id FROM accounts WHERE site_id = ?)`, siteID)
	if err != nil {
		return nil, fmt.Errorf("delete upstream channels: %w", err)
	}
	if count, err := channels.RowsAffected(); err == nil && count > 0 {
		removed["channels"] = count
	}
	tokens, err := tx.ExecContext(ctx, `DELETE FROM account_tokens WHERE account_id IN (SELECT id FROM accounts WHERE site_id = ?)`, siteID)
	if err != nil {
		return nil, fmt.Errorf("delete upstream keys: %w", err)
	}
	if count, err := tokens.RowsAffected(); err == nil && count > 0 {
		removed["tokens"] = count
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM accounts WHERE site_id = ?`, siteID); err != nil {
		return nil, fmt.Errorf("delete upstream account: %w", err)
	}
	removed["accounts"] = int64(len(accounts))

	routes, err := pruneEmptyManagedRoutes(ctx, tx)
	if err != nil {
		return nil, err
	}
	if routes > 0 {
		removed["routes"] = routes
	}
	// A deleted channel's circuit is meaningless once its id can be reused, so
	// the recorded breaker state goes with it.
	if err := deleteBreakerStates(ctx, tx, channelIDs); err != nil {
		return nil, err
	}
	return removed, nil
}

func siteAccountIDs(ctx context.Context, db queryer, siteID int64) ([]int64, error) {
	rows, err := db.QueryContext(ctx, `SELECT id FROM accounts WHERE site_id = ? ORDER BY id`, siteID)
	if err != nil {
		return nil, fmt.Errorf("list upstream accounts: %w", err)
	}
	defer rows.Close()
	ids := make([]int64, 0, 1)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan upstream account: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// siteChannelIDs lists every channel of a site's accounts, so the circuits they
// recorded can be cleared with them.
func siteChannelIDs(ctx context.Context, db queryer, accountIDs []int64) ([]string, error) {
	ids := make([]string, 0)
	for _, accountID := range accountIDs {
		rows, err := db.QueryContext(ctx, `SELECT id FROM route_channels WHERE account_id = ?`, accountID)
		if err != nil {
			return nil, fmt.Errorf("list upstream channels: %w", err)
		}
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, fmt.Errorf("scan upstream channel: %w", err)
			}
			ids = append(ids, fmt.Sprintf("%d", id))
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, fmt.Errorf("list upstream channels: %w", err)
		}
		rows.Close()
	}
	return ids, nil
}

// pruneEmptyManagedRoutes deletes the routes the console created that no channel
// uses any more, and reports how many went.
func pruneEmptyManagedRoutes(ctx context.Context, tx *sql.Tx) (int64, error) {
	managed, err := managedRoutes(ctx, tx)
	if err != nil {
		return 0, err
	}
	removed := int64(0)
	for routeID := range managed {
		empty, err := routeHasNoChannels(ctx, tx, routeID)
		if err != nil {
			return 0, err
		}
		if !empty {
			continue
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM token_routes WHERE id = ?`, routeID); err != nil {
			return 0, fmt.Errorf("delete empty route: %w", err)
		}
		delete(managed, routeID)
		removed++
	}
	if removed > 0 {
		if err := saveManagedRoutes(ctx, tx, managed); err != nil {
			return 0, err
		}
	}
	return removed, nil
}

// deleteBreakerStates drops the recorded circuits of channels that no longer
// exist. A database the gateway never opened has no breaker table, which is not
// an error here.
func deleteBreakerStates(ctx context.Context, tx *sql.Tx, channelIDs []string) error {
	if len(channelIDs) == 0 {
		return nil
	}
	var present int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'gateway_breaker_states'`).Scan(&present); err != nil {
		return fmt.Errorf("look for the breaker table: %w", err)
	}
	if present == 0 {
		return nil
	}
	for _, channelID := range channelIDs {
		if _, err := tx.ExecContext(ctx, `DELETE FROM gateway_breaker_states WHERE channel_id = ?`, channelID); err != nil {
			return fmt.Errorf("delete breaker state: %w", err)
		}
	}
	return nil
}

/* ===== Field decoding ===== */

// decodeStringArray reads a JSON array of strings that arrived either as its
// stored text, as the decoded list, or as the Go slice a caller built directly.
func decodeStringArray(value any) ([]string, error) {
	if list, ok := value.([]string); ok {
		values := make([]string, 0, len(list))
		for _, entry := range list {
			if trimmed := strings.TrimSpace(entry); trimmed != "" {
				values = append(values, trimmed)
			}
		}
		return values, nil
	}
	list, err := decodeList(value)
	if err != nil {
		return nil, err
	}
	values := make([]string, 0, len(list))
	for _, entry := range list {
		text, ok := entry.(string)
		if !ok {
			return nil, errors.New("every entry must be a string")
		}
		text = strings.TrimSpace(text)
		if text == "" {
			continue
		}
		values = append(values, text)
	}
	return values, nil
}

// decodeStringMap reads a JSON object whose values are strings.
func decodeStringMap(value any) (map[string]string, error) {
	raw := value
	if text, ok := value.(string); ok {
		var decoded map[string]any
		if err := json.Unmarshal([]byte(text), &decoded); err != nil {
			return nil, errors.New("must be a JSON object")
		}
		raw = decoded
	}
	object, ok := raw.(map[string]any)
	if !ok {
		return nil, errors.New("must be a JSON object")
	}
	values := make(map[string]string, len(object))
	for key, entry := range object {
		text, ok := entry.(string)
		if !ok {
			return nil, fmt.Errorf("the value of %q must be a string", key)
		}
		name := strings.TrimSpace(key)
		if name == "" {
			return nil, errors.New("keys must not be empty")
		}
		values[name] = strings.TrimSpace(text)
	}
	return values, nil
}

func decodeList(value any) ([]any, error) {
	raw := value
	if text, ok := value.(string); ok {
		var decoded []any
		if err := json.Unmarshal([]byte(text), &decoded); err != nil {
			return nil, errors.New("must be a JSON array")
		}
		raw = decoded
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, errors.New("must be a JSON array")
	}
	return list, nil
}
