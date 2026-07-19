// Package store saves maintenance state in SQLite and applies lifecycle changes.
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaSQL string

const (
	applicationID   = 0x4c415a52 // LAZR
	currentSchema   = 1
	busyTimeoutMS   = 5000
	minimumSQLite   = "3.51.3"
	defaultKeepBack = 7
)

var (
	ErrNotFound             = errors.New("resource not found")
	ErrConflict             = errors.New("operation conflicts with current state")
	ErrVersionConflict      = errors.New("resource changed since it was read")
	ErrPreconditionRequired = errors.New("precondition required")
	ErrForbidden            = errors.New("operation forbidden")
	ErrInvalid              = errors.New("invalid resource")
	ErrCorrupt              = errors.New("database verification failed")
)

type ConflictError struct {
	Base            error
	Reason          string
	ResourceType    string
	ResourceID      string
	ExpectedVersion int64
	CurrentVersion  int64
	CurrentState    string
	RequestedState  string
	Holder          string
}

func (e *ConflictError) Error() string {
	if e == nil {
		return "conflict"
	}
	base := e.Base
	if base == nil {
		base = ErrConflict
	}
	parts := make([]string, 0, 4)
	if e.Reason != "" {
		parts = append(parts, e.Reason)
	}
	if e.ResourceType != "" && e.ResourceID != "" {
		parts = append(parts, e.ResourceType+" "+e.ResourceID)
	}
	if e.CurrentState != "" {
		parts = append(parts, "current state="+e.CurrentState)
	}
	if e.CurrentVersion > 0 {
		parts = append(parts, fmt.Sprintf("current version=%d", e.CurrentVersion))
	}
	if e.Holder != "" {
		parts = append(parts, "holder="+e.Holder)
	}
	if len(parts) == 0 {
		return base.Error()
	}
	return base.Error() + ": " + strings.Join(parts, ", ")
}

func (e *ConflictError) Unwrap() error {
	if e == nil || e.Base == nil {
		return ErrConflict
	}
	return e.Base
}

func conflictError(base error, reason, resourceType, resourceID string, expected, current int64, currentState, requested string) error {
	if base == nil {
		base = ErrConflict
	}
	return &ConflictError{Base: base, Reason: reason, ResourceType: resourceType, ResourceID: resourceID, ExpectedVersion: expected, CurrentVersion: current, CurrentState: currentState, RequestedState: requested}
}

type AuditContext struct {
	Actor     string
	Role      string
	RequestID string
}

type StateChange struct {
	State         string
	Justification string
	Detail        json.RawMessage
}

// VersionPrecondition contains the versions accepted by an If-Match request.
// The store checks it in the same transaction that writes the change.
type VersionPrecondition struct {
	Any      bool
	Versions []int64
}

func (p VersionPrecondition) match(version int64) bool {
	if p.Any {
		return true
	}
	for _, candidate := range p.Versions {
		if candidate == version {
			return true
		}
	}
	return false
}

func (p VersionPrecondition) require(version int64, resourceType, resourceID, state, requested string) error {
	if !p.Any && len(p.Versions) == 0 {
		return ErrPreconditionRequired
	}
	if !p.match(version) {
		return conflictError(ErrVersionConflict, resourceType+"_version_changed", resourceType, resourceID, 0, version, state, requested)
	}
	return nil
}

func VersionETag(version int64) string { return `"` + strconv.FormatInt(version, 10) + `"` }

type Store struct {
	db   *sql.DB
	path string
	now  func() time.Time
	mu   sync.Mutex
}

type Capture struct {
	MaintenanceID string          `json:"maintenance_id"`
	CapturedAt    time.Time       `json:"captured_at"`
	Payload       json.RawMessage `json:"payload"`
	SHA256        string          `json:"sha256"`
}

type CaptureResult struct {
	Capture Capture `json:"capture"`
	Created bool    `json:"created"`
}

type Maintenance struct {
	ID              string          `json:"id"`
	State           string          `json:"state"`
	ChangeTicket    string          `json:"change_ticket,omitempty"`
	WorkflowVersion string          `json:"workflow_version,omitempty"`
	Metadata        json.RawMessage `json:"metadata,omitempty"`
	Version         int64           `json:"version"`
	CreatedAt       time.Time       `json:"created_at"`
	UpdatedAt       time.Time       `json:"updated_at"`
}

type Target struct {
	ID            string          `json:"id"`
	MaintenanceID string          `json:"maintenance_id,omitempty"`
	LockKey       string          `json:"lock_key"`
	State         string          `json:"state"`
	OriginalState string          `json:"original_state"`
	Kind          string          `json:"kind,omitempty"`
	Host          string          `json:"host,omitempty"`
	ObservedAt    *time.Time      `json:"observed_at,omitempty"`
	Metadata      json.RawMessage `json:"metadata,omitempty"`
	Version       int64           `json:"version"`
	CreatedAt     time.Time       `json:"created_at"`
	UpdatedAt     time.Time       `json:"updated_at"`
}

type MaintenanceAggregate struct {
	Maintenance Maintenance `json:"maintenance"`
	Targets     []Target    `json:"targets"`
}

type TargetTransitionResult struct {
	Target             Target `json:"target"`
	MaintenanceVersion int64  `json:"maintenance_version"`
}

type MaintenanceListOptions struct {
	State           string
	MaintenanceID   string
	ChangeTicket    string
	WorkflowVersion string
	Cursor          string
	Limit           int
}

type MaintenancePage struct {
	Items      []Maintenance `json:"items"`
	NextCursor string        `json:"next_cursor,omitempty"`
}

type Event struct {
	ID               int64           `json:"id"`
	Type             string          `json:"type"`
	OccurredAt       time.Time       `json:"occurred_at"`
	RequestID        string          `json:"request_id"`
	Actor            string          `json:"actor"`
	Role             string          `json:"role"`
	ResourceType     string          `json:"resource_type"`
	ResourceID       string          `json:"resource_id"`
	MaintenanceID    string          `json:"maintenance_id,omitempty"`
	TargetID         string          `json:"target_id,omitempty"`
	FromState        string          `json:"from_state,omitempty"`
	ToState          string          `json:"to_state,omitempty"`
	FromVersion      int64           `json:"from_version,omitempty"`
	ToVersion        int64           `json:"to_version,omitempty"`
	AggregateVersion int64           `json:"aggregate_version,omitempty"`
	Payload          json.RawMessage `json:"payload"`
}

type EventListOptions struct {
	MaintenanceID string
	TargetID      string
	EventType     string
	ResourceType  string
	ResourceID    string
	Actor         string
	Role          string
	RequestID     string
	Cursor        string
	Limit         int
}

type EventPage struct {
	Items      []Event `json:"items"`
	NextCursor string  `json:"next_cursor,omitempty"`
}

type BackupManifest struct {
	Filename       string    `json:"filename"`
	CreatedAt      time.Time `json:"created_at"`
	Bytes          int64     `json:"bytes"`
	SHA256         string    `json:"sha256"`
	SQLiteVersion  string    `json:"sqlite_version"`
	IntegrityCheck string    `json:"integrity_check"`
	ForeignKeys    string    `json:"foreign_key_check"`
	Application    string    `json:"application_check"`
	AuditRecorded  bool      `json:"audit_recorded"`
}

func Open(ctx context.Context, path string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("database path is required")
	}
	existed, err := inspectDatabasePath(path)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	s := &Store{db: db, path: path, now: func() time.Time { return time.Now().UTC() }}
	fail := func(err error) (*Store, error) { _ = db.Close(); return nil, err }
	for _, pragma := range []string{
		"PRAGMA busy_timeout=" + strconv.Itoa(busyTimeoutMS),
		"PRAGMA foreign_keys=ON",
		"PRAGMA synchronous=FULL",
		"PRAGMA journal_mode=WAL",
	} {
		if _, err := db.ExecContext(ctx, pragma); err != nil {
			return fail(fmt.Errorf("%s: %w", pragma, err))
		}
	}
	var sqliteVersion string
	if err := db.QueryRowContext(ctx, "SELECT sqlite_version()").Scan(&sqliteVersion); err != nil {
		return fail(err)
	}
	if compareVersions(sqliteVersion, minimumSQLite) < 0 {
		return fail(fmt.Errorf("sqlite runtime %s is older than required %s", sqliteVersion, minimumSQLite))
	}
	if existed {
		if err := startupQuickCheck(ctx, db); err != nil {
			return fail(err)
		}
		if err := requireSchemaIdentity(ctx, db); err != nil {
			return fail(err)
		}
	} else if err := s.initializeSchema(ctx); err != nil {
		return fail(fmt.Errorf("initialize schema: %w", err))
	}
	if !existed && !isMemoryPath(path) {
		if err := os.Chmod(path, 0o660); err != nil {
			return fail(err)
		}
	}
	if err := verifyRequiredSchema(ctx, db); err != nil {
		return fail(fmt.Errorf("verify startup schema: %w", err))
	}
	return s, nil
}

func inspectDatabasePath(path string) (bool, error) {
	if isMemoryPath(path) {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o770); err != nil {
		return false, fmt.Errorf("create database directory: %w", err)
	}
	info, err := os.Stat(path)
	if err == nil {
		if !info.Mode().IsRegular() || info.Size() == 0 {
			return false, fmt.Errorf("refusing to initialize invalid existing database file")
		}
		return true, nil
	}
	if !os.IsNotExist(err) {
		return false, err
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, sidecarErr := os.Stat(path + suffix); sidecarErr == nil {
			return false, fmt.Errorf("refusing to initialize database while orphan sidecar %s exists", filepath.Base(path+suffix))
		} else if !os.IsNotExist(sidecarErr) {
			return false, sidecarErr
		}
	}
	return false, nil
}

func (s *Store) initializeSchema(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, schemaSQL); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA application_id=%d", applicationID)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version=%d", currentSchema)); err != nil {
		return err
	}
	return tx.Commit()
}

func requireSchemaIdentity(ctx context.Context, db *sql.DB) error {
	var appID, version int
	if err := db.QueryRowContext(ctx, "PRAGMA application_id").Scan(&appID); err != nil {
		return err
	}
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if appID != applicationID || version != currentSchema {
		return fmt.Errorf("%w: unsupported database identity or schema", ErrCorrupt)
	}
	return nil
}

func (s *Store) DB() *sql.DB  { return s.db }
func (s *Store) Path() string { return s.path }

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	_, _ = s.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	return s.db.Close()
}

func (s *Store) write(ctx context.Context, fn func(*sql.Tx) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func isMemoryPath(path string) bool {
	return path == ":memory:" || strings.HasPrefix(path, "file::memory:") || strings.HasPrefix(path, "file:memdb")
}

func compareVersions(a, b string) int {
	parse := func(v string) [3]int {
		var out [3]int
		for i, part := range strings.Split(v, ".") {
			if i == len(out) {
				break
			}
			out[i], _ = strconv.Atoi(part)
		}
		return out
	}
	x, y := parse(a), parse(b)
	for i := range x {
		if x[i] < y[i] {
			return -1
		}
		if x[i] > y[i] {
			return 1
		}
	}
	return 0
}

func validID(v string) bool {
	if len(v) < 1 || len(v) > 200 {
		return false
	}
	for i, r := range v {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || (i > 0 && strings.ContainsRune("._:@+-", r)) {
			continue
		}
		return false
	}
	return true
}

func normalizeJSON(raw json.RawMessage) ([]byte, error) {
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	if !json.Valid(raw) {
		return nil, fmt.Errorf("%w: payload must be valid JSON", ErrInvalid)
	}
	return append([]byte(nil), raw...), nil
}

func canonicalJSON(raw []byte) ([]byte, error) {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return nil, fmt.Errorf("%w: invalid JSON", ErrInvalid)
	}
	var extra any
	if err := dec.Decode(&extra); err == nil {
		return nil, fmt.Errorf("%w: multiple JSON values", ErrInvalid)
	}
	return json.Marshal(value)
}

func auditValues(a AuditContext) (string, string, string) {
	if a.Actor == "" {
		a.Actor = "system"
	}
	if a.Role == "" {
		a.Role = "system"
	}
	if a.RequestID == "" {
		a.RequestID = "system"
	}
	return a.Actor, a.Role, a.RequestID
}

func pageLimit(value int) int {
	if value <= 0 {
		return 100
	}
	if value > 200 {
		return 200
	}
	return value
}

func encodeCursor(value string) string { return base64.RawURLEncoding.EncodeToString([]byte(value)) }

func decodeCursor(value string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return "", nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(decoded) == 0 {
		return "", fmt.Errorf("%w: invalid cursor", ErrInvalid)
	}
	return string(decoded), nil
}

func isAdmin(role string) bool { return role == "admin" }

func isUnique(err error) bool {
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "unique constraint") || strings.Contains(text, "constraint failed") && strings.Contains(text, "primary key")
}
