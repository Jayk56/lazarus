// Package api provides the Lazarus HTTP API, health checks, and metrics.
package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jayk56/lazarus/internal/auth"
	"github.com/jayk56/lazarus/internal/store"
)

type Config struct {
	Store        *store.Store
	Auth         *auth.Authenticator
	Version      string
	BackupDir    string
	BackupKeep   int
	BackupMinAge time.Duration
	Logger       *slog.Logger
}

type Server struct {
	store        *store.Store
	auth         *auth.Authenticator
	version      string
	backupDir    string
	backupKeep   int
	backupMinAge time.Duration
	logger       *slog.Logger
	requests     atomic.Uint64
	errors       atomic.Uint64
	ready        atomic.Bool
	routes       http.Handler
}

type principalKey struct{}
type requestIDKey struct{}

func New(cfg Config) *Server {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	s := &Server{
		store: cfg.Store, auth: cfg.Auth, version: cfg.Version,
		backupDir: cfg.BackupDir, backupKeep: cfg.BackupKeep,
		backupMinAge: cfg.BackupMinAge, logger: logger,
	}
	if cfg.Store != nil && cfg.Auth != nil {
		s.ready.Store(true)
	}
	s.routes = s.buildRoutes()
	return s
}

func (s *Server) SetReady(value bool)   { s.ready.Store(value) }
func (s *Server) Handler() http.Handler { return s }

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	s.requests.Add(1)
	rid := requestID(r)
	r = r.WithContext(context.WithValue(r.Context(), requestIDKey{}, rid))
	w.Header().Set("X-Request-ID", rid)
	recorded := &responseWriter{ResponseWriter: w, status: http.StatusOK}
	s.serve(recorded, r)
	if recorded.status >= 400 {
		s.errors.Add(1)
	}
	s.logger.Info("http_request", "request_id", rid, "method", r.Method, "path", r.URL.Path, "status", recorded.status, "duration_ms", time.Since(started).Milliseconds())
}

type responseWriter struct {
	http.ResponseWriter
	status int
}

func (w *responseWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

type maintenanceRequest struct {
	MaintenanceID   string          `json:"maintenance_id"`
	ChangeTicket    string          `json:"change_ticket"`
	WorkflowVersion string          `json:"workflow_version"`
	Metadata        json.RawMessage `json:"metadata"`
}

type stateChangeRequest struct {
	State         string          `json:"state"`
	Justification string          `json:"justification"`
	Detail        json.RawMessage `json:"detail"`
}

func (r stateChangeRequest) storeValue() store.StateChange {
	return store.StateChange{State: r.State, Justification: r.Justification, Detail: r.Detail}
}

func queryPageLimit(r *http.Request) (int, error) {
	value := strings.TrimSpace(r.URL.Query().Get("limit"))
	if value == "" {
		return 100, nil
	}
	limit, err := strconv.Atoi(value)
	if err != nil || limit < 1 || limit > 200 {
		return 0, fmt.Errorf("%w: limit must be between 1 and 200", store.ErrInvalid)
	}
	return limit, nil
}

func rejectUnknownQuery(query url.Values, allowed ...string) error {
	valid := make(map[string]bool, len(allowed))
	for _, name := range allowed {
		valid[name] = true
	}
	for name := range query {
		if !valid[name] {
			return fmt.Errorf("%w: unsupported query parameter %s", store.ErrInvalid, name)
		}
	}
	return nil
}

func maintenanceListOptions(r *http.Request) (store.MaintenanceListOptions, error) {
	query := r.URL.Query()
	if err := rejectUnknownQuery(query, "state", "maintenance_id", "change_ticket", "workflow_version", "cursor", "limit"); err != nil {
		return store.MaintenanceListOptions{}, err
	}
	limit, err := queryPageLimit(r)
	if err != nil {
		return store.MaintenanceListOptions{}, err
	}
	return store.MaintenanceListOptions{
		State: strings.TrimSpace(query.Get("state")), MaintenanceID: strings.TrimSpace(query.Get("maintenance_id")),
		ChangeTicket: strings.TrimSpace(query.Get("change_ticket")), WorkflowVersion: strings.TrimSpace(query.Get("workflow_version")),
		Cursor: strings.TrimSpace(query.Get("cursor")), Limit: limit,
	}, nil
}

func eventListOptions(r *http.Request) (store.EventListOptions, error) {
	query := r.URL.Query()
	if err := rejectUnknownQuery(query, "maintenance_id", "target_id", "event_type", "resource_type", "resource_id", "actor", "role", "request_id", "cursor", "limit"); err != nil {
		return store.EventListOptions{}, err
	}
	limit, err := queryPageLimit(r)
	if err != nil {
		return store.EventListOptions{}, err
	}
	opts := store.EventListOptions{
		MaintenanceID: strings.TrimSpace(query.Get("maintenance_id")), TargetID: strings.TrimSpace(query.Get("target_id")),
		EventType: strings.TrimSpace(query.Get("event_type")), ResourceType: strings.TrimSpace(query.Get("resource_type")),
		ResourceID: strings.TrimSpace(query.Get("resource_id")), Actor: strings.TrimSpace(query.Get("actor")),
		Role: strings.TrimSpace(query.Get("role")), RequestID: strings.TrimSpace(query.Get("request_id")),
		Cursor: strings.TrimSpace(query.Get("cursor")), Limit: limit,
	}
	if opts.TargetID != "" && opts.MaintenanceID == "" {
		return store.EventListOptions{}, fmt.Errorf("%w: target_id requires maintenance_id", store.ErrInvalid)
	}
	return opts, nil
}

func decodeJSON(r *http.Request, dst any) error {
	data, err := readBody(r, 2<<20)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return fmt.Errorf("multiple JSON values")
	}
	return nil
}

func decodeJSONObject(r *http.Request, limit int64) (json.RawMessage, error) {
	data, err := readBody(r, limit)
	if err != nil || !isJSONObject(data) {
		return nil, fmt.Errorf("invalid JSON object")
	}
	return json.RawMessage(data), nil
}

func readBody(r *http.Request, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil || len(data) == 0 || int64(len(data)) > limit {
		return nil, fmt.Errorf("request body exceeds limit or is empty")
	}
	return data, nil
}

func isJSONObject(raw []byte) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) >= 2 && trimmed[0] == '{' && trimmed[len(trimmed)-1] == '}' && json.Valid(trimmed)
}

// versionPrecondition reads If-Match. Lazarus versions are positive numbers in
// strong ETags, such as "3". Weak tags and other valid tags do not match.
func versionPrecondition(r *http.Request) (store.VersionPrecondition, string) {
	raw := strings.TrimSpace(strings.Join(r.Header.Values("If-Match"), ","))
	if raw == "" {
		return store.VersionPrecondition{}, "if_match_required"
	}
	if raw == "*" {
		return store.VersionPrecondition{Any: true}, ""
	}
	versions := make([]int64, 0, 2)
	for offset := 0; offset < len(raw); {
		for offset < len(raw) && (raw[offset] == ' ' || raw[offset] == '\t') {
			offset++
		}
		weak := strings.HasPrefix(raw[offset:], "W/")
		if weak {
			offset += 2
		}
		if offset >= len(raw) || raw[offset] != '"' {
			return store.VersionPrecondition{}, "invalid_if_match"
		}
		offset++
		start := offset
		for offset < len(raw) && raw[offset] != '"' {
			char := raw[offset]
			if !(char == 0x21 || char >= 0x23 && char <= 0x7e || char >= 0x80) {
				return store.VersionPrecondition{}, "invalid_if_match"
			}
			offset++
		}
		if offset >= len(raw) {
			return store.VersionPrecondition{}, "invalid_if_match"
		}
		opaque := raw[start:offset]
		offset++
		if !weak {
			if version, err := strconv.ParseInt(opaque, 10, 64); err == nil && version > 0 && strconv.FormatInt(version, 10) == opaque {
				versions = append(versions, version)
			}
		}
		for offset < len(raw) && (raw[offset] == ' ' || raw[offset] == '\t') {
			offset++
		}
		if offset == len(raw) {
			break
		}
		if raw[offset] != ',' {
			return store.VersionPrecondition{}, "invalid_if_match"
		}
		offset++
		if offset == len(raw) {
			return store.VersionPrecondition{}, "invalid_if_match"
		}
	}
	if len(versions) == 0 {
		versions = append(versions, 0) // A syntactically valid condition that cannot match.
	}
	return store.VersionPrecondition{Versions: versions}, ""
}

func (s *Server) writeStoreError(w http.ResponseWriter, err error) {
	code, status := "internal_error", http.StatusInternalServerError
	switch {
	case errors.Is(err, store.ErrNotFound):
		code, status = "not_found", http.StatusNotFound
	case errors.Is(err, store.ErrForbidden):
		code, status = "forbidden", http.StatusForbidden
	case errors.Is(err, store.ErrVersionConflict):
		code, status = "version_conflict", http.StatusPreconditionFailed
	case errors.Is(err, store.ErrPreconditionRequired):
		code, status = "if_match_required", http.StatusPreconditionRequired
	case errors.Is(err, store.ErrConflict):
		code, status = "conflict", http.StatusConflict
	case errors.Is(err, store.ErrInvalid):
		code, status = "invalid_resource", http.StatusBadRequest
	}
	body := map[string]any{"error": code}
	if code == "invalid_resource" || code == "conflict" || code == "version_conflict" {
		body["message"] = err.Error()
	}
	var details *store.ConflictError
	if errors.As(err, &details) && details != nil {
		if details.Reason != "" {
			body["reason"] = details.Reason
		}
		if details.ResourceType != "" {
			body["resource_type"] = details.ResourceType
		}
		if details.ResourceID != "" {
			body["resource_id"] = details.ResourceID
		}
		if details.Holder != "" {
			body["holder"] = details.Holder
		}
		if details.ExpectedVersion > 0 {
			body["expected_version"] = details.ExpectedVersion
		}
		if details.CurrentVersion > 0 {
			body["current_version"] = details.CurrentVersion
		}
		if details.CurrentState != "" {
			body["current_state"] = details.CurrentState
		}
		if details.RequestedState != "" {
			body["requested_state"] = details.RequestedState
		}
	}
	s.writeJSON(w, status, body)
}

func (s *Server) writeError(w http.ResponseWriter, status int, code string) {
	s.writeJSON(w, status, map[string]any{"error": code})
}

func (s *Server) writeIfMatchError(w http.ResponseWriter, code string) {
	status, detail := http.StatusBadRequest, "If-Match must use RFC entity-tag syntax or *"
	if code == "if_match_required" {
		status, detail = http.StatusPreconditionRequired, "send If-Match with a strong ETag, a tag list, or *"
	}
	s.writeJSON(w, status, map[string]any{"error": code, "detail": detail})
}

func (s *Server) writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func (s *Server) metrics(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	fmt.Fprintf(w, "# TYPE lazarus_http_requests_total counter\nlazarus_http_requests_total %d\n", s.requests.Load())
	fmt.Fprintf(w, "# TYPE lazarus_http_errors_total counter\nlazarus_http_errors_total %d\n", s.errors.Load())
	ready := 0
	if s.ready.Load() {
		ready = 1
	}
	fmt.Fprintf(w, "# TYPE lazarus_ready gauge\nlazarus_ready %d\n", ready)
	if s.store != nil {
		if info, err := os.Stat(s.store.Path()); err == nil {
			fmt.Fprintf(w, "# TYPE lazarus_database_bytes gauge\nlazarus_database_bytes %d\n", info.Size())
		}
		var events int64
		if err := s.store.DB().QueryRowContext(context.Background(), `SELECT count(*) FROM journal_events`).Scan(&events); err == nil {
			fmt.Fprintf(w, "# TYPE lazarus_journal_events gauge\nlazarus_journal_events %d\n", events)
		}
	}
	fmt.Fprintf(w, "lazarus_info{version=%q} 1\n", s.version)
}

func requestID(r *http.Request) string {
	if value := strings.TrimSpace(r.Header.Get("X-Request-ID")); value != "" && len(value) <= 128 && !strings.ContainsAny(value, "\r\n") {
		return value
	}
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 10)
	}
	return hex.EncodeToString(value[:])
}

func requestIDFrom(ctx context.Context) string {
	if value, ok := ctx.Value(requestIDKey{}).(string); ok {
		return value
	}
	return "unknown"
}

func auditContext(r *http.Request) store.AuditContext {
	principal, _ := r.Context().Value(principalKey{}).(auth.Principal)
	return store.AuditContext{Actor: principal.Name, Role: string(principal.Role), RequestID: requestIDFrom(r.Context())}
}
