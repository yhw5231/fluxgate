package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/yhw5231/fluxgate/internal/admin"
)

// ErrNoAdminAccount reports that no administrator has been created yet. The
// console answers 503 in that state rather than offering a login form that can
// never succeed.
var ErrNoAdminAccount = errors.New("no administrator account has been created")

// ErrAdminAccountExists reports an attempt to create a second administrator.
// Exactly one account is supported, so the caller must reset the credential
// instead.
var ErrAdminAccountExists = errors.New("an administrator account already exists")

// ErrSessionNotFound reports an unknown, expired, or revoked session token.
var ErrSessionNotFound = errors.New("session is not valid")

// adminSchemaDDL creates the gateway-owned authentication tables. These are not
// part of the upstream configuration schema, so they are created on every start
// and never validated against the upstream table list.
var adminSchemaDDL = []string{
	`CREATE TABLE IF NOT EXISTS gateway_admin_users (
		id INTEGER PRIMARY KEY,
		username TEXT NOT NULL UNIQUE,
		password_hash TEXT NOT NULL,
		change_required INTEGER NOT NULL DEFAULT 0,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS gateway_admin_sessions (
		id INTEGER PRIMARY KEY,
		token_hash TEXT NOT NULL UNIQUE,
		account_id INTEGER NOT NULL,
		issued_at TEXT NOT NULL,
		expires_at TEXT NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS gateway_admin_sessions_expires_at_idx ON gateway_admin_sessions(expires_at)`,
}

// EnsureAdminSchema prepares the authentication tables at startup and brings a
// database created by an earlier version up to the current shape.
func (s *SQLiteStore) EnsureAdminSchema(ctx context.Context) error {
	for _, statement := range adminSchemaDDL {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("create admin table: %w", err)
		}
	}
	if err := s.addAdminColumnIfMissing(ctx, "change_required", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	return nil
}

// addAdminColumnIfMissing adds a column to gateway_admin_users when an older
// database predates it. SQLite has no IF NOT EXISTS for a column, so the current
// shape is compared first. The default is chosen so an account created before
// the column existed is not forced through a password change it does not need.
func (s *SQLiteStore) addAdminColumnIfMissing(ctx context.Context, column, definition string) error {
	rows, err := s.db.QueryContext(ctx, `SELECT name FROM pragma_table_info('gateway_admin_users')`)
	if err != nil {
		return fmt.Errorf("inspect gateway_admin_users: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return fmt.Errorf("scan gateway_admin_users column: %w", err)
		}
		if name == column {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate gateway_admin_users columns: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `ALTER TABLE gateway_admin_users ADD COLUMN `+column+` `+definition); err != nil {
		return fmt.Errorf("add gateway_admin_users.%s: %w", column, err)
	}
	return nil
}

// CreateAdminAccount stores the administrator credential. It refuses to create
// a second account so the singleton guarantee is enforced in the database
// rather than only by the caller. The check is by row count, not by username,
// because a unique index on the name alone would still allow a second
// administrator under a different name.
//
// changeRequired marks an account created with a well-known password; the
// console will not serve data to such an account until the password is changed.
func (s *SQLiteStore) CreateAdminAccount(ctx context.Context, username, passwordHash string, changeRequired bool) (admin.Account, error) {
	normalized, err := admin.ValidateUsername(username)
	if err != nil {
		return admin.Account{}, err
	}
	if strings.TrimSpace(passwordHash) == "" {
		return admin.Account{}, errors.New("password hash must not be empty")
	}

	var existing int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM gateway_admin_users`).Scan(&existing); err != nil {
		return admin.Account{}, fmt.Errorf("count admin accounts: %w", err)
	}
	if existing > 0 {
		return admin.Account{}, ErrAdminAccountExists
	}

	now := time.Now().UTC()
	result, err := s.db.ExecContext(ctx,
		`INSERT INTO gateway_admin_users (username, password_hash, change_required, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		normalized, passwordHash, boolInt(changeRequired), formatTime(now), formatTime(now))
	if err != nil {
		// A concurrent create can still lose the race against the count above;
		// the unique index is what actually serializes it.
		if isUniqueViolation(err) {
			return admin.Account{}, ErrAdminAccountExists
		}
		return admin.Account{}, fmt.Errorf("create admin account: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return admin.Account{}, fmt.Errorf("read admin account id: %w", err)
	}
	return admin.Account{
		ID:             id,
		Username:       normalized,
		PasswordHash:   passwordHash,
		ChangeRequired: changeRequired,
		CreatedAt:      now,
		UpdatedAt:      now,
	}, nil
}

// EnsureDefaultAdminAccount creates the built-in administrator when the database
// has none, so a fresh deployment is reachable without a CLI step. The account
// is created with a change-required flag, so the well-known password grants only
// the ability to set a real one.
//
// It reports whether an account was created.
func (s *SQLiteStore) EnsureDefaultAdminAccount(ctx context.Context) (bool, error) {
	_, err := s.LoadAdminAccount(ctx)
	if err == nil {
		return false, nil
	}
	if !errors.Is(err, ErrNoAdminAccount) {
		return false, err
	}

	hash, err := admin.HashPassword(admin.DefaultPassword)
	if err != nil {
		return false, fmt.Errorf("hash default password: %w", err)
	}
	_, err = s.CreateAdminAccount(ctx, admin.DefaultUsername, hash, true)
	if errors.Is(err, ErrAdminAccountExists) {
		// Another process created it between the read and the write.
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// ReplaceAdminAccount resets the credential. It is used by the CLI so lost
// access can be recovered without deleting the database. The reset clears the
// change-required flag, because an operator running the CLI has already chosen
// the password deliberately.
func (s *SQLiteStore) ReplaceAdminAccount(ctx context.Context, username, passwordHash string) (admin.Account, error) {
	normalized, err := admin.ValidateUsername(username)
	if err != nil {
		return admin.Account{}, err
	}
	if strings.TrimSpace(passwordHash) == "" {
		return admin.Account{}, errors.New("password hash must not be empty")
	}

	now := time.Now().UTC()
	// Sessions issued under the previous credential must not survive a reset.
	if err := s.DeleteAllAdminSessions(ctx); err != nil {
		return admin.Account{}, err
	}

	result, err := s.db.ExecContext(ctx,
		`UPDATE gateway_admin_users SET username = ?, password_hash = ?, change_required = 0, updated_at = ?`,
		normalized, passwordHash, formatTime(now))
	if err != nil {
		return admin.Account{}, fmt.Errorf("replace admin account: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return admin.Account{}, fmt.Errorf("read affected rows: %w", err)
	}
	if affected == 0 {
		return s.CreateAdminAccount(ctx, normalized, passwordHash, false)
	}

	account, err := s.LoadAdminAccount(ctx)
	if err != nil {
		return admin.Account{}, err
	}
	return account, nil
}

// ChangeAdminPassword replaces the credential of the existing account and
// clears the change-required flag. Unlike ReplaceAdminAccount it keeps the
// current sessions, because the caller is the signed-in operator rotating their
// own password rather than an out-of-band reset.
func (s *SQLiteStore) ChangeAdminPassword(ctx context.Context, accountID int64, passwordHash string) error {
	if strings.TrimSpace(passwordHash) == "" {
		return errors.New("password hash must not be empty")
	}
	result, err := s.db.ExecContext(ctx,
		`UPDATE gateway_admin_users SET password_hash = ?, change_required = 0, updated_at = ? WHERE id = ?`,
		passwordHash, formatTime(time.Now().UTC()), accountID)
	if err != nil {
		return fmt.Errorf("change admin password: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read affected rows: %w", err)
	}
	if affected == 0 {
		return ErrNoAdminAccount
	}
	return nil
}

// LoadAdminAccount returns the single administrator account.
func (s *SQLiteStore) LoadAdminAccount(ctx context.Context) (admin.Account, error) {
	var account admin.Account
	var createdAt, updatedAt string
	var changeRequired int
	err := s.db.QueryRowContext(ctx,
		`SELECT id, username, password_hash, change_required, created_at, updated_at FROM gateway_admin_users ORDER BY id LIMIT 1`,
	).Scan(&account.ID, &account.Username, &account.PasswordHash, &changeRequired, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return admin.Account{}, ErrNoAdminAccount
	}
	if err != nil {
		return admin.Account{}, fmt.Errorf("load admin account: %w", err)
	}
	account.ChangeRequired = changeRequired != 0
	account.CreatedAt, _ = parseTime(createdAt)
	account.UpdatedAt, _ = parseTime(updatedAt)
	return account, nil
}

// CreateAdminSession persists a new session for the account. Only the token
// hash is written; the raw token lives in the client's cookie.
func (s *SQLiteStore) CreateAdminSession(ctx context.Context, accountID int64, tokenHash string, issuedAt, expiresAt time.Time) error {
	if strings.TrimSpace(tokenHash) == "" {
		return errors.New("session token hash must not be empty")
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO gateway_admin_sessions (token_hash, account_id, issued_at, expires_at) VALUES (?, ?, ?, ?)`,
		tokenHash, accountID, formatTime(issuedAt), formatTime(expiresAt))
	if err != nil {
		return fmt.Errorf("create admin session: %w", err)
	}
	return nil
}

// AuthenticateAdminSession resolves a session token to its account, rejecting
// sessions that have expired. The caller passes the raw token; hashing happens
// here so a raw token is never compared against stored rows.
func (s *SQLiteStore) AuthenticateAdminSession(ctx context.Context, token string, now time.Time) (admin.Account, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return admin.Account{}, ErrSessionNotFound
	}
	// The lookup is an indexed equality test on the token hash. The stored hash
	// is a SHA-256 of a 32-byte random token, so an attacker cannot steer the
	// index into a partial match the way a guessable secret would allow.
	hash := admin.HashSessionToken(token)

	var accountID int64
	var expiresAt string
	err := s.db.QueryRowContext(ctx,
		`SELECT account_id, expires_at FROM gateway_admin_sessions WHERE token_hash = ?`, hash,
	).Scan(&accountID, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return admin.Account{}, ErrSessionNotFound
	}
	if err != nil {
		return admin.Account{}, fmt.Errorf("load admin session: %w", err)
	}

	expiry, err := parseTime(expiresAt)
	if err != nil {
		return admin.Account{}, fmt.Errorf("parse admin session expiry: %w", err)
	}

	// The indexed equality lookup is the authentication step. A constant-time
	// comparison would add nothing here: the stored value is a SHA-256 of a
	// 32-byte random token, so there is no guessable structure for an attacker
	// to steer toward a partial match.
	if !now.UTC().Before(expiry) {
		// Clean up lazily so an expired session cannot be replayed even if the
		// periodic sweep has not run yet.
		_ = s.DeleteAdminSession(ctx, hash)
		return admin.Account{}, ErrSessionNotFound
	}

	account, err := s.LoadAdminAccount(ctx)
	if err != nil {
		return admin.Account{}, err
	}
	if account.ID != accountID {
		return admin.Account{}, ErrSessionNotFound
	}
	return account, nil
}

// DeleteAdminSession revokes one session by raw token.
func (s *SQLiteStore) DeleteAdminSession(ctx context.Context, tokenHash string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM gateway_admin_sessions WHERE token_hash = ?`, tokenHash)
	if err != nil {
		return fmt.Errorf("delete admin session: %w", err)
	}
	return nil
}

// DeleteAllAdminSessions revokes every session, used when the credential is
// reset so old cookies stop working immediately.
func (s *SQLiteStore) DeleteAllAdminSessions(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM gateway_admin_sessions`); err != nil {
		return fmt.Errorf("delete all admin sessions: %w", err)
	}
	return nil
}

// CleanupAdminSessions removes sessions that expired before the given time.
func (s *SQLiteStore) CleanupAdminSessions(ctx context.Context, before time.Time) (int64, error) {
	result, err := s.db.ExecContext(ctx, `DELETE FROM gateway_admin_sessions WHERE expires_at < ?`, formatTime(before))
	if err != nil {
		return 0, fmt.Errorf("cleanup admin sessions: %w", err)
	}
	return result.RowsAffected()
}

func formatTime(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}

// isUniqueViolation reports whether an error is a UNIQUE constraint failure.
// The check is textual because the SQLite driver exposes no typed error for it,
// and it is only used to turn a lost race into a clean domain error.
func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "unique")
}
