package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

var backupMu sync.Mutex

// Backup creates and checks a standalone SQLite backup, then publishes it with
// a matching manifest.
func (s *Store) Backup(ctx context.Context, dir string, keep int, actor, role, requestID string) (BackupManifest, error) {
	return s.BackupWithRetention(ctx, dir, keep, 0, actor, role, requestID)
}

// BackupWithRetention uses a separate SQLite connection so reads, readiness
// checks, and ordinary writes can continue while it copies and checks the data.
func (s *Store) BackupWithRetention(ctx context.Context, dir string, keep int, minAge time.Duration, actor, role, requestID string) (BackupManifest, error) {
	if strings.TrimSpace(dir) == "" {
		return BackupManifest{}, fmt.Errorf("backup directory is required")
	}
	if minAge < 0 {
		return BackupManifest{}, fmt.Errorf("backup minimum age must not be negative")
	}
	if keep <= 0 {
		keep = defaultKeepBack
	}
	if keep > 1000 {
		keep = 1000
	}
	if err := os.MkdirAll(dir, 0o770); err != nil {
		return BackupManifest{}, fmt.Errorf("create backup directory: %w", err)
	}

	backupMu.Lock()
	defer backupMu.Unlock()
	if err := cleanupBackupTemps(dir); err != nil {
		return BackupManifest{}, err
	}
	if existing, ok, err := s.backupForRequest(ctx, dir, requestID, actor); err != nil {
		return BackupManifest{}, err
	} else if ok {
		return existing, nil
	}
	snapshotDB, err := openBackupConnection(ctx, s.path)
	if err != nil {
		return BackupManifest{}, err
	}
	defer snapshotDB.Close()
	if err := integrityCheckDB(ctx, snapshotDB); err != nil {
		return BackupManifest{}, err
	}
	var sqliteVersion string
	if err := snapshotDB.QueryRowContext(ctx, "SELECT sqlite_version()").Scan(&sqliteVersion); err != nil {
		return BackupManifest{}, err
	}

	now := s.now()
	base := fmt.Sprintf("lazarus-%s-%d.db", now.UTC().Format("20060102T150405.000000000Z"), now.UnixNano())
	finalPath := filepath.Join(dir, base)
	tmpPath := finalPath + ".tmp"
	_ = os.Remove(tmpPath)
	// SQLite requires a new path. The API does not expose this temporary file,
	// and a failed backup removes it.
	if _, err := snapshotDB.ExecContext(ctx, "VACUUM INTO ?", tmpPath); err != nil {
		_ = os.Remove(tmpPath)
		return BackupManifest{}, fmt.Errorf("vacuum into: %w", err)
	}
	defer os.Remove(tmpPath)
	if err := os.Chmod(tmpPath, 0o660); err != nil {
		return BackupManifest{}, fmt.Errorf("set backup permissions: %w", err)
	}

	firstVerification, err := VerifyPath(ctx, tmpPath)
	if err != nil || firstVerification.ApplicationData != "ok" {
		return BackupManifest{}, fmt.Errorf("backup application validation failed: %w", err)
	}

	backupFile, err := os.Open(tmpPath)
	if err != nil {
		return BackupManifest{}, fmt.Errorf("open backup for hashing: %w", err)
	}
	hash := sha256.New()
	bytesWritten, copyErr := io.Copy(hash, backupFile)
	closeErr := backupFile.Close()
	if copyErr != nil {
		return BackupManifest{}, fmt.Errorf("hash backup: %w", copyErr)
	}
	if closeErr != nil {
		return BackupManifest{}, fmt.Errorf("close backup after hashing: %w", closeErr)
	}
	manifest := BackupManifest{
		Filename:       base,
		CreatedAt:      now,
		Bytes:          bytesWritten,
		SHA256:         hex.EncodeToString(hash.Sum(nil)),
		SQLiteVersion:  sqliteVersion,
		IntegrityCheck: firstVerification.IntegrityCheck,
		ForeignKeys:    firstVerification.ForeignKeys,
		Application:    firstVerification.ApplicationData,
	}
	// Check the file again after hashing so a change during hashing cannot be
	// published as a valid backup.
	verified, err := VerifyPath(ctx, tmpPath)
	if err != nil || verified.ApplicationData != "ok" {
		return BackupManifest{}, fmt.Errorf("backup application validation failed: %w", err)
	}
	if err := syncFile(tmpPath); err != nil {
		return BackupManifest{}, fmt.Errorf("sync backup: %w", err)
	}
	if err := os.Rename(tmpPath, finalPath); err != nil {
		return BackupManifest{}, fmt.Errorf("publish backup: %w", err)
	}
	if err := syncDir(dir); err != nil {
		return BackupManifest{}, fmt.Errorf("sync published backup: %w", err)
	}
	// A published and verified backup still succeeds if its event cannot be
	// saved. The manifest reports that gap so clients do not create unnecessary
	// backups and remove an older recovery point.
	manifest.AuditRecorded = s.RecordBackupCreated(ctx, manifest, AuditContext{Actor: actor, Role: role, RequestID: requestID}) == nil
	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return BackupManifest{}, err
	}
	manifestTmp := finalPath + ".manifest.json.tmp"
	manifestPath := finalPath + ".manifest.json"
	if err := os.WriteFile(manifestTmp, append(manifestBytes, '\n'), 0o660); err != nil {
		return BackupManifest{}, fmt.Errorf("write backup manifest: %w", err)
	}
	if err := syncFile(manifestTmp); err != nil {
		return BackupManifest{}, fmt.Errorf("sync backup manifest: %w", err)
	}
	if err := os.Rename(manifestTmp, manifestPath); err != nil {
		_ = os.Remove(manifestTmp)
		return BackupManifest{}, fmt.Errorf("publish backup manifest: %w", err)
	}
	if err := syncFile(manifestPath); err != nil {
		return BackupManifest{}, fmt.Errorf("sync backup manifest: %w", err)
	}
	if err := syncDir(dir); err != nil {
		return BackupManifest{}, fmt.Errorf("sync backup directory: %w", err)
	}
	if err := rotateBackupsWithAge(dir, keep, minAge, now); err != nil {
		return BackupManifest{}, err
	}
	return manifest, nil
}

func openBackupConnection(ctx context.Context, path string) (*sql.DB, error) {
	if isMemoryPath(path) {
		return nil, fmt.Errorf("online backup requires a file-backed database")
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open backup snapshot connection: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	for _, pragma := range []string{
		"PRAGMA busy_timeout=5000",
		"PRAGMA foreign_keys=ON",
	} {
		if _, err := db.ExecContext(ctx, pragma); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("configure backup snapshot connection: %w", err)
		}
	}
	return db, nil
}

func integrityCheckDB(ctx context.Context, db *sql.DB) error {
	var integrity string
	if err := db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil {
		return fmt.Errorf("sqlite integrity check: %w", err)
	}
	if !strings.EqualFold(integrity, "ok") {
		return fmt.Errorf("%w: %s", ErrCorrupt, integrity)
	}
	rows, err := db.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return fmt.Errorf("sqlite foreign key check: %w", err)
	}
	defer rows.Close()
	if rows.Next() {
		return fmt.Errorf("%w: foreign key violation", ErrCorrupt)
	}
	return rows.Err()
}

func cleanupBackupTemps(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, "lazarus-") || !isBackupTempName(name) {
			continue
		}
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove incomplete backup %s: %w", name, err)
		}
	}
	entries, err = os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "lazarus-") || !strings.HasSuffix(entry.Name(), ".db") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		if _, err := os.Stat(path + ".manifest.json"); err == nil {
			continue
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("inspect incomplete backup %s: %w", entry.Name(), err)
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove incomplete backup %s: %w", entry.Name(), err)
		}
	}
	entries, err = os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		const manifestSuffix = ".db.manifest.json"
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "lazarus-") || !strings.HasSuffix(entry.Name(), manifestSuffix) {
			continue
		}
		manifestPath := filepath.Join(dir, entry.Name())
		databasePath := strings.TrimSuffix(manifestPath, ".manifest.json")
		if _, err := os.Stat(databasePath); err == nil {
			continue
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("inspect orphan manifest %s: %w", entry.Name(), err)
		}
		if err := os.Remove(manifestPath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove orphan manifest %s: %w", entry.Name(), err)
		}
	}
	return nil
}

func isBackupTempName(name string) bool {
	for _, suffix := range []string{".tmp", ".tmp-wal", ".tmp-shm", ".tmp-journal"} {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return false
}

func syncFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func syncDir(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func (s *Store) RecordBackupCreated(ctx context.Context, manifest BackupManifest, audit AuditContext) error {
	// The event exists only if this transaction commits, so its saved manifest
	// records audit_recorded=true. The caller keeps false if the write fails.
	manifest.AuditRecorded = true
	detail, _ := json.Marshal(manifest)
	return s.write(ctx, func(tx *sql.Tx) error {
		actor, role, requestID := auditValues(audit)
		return appendEvent(ctx, tx, Event{Type: "backup.created", OccurredAt: manifest.CreatedAt,
			RequestID: requestID, Actor: actor, Role: role, ResourceType: "backup",
			ResourceID: manifest.Filename, Payload: detail})
	})
}

func (s *Store) backupForRequest(ctx context.Context, dir, requestID, actor string) (BackupManifest, bool, error) {
	if strings.TrimSpace(requestID) == "" {
		return BackupManifest{}, false, nil
	}
	var detail string
	err := s.db.QueryRowContext(ctx, `SELECT payload_json FROM journal_events WHERE event_type='backup.created' AND request_id=? AND actor=? ORDER BY journal_id DESC LIMIT 1`, requestID, actor).Scan(&detail)
	if err != nil {
		if err == sql.ErrNoRows {
			return BackupManifest{}, false, nil
		}
		return BackupManifest{}, false, err
	}
	var manifest BackupManifest
	if err := json.Unmarshal([]byte(detail), &manifest); err != nil || manifest.Filename == "" {
		return BackupManifest{}, false, fmt.Errorf("%w: saved backup event is invalid", ErrCorrupt)
	}
	if filepath.Base(manifest.Filename) != manifest.Filename {
		return BackupManifest{}, false, fmt.Errorf("%w: saved backup event has an invalid filename", ErrCorrupt)
	}
	_, validated, validateErr := BackupArtifact(ctx, dir, manifest.Filename)
	if validateErr != nil {
		// The process may have stopped after saving the event but before publishing
		// the manifest. The client can retry; cleanupBackupTemps removed any
		// incomplete file.
		return BackupManifest{}, false, nil
	}
	validated.AuditRecorded = true
	return validated, true, nil
}

func rotateBackupsWithAge(dir string, keep int, minAge time.Duration, now time.Time) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	type item struct {
		path string
	}
	var items []item
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "lazarus-") || !strings.HasSuffix(entry.Name(), ".db") {
			continue
		}
		items = append(items, item{path: filepath.Join(dir, entry.Name())})
	}
	sort.Slice(items, func(i, j int) bool { return filepath.Base(items[i].path) > filepath.Base(items[j].path) })
	if len(items) <= keep {
		return nil
	}
	for _, old := range items[keep:] {
		if minAge > 0 {
			info, err := os.Stat(old.path)
			if err != nil {
				if os.IsNotExist(err) {
					continue
				}
				return fmt.Errorf("inspect backup age %s: %w", filepath.Base(old.path), err)
			}
			if now.Sub(info.ModTime()) < minAge {
				continue
			}
		}
		if err := os.Remove(old.path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("rotate backup %s: %w", filepath.Base(old.path), err)
		}
		_ = os.Remove(old.path + ".manifest.json")
	}
	return syncDir(dir)
}
