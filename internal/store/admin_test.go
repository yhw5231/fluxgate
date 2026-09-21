package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/yhw5231/fluxgate/internal/admin"
)

// newAdminTestStore returns a store with the authentication schema prepared and
// an administrator account created.
func newAdminTestStore(t *testing.T) (*SQLiteStore, admin.Account) {
	t.Helper()
	store := openTestStore(t)
	ctx := context.Background()
	if err := store.EnsureAdminSchema(ctx); err != nil {
		t.Fatalf("EnsureAdminSchema() error = %v", err)
	}
	hash, err := admin.HashPassword("correct-horse-battery-staple")
	if err != nil {
		t.Fatalf("HashPassword() error = %v", err)
	}
	account, err := store.CreateAdminAccount(ctx, "admin", hash)
	if err != nil {
		t.Fatalf("CreateAdminAccount() error = %v", err)
	}
	return store, account
}

func TestEnsureAdminSchemaIsIdempotent(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		if err := store.EnsureAdminSchema(ctx); err != nil {
			t.Fatalf("EnsureAdminSchema() run %d error = %v", i+1, err)
		}
	}
}

func TestLoadAdminAccountWithoutAccount(t *testing.T) {
	store := openTestStore(t)
	if err := store.EnsureAdminSchema(context.Background()); err != nil {
		t.Fatalf("EnsureAdminSchema() error = %v", err)
	}

	if _, err := store.LoadAdminAccount(context.Background()); !errors.Is(err, ErrNoAdminAccount) {
		t.Fatalf("LoadAdminAccount() error = %v, want ErrNoAdminAccount", err)
	}
}

func TestCreateAdminAccountStoresHashedCredential(t *testing.T) {
	store, account := newAdminTestStore(t)

	if account.ID == 0 {
		t.Fatal("account ID was not assigned")
	}
	if account.Username != "admin" {
		t.Fatalf("username = %q, want admin", account.Username)
	}
	if account.CreatedAt.IsZero() {
		t.Fatal("created_at was not recorded")
	}

	loaded, err := store.LoadAdminAccount(context.Background())
	if err != nil {
		t.Fatalf("LoadAdminAccount() error = %v", err)
	}
	if loaded.PasswordHash != account.PasswordHash {
		t.Fatal("stored password hash differs from the created one")
	}
	if !strings.HasPrefix(loaded.PasswordHash, "$2") {
		t.Fatalf("stored hash = %q, want a bcrypt hash", loaded.PasswordHash)
	}
	if err := admin.VerifyPassword(loaded.PasswordHash, "correct-horse-battery-staple"); err != nil {
		t.Fatalf("the stored hash does not verify the original password: %v", err)
	}
}

// A second administrator would break the single-account guarantee the console
// depends on, so the database must refuse it even under a different username.
func TestCreateAdminAccountRejectsSecondAccount(t *testing.T) {
	store, _ := newAdminTestStore(t)
	ctx := context.Background()

	hash, err := admin.HashPassword("another-strong-password")
	if err != nil {
		t.Fatalf("HashPassword() error = %v", err)
	}
	if _, err := store.CreateAdminAccount(ctx, "second", hash); !errors.Is(err, ErrAdminAccountExists) {
		t.Fatalf("CreateAdminAccount() with a different username error = %v, want ErrAdminAccountExists", err)
	}

	var count int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM gateway_admin_users`).Scan(&count); err != nil {
		t.Fatalf("count admin accounts: %v", err)
	}
	if count != 1 {
		t.Fatalf("admin account count = %d, want 1", count)
	}
}

func TestCreateAdminAccountRejectsDuplicateUsername(t *testing.T) {
	store, _ := newAdminTestStore(t)
	ctx := context.Background()

	hash, err := admin.HashPassword("another-strong-password")
	if err != nil {
		t.Fatalf("HashPassword() error = %v", err)
	}
	if _, err := store.CreateAdminAccount(ctx, "admin", hash); !errors.Is(err, ErrAdminAccountExists) {
		t.Fatalf("CreateAdminAccount() with the same username error = %v, want ErrAdminAccountExists", err)
	}
}

func TestCreateAdminAccountHonorsUsernameRules(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	if err := store.EnsureAdminSchema(ctx); err != nil {
		t.Fatalf("EnsureAdminSchema() error = %v", err)
	}
	hash, err := admin.HashPassword("correct-horse-battery-staple")
	if err != nil {
		t.Fatalf("HashPassword() error = %v", err)
	}

	if _, err := store.CreateAdminAccount(ctx, "   ", hash); err == nil {
		t.Fatal("CreateAdminAccount() accepted an empty username")
	}
	if _, err := store.CreateAdminAccount(ctx, "admin", "   "); err == nil {
		t.Fatal("CreateAdminAccount() accepted an empty password hash")
	}
}

func TestReplaceAdminAccountUpdatesCredentialAndRevokesSessions(t *testing.T) {
	store, account := newAdminTestStore(t)
	ctx := context.Background()

	// Establish a live session that the reset must invalidate.
	token, tokenHash, err := admin.NewSessionToken()
	if err != nil {
		t.Fatalf("NewSessionToken() error = %v", err)
	}
	now := time.Now().UTC()
	if err := store.CreateAdminSession(ctx, account.ID, tokenHash, now, now.Add(time.Hour)); err != nil {
		t.Fatalf("CreateAdminSession() error = %v", err)
	}
	if _, err := store.AuthenticateAdminSession(ctx, token, now); err != nil {
		t.Fatalf("the freshly created session did not authenticate: %v", err)
	}

	newHash, err := admin.HashPassword("a-completely-different-password")
	if err != nil {
		t.Fatalf("HashPassword() error = %v", err)
	}
	replaced, err := store.ReplaceAdminAccount(ctx, "operator", newHash)
	if err != nil {
		t.Fatalf("ReplaceAdminAccount() error = %v", err)
	}
	if replaced.Username != "operator" {
		t.Fatalf("username = %q, want operator", replaced.Username)
	}
	if err := admin.VerifyPassword(replaced.PasswordHash, "a-completely-different-password"); err != nil {
		t.Fatalf("the replaced hash does not verify the new password: %v", err)
	}
	if _, err := store.AuthenticateAdminSession(ctx, token, time.Now().UTC()); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("a session survived the credential reset: error = %v", err)
	}
}

func TestReplaceAdminAccountCreatesWhenMissing(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	if err := store.EnsureAdminSchema(ctx); err != nil {
		t.Fatalf("EnsureAdminSchema() error = %v", err)
	}
	hash, err := admin.HashPassword("correct-horse-battery-staple")
	if err != nil {
		t.Fatalf("HashPassword() error = %v", err)
	}

	account, err := store.ReplaceAdminAccount(ctx, "admin", hash)
	if err != nil {
		t.Fatalf("ReplaceAdminAccount() on an empty database error = %v", err)
	}
	if account.Username != "admin" {
		t.Fatalf("username = %q, want admin", account.Username)
	}
}

func TestAdminSessionLifecycle(t *testing.T) {
	store, account := newAdminTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	token, tokenHash, err := admin.NewSessionToken()
	if err != nil {
		t.Fatalf("NewSessionToken() error = %v", err)
	}
	if err := store.CreateAdminSession(ctx, account.ID, tokenHash, now, now.Add(time.Hour)); err != nil {
		t.Fatalf("CreateAdminSession() error = %v", err)
	}

	authenticated, err := store.AuthenticateAdminSession(ctx, token, now)
	if err != nil {
		t.Fatalf("AuthenticateAdminSession() error = %v", err)
	}
	if authenticated.ID != account.ID {
		t.Fatalf("authenticated account ID = %d, want %d", authenticated.ID, account.ID)
	}

	if err := store.DeleteAdminSession(ctx, tokenHash); err != nil {
		t.Fatalf("DeleteAdminSession() error = %v", err)
	}
	if _, err := store.AuthenticateAdminSession(ctx, token, now); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("a revoked session still authenticates: error = %v", err)
	}
}

func TestAuthenticateAdminSessionRejectsUnknownAndEmptyTokens(t *testing.T) {
	store, _ := newAdminTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	if _, err := store.AuthenticateAdminSession(ctx, "", now); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("empty token error = %v, want ErrSessionNotFound", err)
	}
	if _, err := store.AuthenticateAdminSession(ctx, "   ", now); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("blank token error = %v, want ErrSessionNotFound", err)
	}
	if _, err := store.AuthenticateAdminSession(ctx, "not-a-real-token", now); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("unknown token error = %v, want ErrSessionNotFound", err)
	}
}

// The raw token must never be what is stored, so a database leak cannot be
// replayed as a live session.
func TestAdminSessionStoresOnlyTheTokenHash(t *testing.T) {
	store, account := newAdminTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	token, tokenHash, err := admin.NewSessionToken()
	if err != nil {
		t.Fatalf("NewSessionToken() error = %v", err)
	}
	if err := store.CreateAdminSession(ctx, account.ID, tokenHash, now, now.Add(time.Hour)); err != nil {
		t.Fatalf("CreateAdminSession() error = %v", err)
	}

	var stored string
	if err := store.db.QueryRowContext(ctx, `SELECT token_hash FROM gateway_admin_sessions`).Scan(&stored); err != nil {
		t.Fatalf("read stored token hash: %v", err)
	}
	if stored == token {
		t.Fatal("the raw session token was stored in the database")
	}
	if stored != admin.HashSessionToken(token) {
		t.Fatal("the stored value is not the token hash")
	}
}

func TestAuthenticateAdminSessionRejectsExpiredSession(t *testing.T) {
	store, account := newAdminTestStore(t)
	ctx := context.Background()
	issuedAt := time.Now().UTC().Add(-2 * time.Hour)

	token, tokenHash, err := admin.NewSessionToken()
	if err != nil {
		t.Fatalf("NewSessionToken() error = %v", err)
	}
	// Expired one hour ago.
	if err := store.CreateAdminSession(ctx, account.ID, tokenHash, issuedAt, issuedAt.Add(time.Hour)); err != nil {
		t.Fatalf("CreateAdminSession() error = %v", err)
	}

	if _, err := store.AuthenticateAdminSession(ctx, token, time.Now().UTC()); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("an expired session authenticated: error = %v", err)
	}

	// The expired session is cleaned up rather than left to be retried.
	var remaining int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM gateway_admin_sessions`).Scan(&remaining); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("expired session rows remaining = %d, want 0", remaining)
	}
}

func TestCleanupAdminSessions(t *testing.T) {
	store, account := newAdminTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	expiredToken, expiredHash, err := admin.NewSessionToken()
	if err != nil {
		t.Fatalf("NewSessionToken() error = %v", err)
	}
	liveToken, liveHash, err := admin.NewSessionToken()
	if err != nil {
		t.Fatalf("NewSessionToken() error = %v", err)
	}
	if err := store.CreateAdminSession(ctx, account.ID, expiredHash, now.Add(-2*time.Hour), now.Add(-time.Hour)); err != nil {
		t.Fatalf("CreateAdminSession() expired error = %v", err)
	}
	if err := store.CreateAdminSession(ctx, account.ID, liveHash, now, now.Add(time.Hour)); err != nil {
		t.Fatalf("CreateAdminSession() live error = %v", err)
	}

	removed, err := store.CleanupAdminSessions(ctx, now)
	if err != nil {
		t.Fatalf("CleanupAdminSessions() error = %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}
	if _, err := store.AuthenticateAdminSession(ctx, expiredToken, now); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("the expired session survived cleanup: %v", err)
	}
	if _, err := store.AuthenticateAdminSession(ctx, liveToken, now); err != nil {
		t.Errorf("cleanup removed a live session: %v", err)
	}
}

func TestDeleteAllAdminSessions(t *testing.T) {
	store, account := newAdminTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	for i := 0; i < 3; i++ {
		_, tokenHash, err := admin.NewSessionToken()
		if err != nil {
			t.Fatalf("NewSessionToken() error = %v", err)
		}
		if err := store.CreateAdminSession(ctx, account.ID, tokenHash, now, now.Add(time.Hour)); err != nil {
			t.Fatalf("CreateAdminSession() error = %v", err)
		}
	}

	if err := store.DeleteAllAdminSessions(ctx); err != nil {
		t.Fatalf("DeleteAllAdminSessions() error = %v", err)
	}
	var remaining int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM gateway_admin_sessions`).Scan(&remaining); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("remaining sessions = %d, want 0", remaining)
	}
}

// The authentication tables are gateway-owned, so they must not be mistaken for
// part of the upstream configuration schema that the gateway validates.
func TestAdminTablesAreNotPartOfUpstreamSchema(t *testing.T) {
	for _, name := range requiredUpstreamTables {
		if strings.HasPrefix(name, "gateway_admin") {
			t.Fatalf("upstream schema unexpectedly requires %q", name)
		}
	}
}
