package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/yhw5231/fluxgate/internal/breaker"
)

// Settings are the gateway's own rows in the shared settings table: runtime
// policy overrides and the bookkeeping the console needs. They are written
// through one method so a change is a single transaction, and so a value the
// caller asks to clear is deleted rather than stored as an empty string.

// PutSettings stores a set of settings keys. A key mapped to the empty string is
// deleted, which is how a value returns to its process default.
func (s *SQLiteStore) PutSettings(ctx context.Context, values map[string]string) error {
	if len(values) == 0 {
		return nil
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		trimmed := strings.TrimSpace(key)
		if trimmed == "" {
			return ValidationError{Field: "key", Message: "a settings key must not be empty"}
		}
		if len(trimmed) > 200 {
			return invalidValue("key", ReasonTooLong, map[string]any{"limit": 200}, "use at most %d bytes per key", 200)
		}
		keys = append(keys, trimmed)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin settings write: %w", err)
	}
	defer tx.Rollback()

	for _, key := range keys {
		value := values[key]
		if strings.TrimSpace(value) == "" {
			if _, err := tx.ExecContext(ctx, `DELETE FROM settings WHERE key = ?`, key); err != nil {
				return fmt.Errorf("clear setting %s: %w", key, err)
			}
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES (?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value); err != nil {
			return fmt.Errorf("store setting %s: %w", key, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit settings write: %w", err)
	}
	return nil
}

// LoadSettings reads the settings table on its own, for a caller that needs the
// policy without the rest of the configuration.
func (s *SQLiteStore) LoadSettings(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, COALESCE(value, '') FROM settings`)
	if err != nil {
		return nil, fmt.Errorf("load settings: %w", err)
	}
	defer rows.Close()
	settings := make(map[string]string)
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return nil, fmt.Errorf("scan setting: %w", err)
		}
		settings[key] = value
	}
	return settings, rows.Err()
}

/* ===== Breaker recovery ===== */

// DeleteBreakerStates clears the recorded circuits a caller names, which is how
// an operator brings a cooled-down or disabled channel back into service before
// its cooldown would have expired. An empty scope list clears every circuit.
func (s *SQLiteStore) DeleteBreakerStates(ctx context.Context, scopes []breaker.Scope) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin breaker reset: %w", err)
	}
	defer tx.Rollback()

	removed := int64(0)
	if len(scopes) == 0 {
		result, err := tx.ExecContext(ctx, `DELETE FROM gateway_breaker_states`)
		if err != nil {
			return 0, fmt.Errorf("reset breaker states: %w", err)
		}
		removed, err = result.RowsAffected()
		if err != nil {
			return 0, fmt.Errorf("count reset breaker states: %w", err)
		}
	} else {
		for _, scope := range scopes {
			result, err := tx.ExecContext(ctx, `DELETE FROM gateway_breaker_states WHERE scope_key = ?`, scopeKey(scope))
			if err != nil {
				return 0, fmt.Errorf("reset breaker state: %w", err)
			}
			count, err := result.RowsAffected()
			if err != nil {
				return 0, fmt.Errorf("count reset breaker states: %w", err)
			}
			removed += count
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit breaker reset: %w", err)
	}
	return removed, nil
}

// ResetBreakerStates clears one circuit, or all of them when the scope is
// empty, in both the persisted state and the in-memory snapshot the adapter
// answers selection from.
func (a *BreakerAdapter) ResetBreakerStates(ctx context.Context, scopes []breaker.Scope) (int64, error) {
	removed, err := a.store.DeleteBreakerStates(ctx, scopes)
	if err != nil {
		return 0, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(scopes) == 0 {
		a.states = make(map[breaker.Scope]breaker.State)
		return removed, nil
	}
	for _, scope := range scopes {
		delete(a.states, scope)
	}
	return removed, nil
}
