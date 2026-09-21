package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Management writes to the upstream configuration.
//
// The gateway reads the upstream tables and, before the console could edit
// anything, never wrote them. Each editable table is therefore described once,
// below: its columns, how a value of each column is validated and stored, the
// references that must stay intact, and what a delete has to remove or refuse.
// List, create, update and delete are then driven from that description, so the
// SQL, the validation and the field metadata the console renders its forms from
// cannot drift apart as columns are added.
//
// Nothing here trusts its input. A management request is authenticated, but its
// body is still just JSON, so every value is normalized to the column's type,
// bounded in length, and checked against the rows it points at before the write
// happens.

// ErrUnknownResource reports a request for a resource the gateway does not manage.
var ErrUnknownResource = errors.New("unknown configuration resource")

// ErrResourceNotFound reports a read, update, or delete of a row that is gone.
var ErrResourceNotFound = errors.New("configuration row not found")

// ErrDuplicateValue reports a write that would create a second row with a value
// that has to stay unique, such as a downstream API key.
var ErrDuplicateValue = errors.New("value must be unique across existing rows")

// ErrResourceReferenced reports a delete refused because other configuration
// still points at the row. The operator deletes the dependents first, which
// keeps one click from silently unwiring a working gateway.
var ErrResourceReferenced = errors.New("configuration is still referenced by other rows")

// ReferencedError describes a refused delete: what was being deleted, which
// resource still points at it, and how many rows do. The counts travel as data
// so a client can phrase the refusal in its own language rather than parsing an
// English sentence.
type ReferencedError struct {
	// Subject names what was being deleted, such as "site".
	Subject string
	// Resource names the resource that still refers to it.
	Resource string
	// Count is how many rows of that resource still refer to it.
	Count int64
}

func (e ReferencedError) Error() string {
	return fmt.Sprintf("%s: %d row(s) in %s still reference the %s", ErrResourceReferenced, e.Count, e.Resource, e.Subject)
}

// Unwrap makes errors.Is(err, ErrResourceReferenced) true for a typed error.
func (e ReferencedError) Unwrap() error { return ErrResourceReferenced }

// ValidationError describes a rejected value. Field names the JSON field when
// the fault belongs to one, so the console can highlight the input that has to
// change; it is empty for a fault in the request as a whole.
//
// Reason is a stable code for the faults an operator actually causes by typing,
// with Params carrying what its message template needs. The console renders
// those in its own language, while Message stays an English sentence that stands
// on its own for every other client.
type ValidationError struct {
	Field   string
	Reason  string
	Params  map[string]any
	Message string
}

func (e ValidationError) Error() string {
	if e.Field == "" {
		return e.Message
	}
	return e.Field + ": " + e.Message
}

// Reasons a rejected field can carry.
const (
	// ReasonRequired means the field must carry a value.
	ReasonRequired = "required"
	// ReasonTooLong means a text value exceeds its byte bound.
	ReasonTooLong = "too_long"
	// ReasonNotAllowed means the value is outside the column's fixed set.
	ReasonNotAllowed = "not_allowed"
	// ReasonPositive means a number has to be greater than zero.
	ReasonPositive = "must_be_positive"
	// ReasonMissingReference means an id does not name an existing row.
	ReasonMissingReference = "missing_reference"
	// ReasonReferenceMismatch means two references disagree, such as a token
	// that belongs to a different account than the channel names.
	ReasonReferenceMismatch = "reference_mismatch"
	// ReasonUnknownField means the request named a field the resource lacks.
	ReasonUnknownField = "unknown_field"
)

// invalidField builds a field-scoped validation error that carries no reason.
func invalidField(field, format string, args ...any) error {
	return ValidationError{Field: field, Message: fmt.Sprintf(format, args...)}
}

// invalidValue builds a field-scoped validation error with a stable reason the
// console can render in its own language.
func invalidValue(field, reason string, params map[string]any, format string, args ...any) error {
	return ValidationError{Field: field, Reason: reason, Params: params, Message: fmt.Sprintf(format, args...)}
}

// Kind classifies a column so a management write can be validated and stored
// without a hand-written handler per table.
type Kind string

const (
	// KindText is a plain string.
	KindText Kind = "text"
	// KindInt is a whole number.
	KindInt Kind = "int"
	// KindReal is a floating-point number.
	KindReal Kind = "real"
	// KindBool is a 0/1 flag.
	KindBool Kind = "bool"
	// KindTime is a nullable timestamp, stored in RFC 3339.
	KindTime Kind = "time"
	// KindJSONArray is a JSON list, stored as its compacted text.
	KindJSONArray Kind = "json_array"
	// KindJSONObject is a JSON object, stored as its compacted text so an
	// author-declared key order is preserved exactly as written.
	KindJSONObject Kind = "json_object"
	// KindJSON is any JSON value, stored as its compacted text.
	KindJSON Kind = "json"
)

// defaultMaxTextLength bounds a text column that declares no bound of its own.
const defaultMaxTextLength = 1024

// Column describes one writable field of a resource.
type Column struct {
	// Name is both the SQL column and the JSON field name.
	Name string
	Kind Kind
	// Required rejects a create that omits the column and an update that clears it.
	Required bool
	// Secret is masked in management responses. An update that sends an empty
	// value keeps the stored one, so a mask is never written back over a
	// credential.
	Secret bool
	// Clearable marks an optional secret an update may erase by sending null.
	Clearable bool
	// MaxLength bounds a text value so a hostile request cannot write unbounded
	// data into the configuration database.
	MaxLength int
	// Choices restricts a text value to a fixed set. It is empty when any value
	// is acceptable, which is the right default for a column the gateway only
	// ever compares against one known value.
	Choices []string
	// Default is the value a flag takes when a create omits it, which is what the
	// console shows for an untouched checkbox. It is only meaningful for
	// KindBool and mirrors the column default in the schema.
	Default bool
	// Synthetic marks a field that is not a column. A request may carry it and
	// Apply stores it, but it is never part of the row itself.
	Synthetic bool
	// Validate inspects the normalized value: a string for text and time, an
	// int64, a float64, a bool, or the decoded JSON for the JSON kinds.
	Validate func(value any) error
}

// Resource describes one editable table.
type Resource struct {
	// Name is the identifier the management API addresses the resource by.
	Name string
	// Table is the SQL table the columns live in.
	Table string
	// OrderBy is appended to every list query.
	OrderBy string
	// Columns lists the writable fields in the order the console renders them.
	Columns []Column
	// CheckRow receives the row as it will be stored — the existing row with the
	// request applied — and may reject it. It is where a reference to another
	// table and a rule spanning two columns are enforced.
	CheckRow func(ctx context.Context, tx *sql.Tx, id int64, row map[string]any) error
	// Apply stores the synthetic fields of a request once the row itself is written.
	Apply func(ctx context.Context, tx *sql.Tx, id int64, values map[string]any) error
	// GuardDelete refuses a delete that would leave other configuration pointing
	// at a missing row.
	GuardDelete func(ctx context.Context, tx *sql.Tx, id int64) error
	// CascadeDelete removes rows owned by the deleted one after the guard passes.
	// It reports how many rows of each resource it removed, so the console can
	// say what a delete took with it.
	CascadeDelete func(ctx context.Context, tx *sql.Tx, id int64) (map[string]int64, error)
}

// resources is the editable configuration surface in the order the console
// presents it: what upstreams the gateway talks to, what it talks to them with,
// how models reach them, and what clients present to the gateway.
var resources = []Resource{
	{
		Name: "sites", Table: "sites", OrderBy: "id",
		Columns: []Column{
			{Name: "name", Kind: KindText, Required: true, MaxLength: 200},
			{Name: "url", Kind: KindText, Required: true, MaxLength: 2048, Validate: validateHTTPURL},
			{Name: "platform", Kind: KindText, Required: true, MaxLength: 64},
			{Name: "status", Kind: KindText, MaxLength: 32, Validate: validateToken},
			{Name: "global_weight", Kind: KindReal, Validate: validatePositive},
			{Name: "proxy_url", Kind: KindText, MaxLength: 2048, Validate: validateProxyURL},
			{Name: "use_system_proxy", Kind: KindBool},
			{Name: "custom_headers", Kind: KindJSONObject, MaxLength: 8192, Validate: validateStringMap},
			{Name: "forced_upstream_endpoint", Kind: KindText, MaxLength: 2048, Validate: validateHTTPURL},
		},
		GuardDelete: func(ctx context.Context, tx *sql.Tx, id int64) error {
			return guardNoReferences(ctx, tx, id, "site", []reference{
				{Table: "accounts", Column: "site_id", Resource: "accounts"},
			})
		},
	},
	{
		Name: "accounts", Table: "accounts", OrderBy: "id",
		Columns: []Column{
			{Name: "site_id", Kind: KindInt, Required: true, Validate: validatePositive},
			{Name: "access_token", Kind: KindText, Required: true, Secret: true, MaxLength: 4096},
			{Name: "api_token", Kind: KindText, Secret: true, Clearable: true, MaxLength: 4096},
			{Name: "status", Kind: KindText, MaxLength: 32, Validate: validateToken},
			{Name: "extra_config", Kind: KindJSONObject, MaxLength: 8192, Validate: validateAccountExtraConfig},
		},
		CheckRow: func(ctx context.Context, tx *sql.Tx, _ int64, row map[string]any) error {
			return checkReference(ctx, tx, "site_id", "sites", row)
		},
		GuardDelete: func(ctx context.Context, tx *sql.Tx, id int64) error {
			return guardNoReferences(ctx, tx, id, "account", []reference{
				{Table: "route_channels", Column: "account_id", Resource: "channels"},
				{Table: "route_channels", Column: "token_id", Resource: "channels", Subquery: "SELECT id FROM account_tokens WHERE account_id = ?"},
			})
		},
		CascadeDelete: func(ctx context.Context, tx *sql.Tx, id int64) (map[string]int64, error) {
			affected, err := tx.ExecContext(ctx, `DELETE FROM account_tokens WHERE account_id = ?`, id)
			if err != nil {
				return nil, fmt.Errorf("delete account tokens: %w", err)
			}
			count, err := affected.RowsAffected()
			if err != nil {
				return nil, fmt.Errorf("count deleted account tokens: %w", err)
			}
			return map[string]int64{"tokens": count}, nil
		},
	},
	{
		Name: "tokens", Table: "account_tokens", OrderBy: "id",
		Columns: []Column{
			{Name: "account_id", Kind: KindInt, Required: true, Validate: validatePositive},
			{Name: "token", Kind: KindText, Required: true, Secret: true, MaxLength: 4096},
			{Name: "proxy_url", Kind: KindText, MaxLength: 2048, Validate: validateProxyURL},
			{Name: "use_system_proxy", Kind: KindBool},
			{Name: "enabled", Kind: KindBool, Default: true},
		},
		CheckRow: func(ctx context.Context, tx *sql.Tx, _ int64, row map[string]any) error {
			return checkReference(ctx, tx, "account_id", "accounts", row)
		},
		GuardDelete: func(ctx context.Context, tx *sql.Tx, id int64) error {
			return guardNoReferences(ctx, tx, id, "token", []reference{
				{Table: "route_channels", Column: "token_id", Resource: "channels"},
			})
		},
	},
	{
		Name: "routes", Table: "token_routes", OrderBy: "id",
		Columns: []Column{
			{Name: "model_pattern", Kind: KindText, Required: true, MaxLength: 512, Validate: validateToken},
			{Name: "display_name", Kind: KindText, MaxLength: 512},
			{Name: "route_mode", Kind: KindText, MaxLength: 32, Choices: []string{"pattern", "explicit_group"}},
			{Name: "routing_strategy", Kind: KindText, MaxLength: 32, Choices: []string{"weighted", "round_robin", "stable_first"}},
			{Name: "enabled", Kind: KindBool, Default: true},
			{Name: "model_mapping", Kind: KindJSONObject, MaxLength: 16384, Validate: validateModelMapping},
			{Name: "source_route_ids", Kind: KindJSONArray, Synthetic: true, MaxLength: 4096, Validate: validateIDList},
		},
		Apply: applyGroupSources,
		CheckRow: func(ctx context.Context, tx *sql.Tx, id int64, row map[string]any) error {
			return checkGroupRouteMode(row)
		},
		GuardDelete: func(ctx context.Context, tx *sql.Tx, id int64) error {
			// A group route draws its channels from the routes it lists, so
			// deleting a source route would leave the group pointing at nothing.
			// Whether that group should survive is the operator's call, so the
			// delete is refused and they unwire it deliberately.
			var groups int64
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM route_group_sources WHERE source_route_id = ? AND group_route_id <> ?`, id, id).Scan(&groups); err != nil {
				return fmt.Errorf("count group routes: %w", err)
			}
			if groups > 0 {
				// A group route is itself a route, so the resource that blocks
				// the delete is the same one being deleted.
				return ReferencedError{Subject: "route", Resource: "routes", Count: groups}
			}
			return nil
		},
		CascadeDelete: func(ctx context.Context, tx *sql.Tx, id int64) (map[string]int64, error) {
			channels, err := tx.ExecContext(ctx, `DELETE FROM route_channels WHERE route_id = ?`, id)
			if err != nil {
				return nil, fmt.Errorf("delete route channels: %w", err)
			}
			channelCount, err := channels.RowsAffected()
			if err != nil {
				return nil, fmt.Errorf("count deleted route channels: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM route_group_sources WHERE group_route_id = ? OR source_route_id = ?`, id, id); err != nil {
				return nil, fmt.Errorf("delete route group sources: %w", err)
			}
			return map[string]int64{"channels": channelCount}, nil
		},
	},
	{
		Name: "channels", Table: "route_channels", OrderBy: "route_id, priority DESC, id",
		Columns: []Column{
			{Name: "route_id", Kind: KindInt, Required: true, Validate: validatePositive},
			{Name: "account_id", Kind: KindInt, Required: true, Validate: validatePositive},
			{Name: "token_id", Kind: KindInt, Validate: validatePositive},
			{Name: "source_model", Kind: KindText, MaxLength: 512},
			{Name: "priority", Kind: KindInt},
			{Name: "weight", Kind: KindInt, Validate: validatePositive},
			{Name: "enabled", Kind: KindBool, Default: true},
		},
		CheckRow: func(ctx context.Context, tx *sql.Tx, id int64, row map[string]any) error {
			if err := checkReference(ctx, tx, "route_id", "token_routes", row); err != nil {
				return err
			}
			if err := checkReference(ctx, tx, "account_id", "accounts", row); err != nil {
				return err
			}
			return checkTokenBelongsToAccount(ctx, tx, row)
		},
	},
	{
		Name: "keys", Table: "downstream_api_keys", OrderBy: "id",
		Columns: []Column{
			{Name: "name", Kind: KindText, Required: true, MaxLength: 200},
			{Name: "key", Kind: KindText, Required: true, Secret: true, MaxLength: 512},
			{Name: "enabled", Kind: KindBool, Default: true},
			{Name: "expires_at", Kind: KindTime},
			{Name: "max_cost", Kind: KindReal, Validate: validatePositive},
			{Name: "used_cost", Kind: KindReal},
			{Name: "max_requests", Kind: KindInt, Validate: validatePositive},
			{Name: "used_requests", Kind: KindInt},
			{Name: "supported_models", Kind: KindJSONArray, MaxLength: 8192, Validate: validateStringList},
			{Name: "allowed_route_ids", Kind: KindJSONArray, MaxLength: 8192, Validate: validateIDList},
			{Name: "excluded_site_ids", Kind: KindJSONArray, MaxLength: 8192, Validate: validateIDList},
			{Name: "site_weight_multipliers", Kind: KindJSONObject, MaxLength: 8192, Validate: validateWeightMultipliers},
			{Name: "excluded_credential_refs", Kind: KindJSONArray, MaxLength: 16384, Validate: validateExcludedCredentials},
		},
		// A key value has to stay unique because it is what a client
		// authenticates with: two rows sharing one would make the matching
		// credential, and therefore the policy applied to it, ambiguous.
		CheckRow: func(ctx context.Context, tx *sql.Tx, id int64, row map[string]any) error {
			key, _ := row["key"].(string)
			if strings.TrimSpace(key) == "" {
				return invalidValue("key", ReasonRequired, nil, "a value is required")
			}
			var existing int64
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM downstream_api_keys WHERE key = ? AND id <> ?`, key, id).Scan(&existing); err != nil {
				return fmt.Errorf("check downstream API key: %w", err)
			}
			if existing > 0 {
				return fmt.Errorf("%w: another API key already has that value", ErrDuplicateValue)
			}
			return nil
		},
	},
	{
		Name: "proxies", Table: "proxy_profiles", OrderBy: "id",
		Columns: []Column{
			{Name: "name", Kind: KindText, Required: true, MaxLength: 200},
			{Name: "protocol", Kind: KindText, Required: true, MaxLength: 32, Choices: []string{"http", "https", "socks5", "socks5h"}},
			{Name: "url", Kind: KindText, Required: true, MaxLength: 2048, Validate: validateProxyURL},
			{Name: "is_default", Kind: KindBool},
			{Name: "enabled", Kind: KindBool, Default: true},
		},
	},
}

// Resources returns the editable configuration resources, in console order.
func Resources() []Resource {
	copied := make([]Resource, len(resources))
	copy(copied, resources)
	return copied
}

// FindResource resolves a resource by the name the management API addresses it by.
func FindResource(name string) (Resource, bool) {
	for _, resource := range resources {
		if resource.Name == name {
			return resource, true
		}
	}
	return Resource{}, false
}

// writableColumns lists the real columns of a resource, in order.
func writableColumns(resource Resource) []Column {
	columns := make([]Column, 0, len(resource.Columns))
	for _, column := range resource.Columns {
		if column.Synthetic {
			continue
		}
		columns = append(columns, column)
	}
	return columns
}

// reference names a table and column that must not point at a row being deleted.
type reference struct {
	Table  string
	Column string
	// Resource is the console resource name reported to the operator.
	Resource string
	// Subquery, when set, replaces the compared id. It is how a delete reaches
	// rows that point at a child of the row being deleted.
	Subquery string
}

// guardNoReferences refuses a delete while any reference still points at the row.
func guardNoReferences(ctx context.Context, tx *sql.Tx, id int64, subject string, references []reference) error {
	for _, ref := range references {
		query := fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE %s = ?`, ref.Table, ref.Column)
		if ref.Subquery != "" {
			query = fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE %s IN (%s)`, ref.Table, ref.Column, ref.Subquery)
		}
		var count int64
		if err := tx.QueryRowContext(ctx, query, id).Scan(&count); err != nil {
			return fmt.Errorf("count referencing rows in %s: %w", ref.Table, err)
		}
		if count > 0 {
			return ReferencedError{Subject: subject, Resource: ref.Resource, Count: count}
		}
	}
	return nil
}

// ListResource returns every row of a resource in console order.
func (s *SQLiteStore) ListResource(ctx context.Context, name string) ([]map[string]any, error) {
	resource, ok := FindResource(name)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrUnknownResource, name)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+columnList(resource)+` FROM `+resource.Table+` ORDER BY `+orderBy(resource))
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", resource.Name, err)
	}
	defer rows.Close()

	listed := make([]map[string]any, 0)
	for rows.Next() {
		row, err := scanResourceRow(resource, rows)
		if err != nil {
			return nil, err
		}
		listed = append(listed, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list %s: %w", resource.Name, err)
	}
	return listed, nil
}

// CreateResource inserts one row and returns it as stored.
func (s *SQLiteStore) CreateResource(ctx context.Context, name string, values map[string]any) (map[string]any, error) {
	resource, ok := FindResource(name)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrUnknownResource, name)
	}
	provided, err := resource.providedFields(values)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin %s insert: %w", resource.Name, err)
	}
	defer tx.Rollback()

	if err := resource.checkRequired(provided); err != nil {
		return nil, err
	}
	if err := resource.checkRow(ctx, tx, 0, provided); err != nil {
		return nil, err
	}

	columns := make([]string, 0, len(provided))
	placeholders := make([]string, 0, len(provided))
	args := make([]any, 0, len(provided))
	for _, column := range writableColumns(resource) {
		value, present := provided[column.Name]
		if !present {
			// An omitted column takes the schema default, which keeps a create
			// from having to name every optional field.
			continue
		}
		columns = append(columns, column.Name)
		placeholders = append(placeholders, "?")
		args = append(args, storedValue(column, value))
	}
	if len(columns) == 0 {
		return nil, ValidationError{Message: "the request does not set any field"}
	}

	query := fmt.Sprintf(`INSERT INTO %s (%s) VALUES (%s)`, resource.Table, strings.Join(columns, ", "), strings.Join(placeholders, ", "))
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return nil, wrapResourceWriteError(resource, err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("read %s id: %w", resource.Name, err)
	}
	if err := resource.applySynthetic(ctx, tx, id, provided); err != nil {
		return nil, err
	}
	stored, err := loadResourceRow(ctx, tx, resource, id)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit %s insert: %w", resource.Name, err)
	}
	return stored, nil
}

// UpdateResource applies the fields a request carries to one row. A field the
// request omits keeps its stored value, so a partial update from automation
// never clears what it did not mention. A secret field sent empty is treated the
// same way, because an untouched input in the console submits an empty string
// and the mask the console displayed must never be stored back over it.
func (s *SQLiteStore) UpdateResource(ctx context.Context, name string, id int64, values map[string]any) (map[string]any, error) {
	resource, ok := FindResource(name)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrUnknownResource, name)
	}
	if id <= 0 {
		return nil, ValidationError{Message: "a positive row id is required"}
	}
	provided, err := resource.providedFields(values)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin %s update: %w", resource.Name, err)
	}
	defer tx.Rollback()

	existing, err := loadResourceRow(ctx, tx, resource, id)
	if err != nil {
		return nil, err
	}
	row := mergeRow(existing, provided)
	if err := resource.checkRequired(row); err != nil {
		return nil, err
	}
	if err := resource.checkRow(ctx, tx, id, row); err != nil {
		return nil, err
	}

	assignments := make([]string, 0, len(provided))
	args := make([]any, 0, len(provided)+1)
	for _, column := range writableColumns(resource) {
		value, present := provided[column.Name]
		if !present {
			continue
		}
		assignments = append(assignments, column.Name+" = ?")
		args = append(args, storedValue(column, value))
	}
	if len(assignments) > 0 {
		args = append(args, id)
		query := fmt.Sprintf(`UPDATE %s SET %s WHERE id = ?`, resource.Table, strings.Join(assignments, ", "))
		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			return nil, wrapResourceWriteError(resource, err)
		}
	}
	if err := resource.applySynthetic(ctx, tx, id, provided); err != nil {
		return nil, err
	}
	stored, err := loadResourceRow(ctx, tx, resource, id)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit %s update: %w", resource.Name, err)
	}
	return stored, nil
}

// DeleteResource removes one row. It refuses when other configuration still
// points at it, and reports the rows a cascade removed alongside it.
func (s *SQLiteStore) DeleteResource(ctx context.Context, name string, id int64) (map[string]int64, error) {
	resource, ok := FindResource(name)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrUnknownResource, name)
	}
	if id <= 0 {
		return nil, ValidationError{Message: "a positive row id is required"}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin %s delete: %w", resource.Name, err)
	}
	defer tx.Rollback()

	if _, err := loadResourceRow(ctx, tx, resource, id); err != nil {
		return nil, err
	}
	if resource.GuardDelete != nil {
		if err := resource.GuardDelete(ctx, tx, id); err != nil {
			return nil, err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM `+resource.Table+` WHERE id = ?`, id); err != nil {
		return nil, fmt.Errorf("delete %s: %w", resource.Name, err)
	}
	cascaded := map[string]int64{}
	if resource.CascadeDelete != nil {
		cascaded, err = resource.CascadeDelete(ctx, tx, id)
		if err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit %s delete: %w", resource.Name, err)
	}
	return cascaded, nil
}

func orderBy(resource Resource) string {
	if strings.TrimSpace(resource.OrderBy) == "" {
		return "id"
	}
	return resource.OrderBy
}

// columnList renders the select list of a resource.
func columnList(resource Resource) string {
	names := make([]string, 0, len(resource.Columns))
	for _, column := range writableColumns(resource) {
		names = append(names, column.Name)
	}
	return "id, " + strings.Join(names, ", ")
}

func (r Resource) column(name string) (Column, bool) {
	for _, column := range r.Columns {
		if column.Name == name {
			return column, true
		}
	}
	return Column{}, false
}

// providedFields normalizes the fields a request actually carries. It refuses an
// unknown field name so a typo is reported instead of silently ignored, and drops
// an empty secret so an untouched console input cannot overwrite a credential.
func (r Resource) providedFields(values map[string]any) (map[string]any, error) {
	provided := make(map[string]any, len(values))
	for name, raw := range values {
		column, ok := r.column(name)
		if !ok {
			return nil, invalidValue(name, ReasonUnknownField, map[string]any{"resource": r.Name}, "unknown field for %s", r.Name)
		}
		if column.Secret {
			if raw == nil && !column.Clearable {
				return nil, invalidValue(name, ReasonRequired, nil, "a required secret cannot be cleared")
			}
			if text, isText := raw.(string); isText && strings.TrimSpace(text) == "" {
				continue
			}
		}
		value, err := normalizeValue(column, raw)
		if err != nil {
			return nil, err
		}
		provided[name] = value
	}
	return provided, nil
}

// mergeRow overlays the provided fields on an existing stored row. A nil value
// is an SQL NULL.
func mergeRow(existing, provided map[string]any) map[string]any {
	row := make(map[string]any, len(existing)+len(provided))
	for name, value := range existing {
		row[name] = value
	}
	for name, value := range provided {
		row[name] = value
	}
	return row
}

// checkRequired rejects a row that leaves a required column empty. It runs on
// the merged row, so it covers a create that omits the column, an update that
// clears it, and a stored row that already held nothing where a value belongs.
func (r Resource) checkRequired(row map[string]any) error {
	for _, column := range r.Columns {
		if !column.Required || column.Synthetic {
			continue
		}
		value, present := row[column.Name]
		if !present || value == nil {
			return invalidValue(column.Name, ReasonRequired, nil, "a value is required")
		}
		if text, isText := value.(string); isText && strings.TrimSpace(text) == "" {
			return invalidValue(column.Name, ReasonRequired, nil, "a value is required")
		}
	}
	return nil
}

func (r Resource) checkRow(ctx context.Context, tx *sql.Tx, id int64, row map[string]any) error {
	if r.CheckRow == nil {
		return nil
	}
	return r.CheckRow(ctx, tx, id, row)
}

// applySynthetic stores the fields that are not columns of the table.
func (r Resource) applySynthetic(ctx context.Context, tx *sql.Tx, id int64, values map[string]any) error {
	if r.Apply == nil {
		return nil
	}
	for _, column := range r.Columns {
		if !column.Synthetic {
			continue
		}
		if _, present := values[column.Name]; present {
			return r.Apply(ctx, tx, id, values)
		}
	}
	return nil
}

// loadResourceRow reads one row inside a transaction, reporting a missing row as
// ErrResourceNotFound rather than as an empty result.
func loadResourceRow(ctx context.Context, tx *sql.Tx, resource Resource, id int64) (map[string]any, error) {
	row := tx.QueryRowContext(ctx, `SELECT `+columnList(resource)+` FROM `+resource.Table+` WHERE id = ?`, id)
	stored, err := scanResourceRow(resource, row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s %d", ErrResourceNotFound, resource.Name, id)
	}
	if err != nil {
		return nil, err
	}
	return stored, nil
}

// scanResourceRow converts a stored row into the JSON-friendly form the console
// reads. Secrets are still in place here: masking belongs to the management API,
// so a value this layer validates is always the real one.
func scanResourceRow(resource Resource, scanner interface{ Scan(...any) error }) (map[string]any, error) {
	columns := writableColumns(resource)
	destinations := make([]any, 0, len(columns)+1)
	id := new(sql.NullInt64)
	destinations = append(destinations, id)
	for _, column := range columns {
		switch column.Kind {
		case KindInt:
			destinations = append(destinations, new(sql.NullInt64))
		case KindReal:
			destinations = append(destinations, new(sql.NullFloat64))
		case KindBool:
			destinations = append(destinations, new(sql.NullInt64))
		default:
			destinations = append(destinations, new(sql.NullString))
		}
	}
	if err := scanner.Scan(destinations...); err != nil {
		return nil, err
	}

	row := make(map[string]any, len(columns)+1)
	if id.Valid {
		row["id"] = id.Int64
	}
	for index, column := range columns {
		row[column.Name] = decodedColumnValue(column, destinations[index+1])
	}
	return row, nil
}

func decodedColumnValue(column Column, destination any) any {
	switch column.Kind {
	case KindInt:
		value := destination.(*sql.NullInt64)
		if value.Valid {
			return value.Int64
		}
	case KindReal:
		value := destination.(*sql.NullFloat64)
		if value.Valid {
			return value.Float64
		}
	case KindBool:
		value := destination.(*sql.NullInt64)
		return value.Valid && value.Int64 != 0
	default:
		value := destination.(*sql.NullString)
		if value.Valid {
			return value.String
		}
	}
	return nil
}

// storedValue converts a normalized value to the form the driver binds.
func storedValue(column Column, value any) any {
	if flag, ok := value.(bool); ok {
		if flag {
			return int64(1)
		}
		return int64(0)
	}
	return value
}

// normalizeValue converts one JSON value to the column's stored form.
func normalizeValue(column Column, raw any) (any, error) {
	switch column.Kind {
	case KindText:
		text, ok := raw.(string)
		if !ok {
			if raw == nil {
				return nil, nil
			}
			return nil, invalidField(column.Name, "a text value is required")
		}
		// Trimmed because a pasted credential routinely carries surrounding
		// whitespace, and a value that kept it would be sent upstream verbatim.
		text = strings.TrimSpace(text)
		if text == "" {
			return nil, nil
		}
		limit := column.MaxLength
		if limit <= 0 {
			limit = defaultMaxTextLength
		}
		if len(text) > limit {
			return nil, invalidValue(column.Name, ReasonTooLong, map[string]any{"limit": limit}, "use at most %d bytes", limit)
		}
		canonical, err := canonicalChoice(column, text)
		if err != nil {
			return nil, err
		}
		text = canonical
		if column.Validate != nil {
			if err := column.Validate(text); err != nil {
				return nil, columnValidationError(column.Name, err)
			}
		}
		return text, nil
	case KindInt:
		number, err := toInt64(raw)
		if err != nil {
			if isEmptyJSONValue(raw) {
				return nil, nil
			}
			return nil, invalidField(column.Name, "a whole number is required")
		}
		if column.Validate != nil {
			if err := column.Validate(number); err != nil {
				return nil, columnValidationError(column.Name, err)
			}
		}
		return number, nil
	case KindReal:
		number, err := toFloat64(raw)
		if err != nil {
			if isEmptyJSONValue(raw) {
				return nil, nil
			}
			return nil, invalidField(column.Name, "a number is required")
		}
		if column.Validate != nil {
			if err := column.Validate(number); err != nil {
				return nil, columnValidationError(column.Name, err)
			}
		}
		return number, nil
	case KindBool:
		flag, err := toBool(raw)
		if err != nil {
			return nil, invalidField(column.Name, "a boolean is required")
		}
		return flag, nil
	case KindTime:
		text, _ := raw.(string)
		if raw == nil || strings.TrimSpace(text) == "" {
			return nil, nil
		}
		parsed, err := parseTimestamp(strings.TrimSpace(text))
		if err != nil {
			return nil, invalidField(column.Name, "use an RFC 3339 timestamp such as 2026-01-31T09:00:00Z")
		}
		return formatTime(parsed), nil
	case KindJSONArray, KindJSONObject, KindJSON:
		return normalizeJSON(column, raw)
	default:
		return nil, invalidField(column.Name, "unsupported field type")
	}
}

// normalizeJSON validates a JSON value and returns its compacted text. The text
// is compacted rather than re-encoded, so the key order of a model_mapping
// survives the round trip exactly as the operator wrote it.
func normalizeJSON(column Column, raw any) (any, error) {
	var text string
	switch value := raw.(type) {
	case nil:
		return nil, nil
	case string:
		text = strings.TrimSpace(value)
		if text == "" || text == "null" {
			return nil, nil
		}
	default:
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, invalidField(column.Name, "must be valid JSON")
		}
		text = string(encoded)
	}

	limit := column.MaxLength
	if limit <= 0 {
		limit = defaultMaxTextLength * 8
	}
	if len(text) > limit {
		return nil, invalidValue(column.Name, ReasonTooLong, map[string]any{"limit": limit}, "use at most %d bytes of JSON", limit)
	}

	var decoded any
	if err := json.Unmarshal([]byte(text), &decoded); err != nil {
		return nil, invalidField(column.Name, "must be valid JSON")
	}
	switch column.Kind {
	case KindJSONArray:
		if _, ok := decoded.([]any); !ok {
			return nil, invalidField(column.Name, "must be a JSON array")
		}
	case KindJSONObject:
		if _, ok := decoded.(map[string]any); !ok {
			return nil, invalidField(column.Name, "must be a JSON object")
		}
	}
	if column.Validate != nil {
		if err := column.Validate(decoded); err != nil {
			return nil, columnValidationError(column.Name, err)
		}
	}

	compacted := &bytes.Buffer{}
	if err := json.Compact(compacted, []byte(text)); err != nil {
		return nil, invalidField(column.Name, "must be valid JSON")
	}
	return compacted.String(), nil
}

// columnValidationError turns a column validator's error into a field error,
// keeping a stable reason for the faults the console renders in its own language.
func columnValidationError(field string, err error) error {
	if errors.Is(err, errMustBePositive) {
		return invalidValue(field, ReasonPositive, nil, "%s", err.Error())
	}
	return invalidField(field, "%s", err.Error())
}

// canonicalChoice validates a value against the column's fixed set and returns
// the declared spelling. The gateway compares these values exactly — a routing
// strategy decides the breaker mode, a route mode decides whether a route draws
// channels at all — so an accepted variant has to be stored as the one the code
// reads.
func canonicalChoice(column Column, value string) (string, error) {
	if len(column.Choices) == 0 {
		return value, nil
	}
	for _, allowed := range column.Choices {
		if strings.EqualFold(allowed, value) {
			return allowed, nil
		}
	}
	return "", invalidValue(column.Name, ReasonNotAllowed, map[string]any{"choices": column.Choices}, "must be one of %s", strings.Join(column.Choices, ", "))
}

// checkReference verifies that an integer column points at a row that exists.
func checkReference(ctx context.Context, tx *sql.Tx, column, table string, row map[string]any) error {
	value, present := row[column]
	if !present || value == nil {
		return nil
	}
	id, ok := value.(int64)
	if !ok {
		return invalidField(column, "a row id is required")
	}
	var found int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table+` WHERE id = ?`, id).Scan(&found); err != nil {
		return fmt.Errorf("check %s: %w", column, err)
	}
	if found == 0 {
		return invalidValue(column, ReasonMissingReference, map[string]any{"table": table, "id": id}, "no row with id %d exists in %s", id, table)
	}
	return nil
}

// checkTokenBelongsToAccount keeps the credential pairing the loader relies on:
// a channel resolves its token through the account it names, so a token that
// belongs to a different account would silently dispatch as the wrong identity.
func checkTokenBelongsToAccount(ctx context.Context, tx *sql.Tx, row map[string]any) error {
	tokenValue, present := row["token_id"]
	if !present || tokenValue == nil {
		return nil
	}
	tokenID, ok := tokenValue.(int64)
	if !ok {
		return invalidField("token_id", "a row id is required")
	}
	accountID, ok := row["account_id"].(int64)
	if !ok {
		return nil // reported by the account_id check instead
	}
	var owner int64
	err := tx.QueryRowContext(ctx, `SELECT account_id FROM account_tokens WHERE id = ?`, tokenID).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return invalidValue("token_id", ReasonMissingReference, map[string]any{"table": "account_tokens", "id": tokenID}, "no row with id %d exists in account_tokens", tokenID)
	}
	if err != nil {
		return fmt.Errorf("check token ownership: %w", err)
	}
	if owner != accountID {
		return invalidValue("token_id", ReasonReferenceMismatch, map[string]any{"owner": owner, "account": accountID}, "the token belongs to account %d, not %d", owner, accountID)
	}
	return nil
}

// checkGroupRouteMode refuses source routes on a route that is not an explicit
// group, because the selector only ever draws a group's channels from that list.
// Storing one on a pattern route would leave the operator with a field that
// looks configured and does nothing.
func checkGroupRouteMode(row map[string]any) error {
	sources, present := row["source_route_ids"]
	if !present || sources == nil {
		return nil
	}
	mode, _ := row["route_mode"].(string)
	if strings.EqualFold(strings.TrimSpace(mode), "explicit_group") {
		return nil
	}
	return invalidValue("source_route_ids", ReasonNotAllowed, map[string]any{"choices": []string{"explicit_group"}}, "only an explicit_group route can list source routes")
}

// applyGroupSources replaces the source routes of a group route. The field is
// optional: a request that does not carry it leaves the existing list alone.
func applyGroupSources(ctx context.Context, tx *sql.Tx, id int64, values map[string]any) error {
	raw, present := values["source_route_ids"]
	if !present {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM route_group_sources WHERE group_route_id = ?`, id); err != nil {
		return fmt.Errorf("clear route group sources: %w", err)
	}
	if raw == nil {
		return nil
	}
	ids, err := decodeIDList(raw)
	if err != nil {
		return invalidField("source_route_ids", "%s", err.Error())
	}
	seen := make(map[int64]struct{}, len(ids))
	for _, sourceID := range ids {
		if sourceID == id {
			return invalidField("source_route_ids", "a group route cannot list itself")
		}
		if _, duplicate := seen[sourceID]; duplicate {
			continue
		}
		seen[sourceID] = struct{}{}
		var found int64
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM token_routes WHERE id = ?`, sourceID).Scan(&found); err != nil {
			return fmt.Errorf("check source route %d: %w", sourceID, err)
		}
		if found == 0 {
			return invalidValue("source_route_ids", ReasonMissingReference, map[string]any{"table": "token_routes", "id": sourceID}, "no row with id %d exists in token_routes", sourceID)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO route_group_sources (group_route_id, source_route_id) VALUES (?, ?)`, id, sourceID); err != nil {
			return fmt.Errorf("insert route group source: %w", err)
		}
	}
	return nil
}

// wrapResourceWriteError turns a driver constraint failure into an error the
// management API can answer with a useful status.
func wrapResourceWriteError(resource Resource, err error) error {
	if isUniqueViolation(err) {
		return fmt.Errorf("%w: the %s already holds a row with that value", ErrDuplicateValue, resource.Name)
	}
	return fmt.Errorf("write %s: %w", resource.Name, err)
}

func isEmptyJSONValue(raw any) bool {
	if raw == nil {
		return true
	}
	text, ok := raw.(string)
	return ok && strings.TrimSpace(text) == ""
}

func toInt64(raw any) (int64, error) {
	switch value := raw.(type) {
	case int64:
		return value, nil
	case int:
		return int64(value), nil
	case float64:
		if value != float64(int64(value)) {
			return 0, errors.New("not a whole number")
		}
		return int64(value), nil
	case json.Number:
		return value.Int64()
	case string:
		return strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	default:
		return 0, errors.New("not a number")
	}
}

func toFloat64(raw any) (float64, error) {
	switch value := raw.(type) {
	case float64:
		return value, nil
	case int64:
		return float64(value), nil
	case int:
		return float64(value), nil
	case json.Number:
		return value.Float64()
	case string:
		return strconv.ParseFloat(strings.TrimSpace(value), 64)
	default:
		return 0, errors.New("not a number")
	}
}

func toBool(raw any) (bool, error) {
	switch value := raw.(type) {
	case bool:
		return value, nil
	case string:
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			return false, nil
		}
		return strconv.ParseBool(trimmed)
	case float64:
		return value != 0, nil
	case int64:
		return value != 0, nil
	case nil:
		return false, nil
	default:
		return false, errors.New("not a boolean")
	}
}

// parseTimestamp accepts the layouts the console sends together with the ones an
// existing configuration file may already carry.
func parseTimestamp(value string) (parsed time.Time, err error) {
	if date, dateErr := time.Parse("2006-01-02", value); dateErr == nil {
		return date, nil
	}
	return parseTime(value)
}

/* ===== Field validation ===== */

// validatePositive rejects a zero or negative number where only a positive one
// means anything, such as a weight or a row id.
func validatePositive(value any) error {
	switch number := value.(type) {
	case int64:
		if number <= 0 {
			return errMustBePositive
		}
	case float64:
		if !(number > 0) {
			return errMustBePositive
		}
	}
	return nil
}

// errMustBePositive is returned by validatePositive and recognized by the
// normalization step so the rejection carries a reason code.
var errMustBePositive = errors.New("must be greater than zero")

// validateToken bounds a short identifier such as a platform or a status value.
// It deliberately accepts any token rather than a fixed list, because the
// gateway only ever compares these against one known value: rejecting an
// unrecognized one would make an unrelated edit of that row impossible.
func validateToken(value any) error {
	text, ok := value.(string)
	if !ok {
		return errors.New("must be text")
	}
	if strings.ContainsAny(text, " \t\r\n") {
		return errors.New("must not contain whitespace")
	}
	return nil
}

func validateHTTPURL(value any) error {
	text, ok := value.(string)
	if !ok {
		return errors.New("must be an absolute http or https URL")
	}
	parsed, err := url.Parse(text)
	if err != nil {
		return errors.New("must be a valid URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return errors.New("must start with http:// or https://")
	}
	if parsed.Host == "" {
		return errors.New("must include a host")
	}
	return nil
}

// validateProxyURL accepts the proxy forms the transport can actually use, plus
// the two keywords that select system proxying or an explicit direct connection.
func validateProxyURL(value any) error {
	text, ok := value.(string)
	if !ok {
		return errors.New("must be a proxy URL")
	}
	switch strings.ToLower(text) {
	case "system", "direct", "none":
		return nil
	}
	parsed, err := url.Parse(text)
	if err != nil {
		return errors.New("must be a valid proxy URL")
	}
	switch parsed.Scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return errors.New("must use http, https, socks5, or socks5h")
	}
	if parsed.Host == "" {
		return errors.New("must include a host")
	}
	return nil
}

// validateStringMap accepts a JSON object whose values are strings, the shape of
// custom_headers.
func validateStringMap(value any) error {
	object, ok := value.(map[string]any)
	if !ok {
		return errors.New("must be a JSON object")
	}
	for name, entry := range object {
		if strings.TrimSpace(name) == "" {
			return errors.New("header names must not be empty")
		}
		if _, ok := entry.(string); !ok {
			return fmt.Errorf("the value of %q must be a string", name)
		}
	}
	return nil
}

// validateModelMapping accepts a JSON object of pattern to upstream model.
func validateModelMapping(value any) error {
	object, ok := value.(map[string]any)
	if !ok {
		return errors.New("must be a JSON object")
	}
	for pattern, target := range object {
		if strings.TrimSpace(pattern) == "" {
			return errors.New("patterns must not be empty")
		}
		text, ok := target.(string)
		if !ok || strings.TrimSpace(text) == "" {
			return fmt.Errorf("the target of %q must be a non-empty string", pattern)
		}
	}
	return nil
}

// validateStringList accepts a JSON array of model patterns.
func validateStringList(value any) error {
	list, ok := value.([]any)
	if !ok {
		return errors.New("must be a JSON array")
	}
	for _, entry := range list {
		text, ok := entry.(string)
		if !ok || strings.TrimSpace(text) == "" {
			return errors.New("every entry must be a non-empty string")
		}
	}
	return nil
}

// validateIDList accepts a JSON array of row ids.
func validateIDList(value any) error {
	_, err := decodeIDList(value)
	return err
}

// decodeIDList accepts a JSON array of row ids either as its compacted text,
// which is how a normalized field arrives, or as the decoded list.
func decodeIDList(value any) ([]int64, error) {
	raw := value
	if text, ok := value.(string); ok {
		var decoded []any
		if err := json.Unmarshal([]byte(text), &decoded); err != nil {
			return nil, errors.New("must be a JSON array of row ids")
		}
		raw = decoded
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, errors.New("must be a JSON array of row ids")
	}
	ids := make([]int64, 0, len(list))
	for _, entry := range list {
		id, err := toInt64(entry)
		if err != nil || id <= 0 {
			return nil, errors.New("every entry must be a positive row id")
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// validateWeightMultipliers accepts the site-weight multipliers of a key, the
// shape of {"12": 1.5}. The loader drops a non-positive multiplier, so it is
// rejected here rather than stored as a value that does nothing.
func validateWeightMultipliers(value any) error {
	object, ok := value.(map[string]any)
	if !ok {
		return errors.New("must be a JSON object")
	}
	for rawID, rawMultiplier := range object {
		id, err := strconv.ParseInt(strings.TrimSpace(rawID), 10, 64)
		if err != nil || id <= 0 {
			return fmt.Errorf("the key %q must be a positive site id", rawID)
		}
		multiplier, err := toFloat64(rawMultiplier)
		if err != nil || !(multiplier > 0) {
			return fmt.Errorf("the multiplier for site %s must be greater than zero", rawID)
		}
	}
	return nil
}

// validateExcludedCredentials accepts the credential exclusions of a key, which
// the loader reads as account_token references carrying three identifiers.
func validateExcludedCredentials(value any) error {
	list, ok := value.([]any)
	if !ok {
		return errors.New("must be a JSON array")
	}
	for _, entry := range list {
		object, ok := entry.(map[string]any)
		if !ok {
			return errors.New("every entry must be a JSON object")
		}
		kind, _ := object["kind"].(string)
		if !strings.EqualFold(strings.TrimSpace(kind), "account_token") {
			return errors.New(`every entry needs a "kind" of "account_token"`)
		}
		for _, field := range []string{"siteId", "accountId", "tokenId"} {
			id, err := toInt64(object[field])
			if err != nil || id <= 0 {
				return fmt.Errorf("%q must be a positive row id", field)
			}
		}
	}
	return nil
}

// validateAccountExtraConfig accepts the account proxy settings the loader reads
// out of extra_config, and preserves any other keys the upstream file carries.
func validateAccountExtraConfig(value any) error {
	object, ok := value.(map[string]any)
	if !ok {
		return errors.New("must be a JSON object")
	}
	if proxyValue, present := object["proxyUrl"]; present {
		text, ok := proxyValue.(string)
		if !ok {
			return errors.New(`"proxyUrl" must be a string`)
		}
		if strings.TrimSpace(text) != "" {
			if err := validateProxyURL(strings.TrimSpace(text)); err != nil {
				return fmt.Errorf("proxyUrl %s", err.Error())
			}
		}
	}
	if systemValue, present := object["useSystemProxy"]; present {
		if _, err := toBool(systemValue); err != nil {
			return errors.New(`"useSystemProxy" must be a boolean`)
		}
	}
	return nil
}
