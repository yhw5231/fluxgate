package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"

	"github.com/yhw5231/fluxgate/internal/admin"
	"github.com/yhw5231/fluxgate/internal/config"
	"github.com/yhw5231/fluxgate/internal/store"
)

const adminUsage = `Manage the console administrator account.

Usage:
  fluxgate admin create [--username NAME] [--database PATH]
  fluxgate admin reset  [--username NAME] [--database PATH]
  fluxgate admin status [--database PATH]

The gateway creates a default administrator (admin / admin) automatically on
first start, so this command is only needed to set a different password up
front or to recover a lost one. The password is read from a terminal prompt with
echo disabled, so it never appears in the process list or shell history. Set
FLUXGATE_ADMIN_PASSWORD to supply it non-interactively instead.
`

// runAdmin implements the "admin" subcommand.
func runAdmin(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("expected one of: create, reset, status")
	}
	action := args[0]

	flags := flag.NewFlagSet("admin "+action, flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	flags.Usage = func() {
		fmt.Fprint(os.Stderr, adminUsage)
	}
	username := flags.String("username", "admin", "administrator account name")
	databasePath := flags.String("database", "", "path to hub.db (defaults to FLUXGATE_DATABASE_PATH)")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}

	switch action {
	case "create", "reset", "status":
	default:
		return fmt.Errorf("unknown admin action %q: expected create, reset, or status", action)
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if strings.TrimSpace(*databasePath) != "" {
		cfg.DatabasePath = *databasePath
	}

	persistentStore, err := store.OpenSQLite(cfg.DatabasePath)
	if err != nil {
		return err
	}
	defer persistentStore.Close()
	if err := persistentStore.EnsureAdminSchema(ctx); err != nil {
		return err
	}

	switch action {
	case "status":
		return reportAdminStatus(ctx, persistentStore)
	case "create":
		return createAdminAccount(ctx, persistentStore, *username)
	default: // reset
		return resetAdminAccount(ctx, persistentStore, *username)
	}
}

func reportAdminStatus(ctx context.Context, persistentStore *store.SQLiteStore) error {
	account, err := persistentStore.LoadAdminAccount(ctx)
	if errors.Is(err, store.ErrNoAdminAccount) {
		fmt.Println("no administrator account exists; run 'fluxgate admin create'")
		return nil
	}
	if err != nil {
		return err
	}
	fmt.Printf("administrator %q is configured (created %s)\n",
		account.Username, account.CreatedAt.UTC().Format("2006-01-02 15:04:05 UTC"))
	return nil
}

func createAdminAccount(ctx context.Context, persistentStore *store.SQLiteStore, username string) error {
	normalized, hash, err := prepareAdminCredential(username)
	if err != nil {
		return err
	}
	account, err := persistentStore.CreateAdminAccount(ctx, normalized, hash, false)
	if err != nil {
		if errors.Is(err, store.ErrAdminAccountExists) {
			return errors.New("an administrator account already exists; use 'fluxgate admin reset' to replace the credential")
		}
		return err
	}
	fmt.Printf("administrator %q is ready; sign in to the console at /console/\n", account.Username)
	return nil
}

func resetAdminAccount(ctx context.Context, persistentStore *store.SQLiteStore, username string) error {
	normalized, hash, err := prepareAdminCredential(username)
	if err != nil {
		return err
	}
	account, err := persistentStore.ReplaceAdminAccount(ctx, normalized, hash)
	if err != nil {
		return err
	}
	fmt.Printf("the credential for administrator %q is set\n", account.Username)
	fmt.Println("any existing console sessions were revoked and must sign in again")
	return nil
}

// prepareAdminCredential validates the account name and hashes a password.
func prepareAdminCredential(username string) (string, string, error) {
	normalized, err := admin.ValidateUsername(username)
	if err != nil {
		return "", "", err
	}
	password, err := readNewPassword()
	if err != nil {
		return "", "", err
	}
	hash, err := admin.HashPassword(password)
	if err != nil {
		return "", "", err
	}
	return normalized, hash, nil
}

// readNewPassword reads the password twice from the terminal. FLUXGATE_ADMIN_PASSWORD
// takes precedence so a non-interactive deployment (container entrypoint, CI) can
// set the credential without a TTY.
func readNewPassword() (string, error) {
	if fromEnv := os.Getenv("FLUXGATE_ADMIN_PASSWORD"); fromEnv != "" {
		if err := admin.ValidatePassword(fromEnv); err != nil {
			return "", fmt.Errorf("FLUXGATE_ADMIN_PASSWORD: %w", err)
		}
		return fromEnv, nil
	}

	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return "", errors.New("stdin is not a terminal: set FLUXGATE_ADMIN_PASSWORD to provide the password non-interactively")
	}

	fmt.Print("Password: ")
	first, err := term.ReadPassword(fd)
	fmt.Println()
	if err != nil {
		return "", fmt.Errorf("read password: %w", err)
	}
	if err := admin.ValidatePassword(string(first)); err != nil {
		return "", err
	}

	fmt.Print("Confirm password: ")
	second, err := term.ReadPassword(fd)
	fmt.Println()
	if err != nil {
		return "", fmt.Errorf("read password confirmation: %w", err)
	}
	if string(first) != string(second) {
		return "", errors.New("the two passwords do not match")
	}
	return string(first), nil
}
