package main

import (
	"context"
	"crypto/tls"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jayk56/lazarus/internal/store"
)

func TestBackupKeepEnvIsStrict(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  int
		ok    bool
	}{
		{name: "unset", want: 7, ok: true},
		{name: "valid", value: "12", want: 12, ok: true},
		{name: "leading and trailing whitespace", value: " 12 ", want: 12, ok: true},
		{name: "suffix", value: "12x"},
		{name: "decimal", value: "1.2"},
		{name: "zero", value: "0"},
		{name: "negative", value: "-1"},
		{name: "above maximum", value: "1001"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.value == "" {
				t.Setenv("LAZARUS_BACKUP_KEEP", "")
			} else {
				t.Setenv("LAZARUS_BACKUP_KEEP", tt.value)
			}
			got, err := backupKeepEnv()
			if tt.ok {
				if err != nil || got != tt.want {
					t.Fatalf("backupKeepEnv() = %d, %v; want %d, nil", got, err, tt.want)
				}
				return
			}
			if err == nil {
				t.Fatalf("backupKeepEnv() = %d, nil; want an error", got)
			}
		})
	}
}

func TestBackupMinAgeEnvIsStrict(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  time.Duration
		ok    bool
	}{
		{name: "unset", want: 24 * time.Hour, ok: true},
		{name: "valid", value: "90m", want: 90 * time.Minute, ok: true},
		{name: "zero", value: "0s"},
		{name: "negative", value: "-1h"},
		{name: "malformed", value: "24 hours"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("LAZARUS_BACKUP_MIN_AGE", tt.value)
			got, err := durationEnv("LAZARUS_BACKUP_MIN_AGE", 24*time.Hour)
			if tt.ok {
				if err != nil || got != tt.want {
					t.Fatalf("durationEnv() = %s, %v; want %s, nil", got, err, tt.want)
				}
				return
			}
			if err == nil {
				t.Fatalf("durationEnv() = %s, nil; want an error", got)
			}
		})
	}
}

func TestLazarusTLSConfigRequiresTLS12(t *testing.T) {
	config := lazarusTLSConfig()
	if config.MinVersion != tls.VersionTLS12 {
		t.Fatalf("TLS minimum = %d, want TLS 1.2 (%d)", config.MinVersion, tls.VersionTLS12)
	}
}

func TestRestorePreservesDatabaseAndSidecars(t *testing.T) {
	dir := t.TempDir()
	source, err := store.Open(context.Background(), filepath.Join(dir, "source.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.CreateMaintenance(context.Background(), store.Maintenance{ID: "restore-test"}, store.AuditContext{
		Actor: "test", Role: "admin", RequestID: "request",
	}); err != nil {
		t.Fatal(err)
	}
	manifest, err := source.Backup(context.Background(), filepath.Join(dir, "backups"), 1, "test", "admin", "backup")
	if err != nil {
		t.Fatal(err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(dir, "backups", manifest.Filename)
	manifestPath := backup + ".manifest.json"
	destination := filepath.Join(dir, "destination.db")
	for _, path := range []string{destination, destination + "-wal", destination + "-shm"} {
		if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := restoreDatabase(destination, backup, manifestPath, false); err == nil {
		t.Fatal("restore replaced an existing database without --replace")
	}
	if err := restoreDatabase(destination, backup, manifestPath, true); err != nil {
		t.Fatal(err)
	}
	if _, err := store.VerifyPath(context.Background(), destination); err != nil {
		t.Fatal(err)
	}
	rollbacks, err := filepath.Glob(destination + ".rollback-*")
	if err != nil || len(rollbacks) != 3 {
		t.Fatalf("rollback files = %v, %v", rollbacks, err)
	}
}

func TestRestoreAllowsExplicitReplaceWhenDestinationIsAbsent(t *testing.T) {
	dir := t.TempDir()
	source, err := store.Open(context.Background(), filepath.Join(dir, "source.db"))
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := source.Backup(context.Background(), filepath.Join(dir, "backups"), 1, "test", "admin", "backup")
	if err != nil {
		t.Fatal(err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(dir, "backups", manifest.Filename)
	if err := restoreDatabase(filepath.Join(dir, "new.db"), backup, backup+".manifest.json", true); err != nil {
		t.Fatal(err)
	}
}

func TestRestoreRejectsBackupSidecars(t *testing.T) {
	dir := t.TempDir()
	source, err := store.Open(context.Background(), filepath.Join(dir, "source.db"))
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := source.Backup(context.Background(), filepath.Join(dir, "backups"), 1, "test", "admin", "backup")
	if err != nil {
		t.Fatal(err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(dir, "backups", manifest.Filename)
	if err := os.WriteFile(backup+"-wal", []byte("not-standalone"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := restoreDatabase(filepath.Join(dir, "new.db"), backup, backup+".manifest.json", false); err == nil {
		t.Fatal("restore accepted a backup with a WAL sidecar")
	}
}

func TestRestorePreservesOrphanSQLiteSidecars(t *testing.T) {
	dir := t.TempDir()
	source, err := store.Open(context.Background(), filepath.Join(dir, "source.db"))
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := source.Backup(context.Background(), filepath.Join(dir, "backups"), 1, "test", "admin", "backup")
	if err != nil {
		t.Fatal(err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(dir, "backups", manifest.Filename)
	destination := filepath.Join(dir, "orphan.db")
	if err := os.WriteFile(destination+"-wal", []byte("orphan"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := restoreDatabase(destination, backup, backup+".manifest.json", true); err != nil {
		t.Fatal(err)
	}
	if _, err := store.VerifyPath(context.Background(), destination); err != nil {
		t.Fatal(err)
	}
	rollbacks, err := filepath.Glob(destination + ".rollback-*-wal")
	if err != nil || len(rollbacks) != 1 {
		t.Fatalf("orphan rollback files = %v, %v", rollbacks, err)
	}
}
