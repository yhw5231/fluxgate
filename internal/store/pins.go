package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/yhw5231/fluxgate/internal/domain"
)

// Internal-provider pins.
//
// An upstream gateway sometimes serves one model from several internal providers
// and picks between them per request. An operator who needs one of them says so
// here, per upstream and model, and the gateway writes that into the request
// body before an attempt leaves.
//
// The pins are a gateway-owned table, like the breaker states and the logs: the
// upstream schema has nowhere to keep them, and they are edited from the
// console. They are also described as one of the console's resources, so the
// generic table and form handle them exactly like the configuration tables
// around them.

// gatewayModelPinDDL creates the pin table.
const gatewayModelPinDDL = `CREATE TABLE IF NOT EXISTS gateway_model_pins (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	site_id INTEGER NOT NULL,
	model TEXT NOT NULL,
	mode TEXT NOT NULL DEFAULT 'strict',
	providers TEXT NOT NULL DEFAULT '[]',
	exclude TEXT NOT NULL DEFAULT '[]',
	sort TEXT NOT NULL DEFAULT '',
	pipeline TEXT NOT NULL DEFAULT '',
	probed_at TEXT,
	last_provider TEXT NOT NULL DEFAULT '',
	updated_at TEXT NOT NULL DEFAULT ''
)`

// EnsureModelPinSchema creates the pin table and its uniqueness rule on a
// database that does not have them yet. One pin per upstream and model is what
// makes "the pin for this model" a question with one answer.
func (s *SQLiteStore) EnsureModelPinSchema(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, gatewayModelPinDDL); err != nil {
		return fmt.Errorf("create gateway_model_pins: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `CREATE UNIQUE INDEX IF NOT EXISTS gateway_model_pins_site_model_idx ON gateway_model_pins(site_id, model)`); err != nil {
		return fmt.Errorf("index gateway_model_pins: %w", err)
	}
	return nil
}

// pinResource is the console's view of one pin. It is a gateway-owned table
// rather than an upstream one, but it is edited like every other table, so the
// generic resource machinery carries it.
var pinResource = Resource{
	Name: "pins", Table: "gateway_model_pins", OrderBy: "site_id, model",
	Columns: []Column{
		{Name: "site_id", Kind: KindInt, Required: true, Validate: validatePositive},
		{Name: "model", Kind: KindText, Required: true, MaxLength: 512},
		// Strict tries each provider on its own; preferred sends the whole order and
		// lets the upstream choose inside it.
		{Name: "mode", Kind: KindText, MaxLength: 16, Choices: []string{domain.PinModeStrict, domain.PinModePreferred}, DefaultValue: domain.PinModeStrict},
		{Name: "providers", Kind: KindJSONArray, MaxLength: 4096, Validate: validateStringList},
		{Name: "exclude", Kind: KindJSONArray, MaxLength: 4096, Validate: validateStringList},
		// An empty value leaves the upstream's own ordering alone; the choices are
		// the three things a gateway can order providers by.
		{Name: "sort", Kind: KindText, MaxLength: 16, Choices: []string{"cost", "ttft", "tps"}},
		{Name: "pipeline", Kind: KindText, MaxLength: 16, Choices: []string{domain.PinPipelineDirect, domain.PinPipelinePlanner}},
		{Name: "probed_at", Kind: KindTime},
		{Name: "last_provider", Kind: KindText, MaxLength: 128},
	},
	CheckRow: checkPinRow,
	Decorate: decoratePins,
}

// checkPinRow validates what spans more than one column: the upstream has to
// exist, and a pin that neither names a provider nor asks for an order would be
// stored and never do anything.
func checkPinRow(ctx context.Context, tx *sql.Tx, _ int64, row map[string]any) error {
	if err := checkReference(ctx, tx, "site_id", "sites", row); err != nil {
		return err
	}
	providers, err := providersOf(row["providers"])
	if err != nil {
		return invalidField("providers", "%s", err.Error())
	}
	sortValue, _ := row["sort"].(string)
	if len(providers) == 0 && strings.TrimSpace(sortValue) == "" {
		return invalidField("providers", "a pin needs at least one provider, or a sort order")
	}
	exclude, err := providersOf(row["exclude"])
	if err != nil {
		return invalidField("exclude", "%s", err.Error())
	}
	if len(providers) > 0 && len(exclude) > 0 {
		kept := 0
		excluded := make(map[string]struct{}, len(exclude))
		for _, name := range exclude {
			excluded[name] = struct{}{}
		}
		for _, name := range providers {
			if _, skip := excluded[name]; !skip {
				kept++
			}
		}
		if kept == 0 {
			return invalidField("exclude", "every provider is excluded, so the pin would do nothing")
		}
	}
	return nil
}

// providersOf decodes an optional JSON array of provider names.
func providersOf(raw any) ([]string, error) {
	switch value := raw.(type) {
	case nil:
		return nil, nil
	case string:
		text := strings.TrimSpace(value)
		if text == "" || text == "null" {
			return nil, nil
		}
		var decoded []string
		if err := json.Unmarshal([]byte(text), &decoded); err != nil {
			return nil, fmt.Errorf("must be a JSON array of provider names")
		}
		return decoded, nil
	default:
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, fmt.Errorf("must be a JSON array of provider names")
		}
		var decoded []string
		if err := json.Unmarshal(encoded, &decoded); err != nil {
			return nil, fmt.Errorf("must be a JSON array of provider names")
		}
		return decoded, nil
	}
}

// decoratePins adds the upstream's name to each pin, so the table shows which
// service a pin belongs to without the operator holding site ids in their head.
func decoratePins(ctx context.Context, db queryer, rows []map[string]any) error {
	if len(rows) == 0 {
		return nil
	}
	names, err := sitemap(ctx, db)
	if err != nil {
		return err
	}
	for _, row := range rows {
		siteID, ok := row["site_id"].(int64)
		if !ok {
			continue
		}
		row["site_name"] = names[siteID]
	}
	return nil
}

// sitemap reads the name of every upstream.
func sitemap(ctx context.Context, db queryer) (map[int64]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT id, name FROM sites`)
	if err != nil {
		return nil, fmt.Errorf("read upstream names: %w", err)
	}
	defer rows.Close()
	names := make(map[int64]string)
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, fmt.Errorf("scan upstream name: %w", err)
		}
		names[id] = name
	}
	return names, rows.Err()
}

// loadModelPins attaches every pin to its upstream. A database that predates the
// table, or a test that never created it, reads as an upstream with no pins
// rather than failing: routing must not depend on an optional table existing.
func (s *SQLiteStore) loadModelPins(ctx context.Context) (map[int64][]domain.ModelPin, error) {
	pins := make(map[int64][]domain.ModelPin)
	rows, err := s.db.QueryContext(ctx, `SELECT site_id, model, COALESCE(mode, ''), COALESCE(providers, '[]'),
		COALESCE(exclude, '[]'), COALESCE(sort, ''), COALESCE(pipeline, ''),
		COALESCE(probed_at, ''), COALESCE(last_provider, '')
		FROM gateway_model_pins ORDER BY site_id, model`)
	if err != nil {
		if strings.Contains(err.Error(), "no such table") {
			return pins, nil
		}
		return nil, fmt.Errorf("load model pins: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			siteID                                                       int64
			model, mode, providersJSON, excludeJSON, sortValue, pipeline string
			probedAt, lastProvider                                       string
		)
		if err := rows.Scan(&siteID, &model, &mode, &providersJSON, &excludeJSON,
			&sortValue, &pipeline, &probedAt, &lastProvider); err != nil {
			return nil, fmt.Errorf("scan model pin: %w", err)
		}
		pin := domain.ModelPin{
			Model:        model,
			Mode:         mode,
			Sort:         sortValue,
			Pipeline:     pipeline,
			LastProvider: lastProvider,
		}
		if err := json.Unmarshal([]byte(providersJSON), &pin.Providers); err != nil {
			return nil, fmt.Errorf("parse pin %d/%s providers: %w", siteID, model, err)
		}
		if err := json.Unmarshal([]byte(excludeJSON), &pin.Exclude); err != nil {
			return nil, fmt.Errorf("parse pin %d/%s exclude: %w", siteID, model, err)
		}
		if strings.TrimSpace(probedAt) != "" {
			if parsed, err := time.Parse(time.RFC3339Nano, probedAt); err == nil {
				pin.ProbedAt = parsed.UTC()
			}
		}
		pins[siteID] = append(pins[siteID], pin)
	}
	return pins, rows.Err()
}

// deletePinsForSite removes every pin of an upstream that is being deleted, so a
// pin can never outlive the upstream it describes. A database that predates the
// table has nothing to delete.
func deletePinsForSite(ctx context.Context, tx *sql.Tx, siteID int64) (int64, error) {
	result, err := tx.ExecContext(ctx, `DELETE FROM gateway_model_pins WHERE site_id = ?`, siteID)
	if err != nil {
		if strings.Contains(err.Error(), "no such table") {
			return 0, nil
		}
		return 0, fmt.Errorf("delete upstream pins: %w", err)
	}
	removed, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count deleted upstream pins: %w", err)
	}
	return removed, nil
}

// DeleteModelPin removes one upstream's pin for one model, which is how an
// operator unpins it. It reports whether a pin was there.
func (s *SQLiteStore) DeleteModelPin(ctx context.Context, siteID int64, model string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	result, err := s.db.ExecContext(ctx,
		`DELETE FROM gateway_model_pins WHERE site_id = ? AND model = ? COLLATE NOCASE`,
		siteID, strings.TrimSpace(model))
	if err != nil {
		return false, fmt.Errorf("delete model pin: %w", err)
	}
	removed, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("count deleted model pins: %w", err)
	}
	return removed > 0, nil
}

// SavePinProbe records what a pin probe found: the pipeline the upstream speaks,
// the providers it named, and which one served the probe. The configured order
// is not overwritten unless the probe discovered one and the pin had none.
func (s *SQLiteStore) SavePinProbe(ctx context.Context, siteID int64, model, pipeline string, providers []string, lastProvider string, at time.Time) error {
	encoded, err := json.Marshal(providers)
	if err != nil {
		return fmt.Errorf("encode discovered providers: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	updatedAt := at.UTC().Format(time.RFC3339Nano)
	if _, err := s.db.ExecContext(ctx, `UPDATE gateway_model_pins
		SET pipeline = ?, last_provider = ?, probed_at = ?, updated_at = ?,
		    providers = CASE WHEN providers IS NULL OR providers = '' OR providers = '[]' THEN ? ELSE providers END
		WHERE site_id = ? AND model = ? COLLATE NOCASE`,
		pipeline, lastProvider, updatedAt, updatedAt, string(encoded), siteID, strings.TrimSpace(model)); err != nil {
		return fmt.Errorf("save pin probe: %w", err)
	}
	return nil
}
