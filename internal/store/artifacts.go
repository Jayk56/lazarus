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
	"strings"
)

// BackupArtifact finds a published backup or manifest and validates the pair.
// It never exposes the live database, temporary files, unrelated paths, or an
// incomplete backup.
func BackupArtifact(ctx context.Context, dir, name string) (string, BackupManifest, error) {
	if len(name) == 0 || len(name) > 200 || filepath.Base(name) != name || strings.ContainsAny(name, `/\\`) {
		return "", BackupManifest{}, fmt.Errorf("%w: invalid backup artifact name", ErrInvalid)
	}
	for _, char := range name {
		if !(char >= 'a' && char <= 'z') && !(char >= 'A' && char <= 'Z') && !(char >= '0' && char <= '9') && !strings.ContainsRune("._-", char) {
			return "", BackupManifest{}, fmt.Errorf("%w: invalid backup artifact name", ErrInvalid)
		}
	}
	wantsManifest := strings.HasSuffix(name, ".db.manifest.json")
	databaseName := name
	if wantsManifest {
		databaseName = strings.TrimSuffix(name, ".manifest.json")
	}
	if !strings.HasPrefix(databaseName, "lazarus-") || !strings.HasSuffix(databaseName, ".db") {
		return "", BackupManifest{}, fmt.Errorf("%w: invalid backup artifact name", ErrInvalid)
	}
	databasePath := filepath.Join(dir, databaseName)
	manifestPath := databasePath + ".manifest.json"
	if err := validateRegularFile(manifestPath); err != nil {
		return "", BackupManifest{}, err
	}
	manifest, err := readBackupManifest(manifestPath)
	if err != nil {
		return "", BackupManifest{}, err
	}
	if manifest.Filename != databaseName {
		return "", BackupManifest{}, fmt.Errorf("%w: backup manifest filename mismatch", ErrCorrupt)
	}
	if err := validateRegularFile(databasePath); err != nil {
		return "", BackupManifest{}, err
	}
	if err := validateBackupDigest(databasePath, manifest); err != nil {
		return "", BackupManifest{}, err
	}
	verified, err := VerifyPath(ctx, databasePath)
	if err != nil || verified.ApplicationData != "ok" {
		return "", BackupManifest{}, fmt.Errorf("validate backup artifact: %w", err)
	}
	if wantsManifest {
		return manifestPath, manifest, nil
	}
	return databasePath, manifest, nil
}

func readBackupManifest(path string) (BackupManifest, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return BackupManifest{}, ErrNotFound
		}
		return BackupManifest{}, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 1<<20))
	if err != nil {
		return BackupManifest{}, err
	}
	var manifest BackupManifest
	if len(data) == 1<<20 || json.Unmarshal(data, &manifest) != nil {
		return BackupManifest{}, fmt.Errorf("%w: invalid backup manifest", ErrCorrupt)
	}
	return manifest, nil
}

func validateRegularFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ErrNotFound
		}
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%w: backup artifact is not a regular file", ErrInvalid)
	}
	return nil
}

func validateBackupDigest(path string, manifest BackupManifest) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	hash := sha256.New()
	written, err := io.Copy(hash, f)
	if err != nil {
		return err
	}
	if written != manifest.Bytes || !strings.EqualFold(hex.EncodeToString(hash.Sum(nil)), manifest.SHA256) {
		return fmt.Errorf("%w: backup artifact does not match its manifest", ErrCorrupt)
	}
	return nil
}

// ValidateOpenedBackupArtifact confirms that the opened file is the same file
// BackupArtifact validated. A file replacement cannot change what the API
// sends after validation.
func ValidateOpenedBackupArtifact(file *os.File, artifact string, manifest BackupManifest) error {
	if file == nil {
		return fmt.Errorf("%w: backup artifact is not open", ErrInvalid)
	}
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%w: backup artifact is not a regular file", ErrInvalid)
	}
	if strings.HasSuffix(artifact, ".manifest.json") {
		data, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
		if err != nil {
			return err
		}
		var opened BackupManifest
		if len(data) > 1<<20 || json.Unmarshal(data, &opened) != nil || opened != manifest {
			return fmt.Errorf("%w: opened manifest changed after validation", ErrCorrupt)
		}
	} else {
		hash := sha256.New()
		written, err := io.Copy(hash, file)
		if err != nil {
			return err
		}
		if written != manifest.Bytes || !strings.EqualFold(hex.EncodeToString(hash.Sum(nil)), manifest.SHA256) {
			return fmt.Errorf("%w: opened backup changed after validation", ErrCorrupt)
		}
	}
	_, err = file.Seek(0, io.SeekStart)
	return err
}

func (s *Store) RecordBackupDownload(ctx context.Context, manifest BackupManifest, artifact string, audit AuditContext) error {
	detail, err := json.Marshal(map[string]any{
		"artifact": artifact,
		"filename": manifest.Filename,
		"sha256":   manifest.SHA256,
		"bytes":    manifest.Bytes,
	})
	if err != nil {
		return err
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		actor, role, requestID := auditValues(audit)
		return appendEvent(ctx, tx, Event{Type: "backup.downloaded", OccurredAt: s.now(),
			RequestID: requestID, Actor: actor, Role: role, ResourceType: "backup",
			ResourceID: manifest.Filename, Payload: detail})
	})
}
