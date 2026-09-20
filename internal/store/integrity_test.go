package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVerifyIntegrityAcceptsAHealthyDatabase(t *testing.T) {
	store := openTestStore(t)
	if err := store.VerifyIntegrity(context.Background()); err != nil {
		t.Fatalf("VerifyIntegrity() error = %v", err)
	}
}

// A damaged file must be reported at startup instead of silently serving
// routing data from indexes that no longer match the tables.
func TestVerifyIntegrityRejectsADamagedDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "damaged.db")
	healthy := openTestStoreAt(t, path)
	if err := healthy.EnsureBreakerSchema(context.Background()); err != nil {
		t.Fatalf("EnsureBreakerSchema() error = %v", err)
	}
	if err := healthy.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	// Overwrite a page in the middle of the file while leaving the header, so
	// SQLite still opens the database but the b-tree no longer matches.
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read database: %v", err)
	}
	for offset := len(contents) / 2; offset < len(contents)/2+64; offset++ {
		contents[offset] ^= 0xFF
	}
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatalf("write database: %v", err)
	}

	damaged, err := OpenSQLite(path)
	if err != nil {
		// Refusing to open a corrupt file is an acceptable outcome.
		return
	}
	defer damaged.Close()
	if err := damaged.VerifyIntegrity(context.Background()); err == nil {
		t.Fatal("VerifyIntegrity() = nil for a damaged database")
	} else if !strings.Contains(err.Error(), "integrity") {
		t.Fatalf("VerifyIntegrity() error = %v, want an integrity failure", err)
	}
}

func TestSQLiteDSNAppliesABusyTimeout(t *testing.T) {
	if got := sqliteDSN(filepath.Join("tmp", "hub.db")); !strings.Contains(got, "busy_timeout") {
		t.Fatalf("sqliteDSN() = %q, want a busy timeout", got)
	}
	// Existing URIs and paths this builder cannot quote safely are untouched.
	if got := sqliteDSN("file:custom.db?mode=ro"); got != "file:custom.db?mode=ro" {
		t.Fatalf("sqliteDSN() rewrote an existing URI: %q", got)
	}
	if got := sqliteDSN("odd#name.db"); got != "odd#name.db" {
		t.Fatalf("sqliteDSN() rewrote an unusual path: %q", got)
	}
}

func openTestStoreAt(t *testing.T, path string) *SQLiteStore {
	t.Helper()
	store, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite() error = %v", err)
	}
	return store
}
