package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
)

// startupQuickCheck checks SQLite before the server accepts traffic. It skips
// the slower capture and event checks performed by the verify command. Backup
// creation also runs those full checks.
func startupQuickCheck(ctx context.Context, db *sql.DB) error {
	var result string
	if err := db.QueryRowContext(ctx, "PRAGMA quick_check(1)").Scan(&result); err != nil {
		return fmt.Errorf("sqlite startup quick check: %w", err)
	}
	if !strings.EqualFold(result, "ok") {
		return fmt.Errorf("%w: %s", ErrCorrupt, result)
	}
	return nil
}

// Ready uses a separate read-only connection so backups and writes do not block
// the Kubernetes readiness check.
func (s *Store) Ready(ctx context.Context) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("store is not initialized")
	}
	if isMemoryPath(s.path) {
		return s.db.PingContext(ctx)
	}
	abs, err := filepath.Abs(s.path)
	if err != nil {
		return err
	}
	uri := (&url.URL{Scheme: "file", Path: abs}).String() + "?mode=ro"
	db, err := sql.Open("sqlite", uri)
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if _, err := db.ExecContext(ctx, "PRAGMA query_only=ON"); err != nil {
		return err
	}
	var schemaVersion int
	if err := db.QueryRowContext(ctx, "PRAGMA schema_version").Scan(&schemaVersion); err != nil {
		return err
	}
	if schemaVersion < 1 {
		return fmt.Errorf("unexpected readiness schema version")
	}
	return nil
}
