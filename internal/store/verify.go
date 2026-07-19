package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type VerifyResult struct {
	SQLiteVersion   string `json:"sqlite_version"`
	IntegrityCheck  string `json:"integrity_check"`
	ForeignKeys     string `json:"foreign_key_check"`
	ApplicationData string `json:"application_check"`
}

func VerifyPath(ctx context.Context, path string) (VerifyResult, error) {
	info, err := os.Stat(path)
	if err != nil {
		return VerifyResult{}, fmt.Errorf("stat database: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return VerifyResult{}, fmt.Errorf("database must be a non-empty regular file")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return VerifyResult{}, err
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: abs}).String()+"?mode=ro")
	if err != nil {
		return VerifyResult{}, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, "PRAGMA query_only=ON"); err != nil {
		return VerifyResult{}, err
	}
	var out VerifyResult
	if err := db.QueryRowContext(ctx, "SELECT sqlite_version()").Scan(&out.SQLiteVersion); err != nil {
		return out, err
	}
	if compareVersions(out.SQLiteVersion, minimumSQLite) < 0 {
		return out, fmt.Errorf("sqlite runtime %s is older than required %s", out.SQLiteVersion, minimumSQLite)
	}
	if err := db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&out.IntegrityCheck); err != nil {
		return out, err
	}
	if !strings.EqualFold(out.IntegrityCheck, "ok") {
		return out, fmt.Errorf("%w: %s", ErrCorrupt, out.IntegrityCheck)
	}
	out.ForeignKeys = "ok"
	rows, err := db.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return out, err
	}
	if rows.Next() {
		out.ForeignKeys = "failed"
	}
	if err := rows.Close(); err != nil {
		return out, err
	}
	if out.ForeignKeys != "ok" {
		return out, fmt.Errorf("%w: foreign key violation", ErrCorrupt)
	}
	if err := verifyApplicationData(ctx, db); err != nil {
		return out, err
	}
	out.ApplicationData = "ok"
	return out, nil
}

func verifyRequiredSchema(ctx context.Context, db *sql.DB) error {
	if err := requireSchemaIdentity(ctx, db); err != nil {
		return err
	}
	var tables string
	if err := db.QueryRowContext(ctx, `SELECT group_concat(name, ',') FROM (SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name)`).Scan(&tables); err != nil {
		return err
	}
	if tables != "journal_events,maintenance,maintenance_captures,targets" {
		return fmt.Errorf("%w: unexpected application tables", ErrCorrupt)
	}
	queries := []string{
		`SELECT id,state,change_ticket,workflow_version,metadata_json,version,created_at,updated_at FROM maintenance LIMIT 0`,
		`SELECT maintenance_id,captured_at,payload_json,sha256 FROM maintenance_captures LIMIT 0`,
		`SELECT maintenance_id,id,lock_key,state,original_state,kind,host,observed_at,metadata_json,version,created_at,updated_at FROM targets LIMIT 0`,
		`SELECT journal_id,event_type,occurred_at,request_id,actor,role,resource_type,resource_id,maintenance_id,target_id,from_state,to_state,from_version,to_version,aggregate_version,payload_json FROM journal_events LIMIT 0`,
	}
	for _, query := range queries {
		rows, err := db.QueryContext(ctx, query)
		if err != nil {
			return fmt.Errorf("%w: required schema is missing: %v", ErrCorrupt, err)
		}
		_ = rows.Close()
	}
	for _, trigger := range []string{
		"maintenance_identity_immutable", "maintenance_captures_immutable_update", "maintenance_captures_immutable_delete",
		"targets_identity_immutable", "targets_immutable_delete", "targets_lock_exclusive", "journal_events_append_only_update", "journal_events_append_only_delete",
	} {
		var count int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='trigger' AND name=?`, trigger).Scan(&count); err != nil {
			return err
		}
		if count != 1 {
			return fmt.Errorf("%w: required trigger %s is missing", ErrCorrupt, trigger)
		}
	}
	for _, index := range []string{
		"idx_maintenance_list", "idx_targets_lock", "idx_events_scope", "idx_events_type", "idx_events_resource",
		"idx_events_aggregate_version", "idx_events_observation_request",
	} {
		var count int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='index' AND name=?`, index).Scan(&count); err != nil {
			return err
		}
		if count != 1 {
			return fmt.Errorf("%w: required index %s is missing", ErrCorrupt, index)
		}
	}
	return nil
}

type replayTarget struct {
	state    string
	version  int64
	original string
}

type replayMaintenance struct {
	state              string
	revision           int64
	captured           bool
	targets            map[string]*replayTarget
	physicalTransition bool
	created            *Maintenance
}

func verifyApplicationData(ctx context.Context, db *sql.DB) error {
	if err := verifyRequiredSchema(ctx, db); err != nil {
		return err
	}
	if err := verifyLockExclusivity(ctx, db); err != nil {
		return err
	}
	rows, err := db.QueryContext(ctx, `SELECT id FROM maintenance ORDER BY id`)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, id := range ids {
		if err := verifyMaintenanceReplay(ctx, db, id); err != nil {
			return err
		}
	}
	return verifyGlobalEvents(ctx, db)
}

func verifyLockExclusivity(ctx context.Context, db *sql.DB) error {
	var overlaps int
	err := db.QueryRowContext(ctx, `SELECT count(*) FROM (SELECT t.lock_key FROM targets t JOIN maintenance m ON m.id=t.maintenance_id WHERE m.state NOT IN ('completed','cancelled') GROUP BY t.lock_key HAVING count(DISTINCT t.maintenance_id)>1)`).Scan(&overlaps)
	if err != nil {
		return err
	}
	if overlaps != 0 {
		return fmt.Errorf("%w: the same lock_key is active in more than one maintenance", ErrCorrupt)
	}
	return nil
}

func loadCaptureForVerify(ctx context.Context, db *sql.DB, maintenanceID string) (Capture, []captureTarget, bool, error) {
	var capture Capture
	var at, payload string
	err := db.QueryRowContext(ctx, `SELECT maintenance_id,captured_at,payload_json,sha256 FROM maintenance_captures WHERE maintenance_id=?`, maintenanceID).Scan(&capture.MaintenanceID, &at, &payload, &capture.SHA256)
	if err == sql.ErrNoRows {
		return Capture{}, nil, false, nil
	}
	if err != nil {
		return Capture{}, nil, false, err
	}
	capture.CapturedAt, _ = time.Parse(time.RFC3339Nano, at)
	capture.Payload = json.RawMessage(payload)
	canonical, err := canonicalJSON([]byte(payload))
	if err != nil || string(canonical) != payload {
		return Capture{}, nil, false, fmt.Errorf("%w: capture %s payload is not stored as normalized JSON", ErrCorrupt, maintenanceID)
	}
	sum := sha256.Sum256([]byte(payload))
	if !strings.EqualFold(capture.SHA256, hex.EncodeToString(sum[:])) {
		return Capture{}, nil, false, fmt.Errorf("%w: capture %s hash mismatch", ErrCorrupt, maintenanceID)
	}
	targets, err := parseCaptureTargets(capture.Payload)
	if err != nil {
		return Capture{}, nil, false, fmt.Errorf("%w: invalid capture %s", ErrCorrupt, maintenanceID)
	}
	return capture, targets, true, nil
}

func verifyMaintenanceReplay(ctx context.Context, db *sql.DB, maintenanceID string) error {
	actual, err := scanMaintenance(db.QueryRowContext(ctx, `SELECT `+maintenanceColumns+` FROM maintenance WHERE id=?`, maintenanceID))
	if err != nil {
		return err
	}
	capture, capturedTargets, hasCapture, err := loadCaptureForVerify(ctx, db, maintenanceID)
	if err != nil {
		return err
	}
	replay := replayMaintenance{targets: make(map[string]*replayTarget)}
	captureHash := ""
	if hasCapture {
		captureHash = capture.SHA256
	}
	rows, err := db.QueryContext(ctx, `SELECT journal_id,event_type,occurred_at,request_id,actor,role,resource_type,resource_id,maintenance_id,target_id,from_state,to_state,from_version,to_version,aggregate_version,payload_json FROM journal_events WHERE maintenance_id=? ORDER BY journal_id`, maintenanceID)
	if err != nil {
		return err
	}
	defer rows.Close()
	eventCount := 0
	for rows.Next() {
		event, err := scanEvent(rows)
		if err != nil {
			return err
		}
		if err := validateEventShape(event); err != nil {
			return fmt.Errorf("%w: event %d: %v", ErrCorrupt, event.ID, err)
		}
		eventCount++
		if err := replay.apply(event, capturedTargets, captureHash, hasCapture); err != nil {
			return fmt.Errorf("%w: event %d: %v", ErrCorrupt, event.ID, err)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if eventCount == 0 || replay.state != actual.State || replay.revision != actual.Version || replay.captured != hasCapture || replay.created == nil || replay.created.ID != actual.ID || replay.created.ChangeTicket != actual.ChangeTicket || replay.created.WorkflowVersion != actual.WorkflowVersion || string(replay.created.Metadata) != string(actual.Metadata) || !replay.created.CreatedAt.Equal(actual.CreatedAt) {
		return fmt.Errorf("%w: maintenance %s state does not match its events", ErrCorrupt, maintenanceID)
	}
	actualTargets, err := listTargets(ctx, db, maintenanceID)
	if err != nil {
		return err
	}
	if len(actualTargets) != len(capturedTargets) || len(actualTargets) != len(replay.targets) {
		return fmt.Errorf("%w: maintenance %s target count does not match its capture and events", ErrCorrupt, maintenanceID)
	}
	inputs := make(map[string]captureTarget, len(capturedTargets))
	for _, target := range capturedTargets {
		inputs[target.TargetID] = target
	}
	for _, target := range actualTargets {
		input, ok := inputs[target.ID]
		replayed, replayOK := replay.targets[target.ID]
		if !ok || !replayOK || target.LockKey != input.LockKey || target.OriginalState != input.OriginalState || target.Kind != input.Kind || target.Host != input.Host || !sameTime(target.ObservedAt, input.ObservedAt) || string(target.Metadata) != string(input.Metadata) || target.State != replayed.state || target.Version != replayed.version {
			return fmt.Errorf("%w: target %s:%s state does not match its capture and events", ErrCorrupt, maintenanceID, target.ID)
		}
	}
	return nil
}

func sameTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

func scanEvent(row rowScanner) (Event, error) {
	var event Event
	var at, payload string
	var maintenanceID, targetID, fromState, toState sql.NullString
	var fromVersion, toVersion, aggregateVersion sql.NullInt64
	err := row.Scan(&event.ID, &event.Type, &at, &event.RequestID, &event.Actor, &event.Role, &event.ResourceType, &event.ResourceID, &maintenanceID, &targetID, &fromState, &toState, &fromVersion, &toVersion, &aggregateVersion, &payload)
	if err != nil {
		return Event{}, err
	}
	event.OccurredAt, _ = time.Parse(time.RFC3339Nano, at)
	event.MaintenanceID, event.TargetID = maintenanceID.String, targetID.String
	event.FromState, event.ToState = fromState.String, toState.String
	event.FromVersion, event.ToVersion = fromVersion.Int64, toVersion.Int64
	event.AggregateVersion, event.Payload = aggregateVersion.Int64, json.RawMessage(payload)
	return event, nil
}

func validateEventShape(event Event) error {
	if event.ID < 1 || event.Type == "" || event.OccurredAt.IsZero() || event.RequestID == "" || event.Actor == "" || event.Role == "" || event.ResourceType == "" || event.ResourceID == "" || !json.Valid(event.Payload) {
		return fmt.Errorf("event identity is incomplete")
	}
	switch {
	case strings.HasPrefix(event.Type, "maintenance."):
		if event.ResourceType != "maintenance" || event.MaintenanceID == "" || event.ResourceID != event.MaintenanceID || event.TargetID != "" {
			return fmt.Errorf("maintenance event identifies the wrong resource")
		}
	case strings.HasPrefix(event.Type, "target."):
		if event.ResourceType != "target" || event.MaintenanceID == "" || event.TargetID == "" || event.ResourceID != event.TargetID {
			return fmt.Errorf("target event identifies the wrong resource")
		}
	case strings.HasPrefix(event.Type, "backup."):
		if event.ResourceType != "backup" || event.MaintenanceID != "" || event.TargetID != "" || event.FromState != "" || event.ToState != "" || event.FromVersion != 0 || event.ToVersion != 0 || event.AggregateVersion != 0 {
			return fmt.Errorf("backup event identifies the wrong resource")
		}
	default:
		return fmt.Errorf("unknown event type %s", event.Type)
	}
	return nil
}

func verifyGlobalEvents(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, `SELECT journal_id,event_type,occurred_at,request_id,actor,role,resource_type,resource_id,maintenance_id,target_id,from_state,to_state,from_version,to_version,aggregate_version,payload_json FROM journal_events WHERE maintenance_id IS NULL ORDER BY journal_id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		event, err := scanEvent(rows)
		if err != nil {
			return err
		}
		if err := validateEventShape(event); err != nil {
			return fmt.Errorf("%w: event %d: %v", ErrCorrupt, event.ID, err)
		}
		switch event.Type {
		case "backup.created":
			var manifest BackupManifest
			if json.Unmarshal(event.Payload, &manifest) != nil || manifest.Filename != event.ResourceID || manifest.SHA256 == "" || manifest.Bytes < 1 || !manifest.AuditRecorded {
				return fmt.Errorf("%w: invalid backup.created event %d", ErrCorrupt, event.ID)
			}
		case "backup.downloaded":
			var detail struct {
				Filename string `json:"filename"`
				SHA256   string `json:"sha256"`
				Bytes    int64  `json:"bytes"`
				Artifact string `json:"artifact"`
			}
			if json.Unmarshal(event.Payload, &detail) != nil || detail.Filename != event.ResourceID || detail.SHA256 == "" || detail.Bytes < 1 || detail.Artifact == "" {
				return fmt.Errorf("%w: invalid backup.downloaded event %d", ErrCorrupt, event.ID)
			}
		default:
			return fmt.Errorf("%w: unknown global event %s", ErrCorrupt, event.Type)
		}
	}
	return rows.Err()
}

func (r *replayMaintenance) apply(event Event, capturedTargets []captureTarget, captureHash string, hasCapture bool) error {
	switch event.Type {
	case "maintenance.created":
		if r.revision != 0 || event.ResourceType != "maintenance" || event.TargetID != "" || event.AggregateVersion != 1 || event.FromState != "" || event.ToState != "" || event.FromVersion != 0 || event.ToVersion != 0 {
			return fmt.Errorf("invalid creation event")
		}
		var created Maintenance
		if json.Unmarshal(event.Payload, &created) != nil || created.ID != event.MaintenanceID || created.State != "new" || created.Version != 1 || !created.CreatedAt.Equal(event.OccurredAt) {
			return fmt.Errorf("invalid creation payload")
		}
		r.state, r.revision = "new", 1
		r.created = &created
	case "maintenance.captured":
		if !hasCapture || r.captured || event.FromState != r.state || event.FromVersion != r.revision || event.ToState != "captured" || event.ToVersion != r.revision+1 || event.AggregateVersion != event.ToVersion {
			return fmt.Errorf("invalid capture event")
		}
		var detail struct {
			SHA256  string `json:"sha256"`
			Targets int    `json:"targets"`
		}
		if json.Unmarshal(event.Payload, &detail) != nil || detail.SHA256 != captureHash || detail.Targets != len(capturedTargets) {
			return fmt.Errorf("capture event does not match the saved capture")
		}
		for _, input := range capturedTargets {
			state := "captured"
			if input.OriginalState == "stopped" {
				state = "stopped"
			}
			r.targets[input.TargetID] = &replayTarget{state: state, version: 1, original: input.OriginalState}
		}
		r.state, r.revision, r.captured = "captured", event.ToVersion, true
	case "maintenance.transitioned", "maintenance.reopened", "maintenance.completed":
		if r.revision == 0 || event.ResourceType != "maintenance" || event.TargetID != "" || event.FromState != r.state || event.FromVersion != r.revision || event.ToVersion != r.revision+1 || event.AggregateVersion != event.ToVersion || !ValidMaintenanceTransition(r.state, event.ToState) {
			return fmt.Errorf("invalid maintenance transition")
		}
		if event.Type == "maintenance.reopened" {
			if r.state != "failed" || !isAdmin(event.Role) || validEventJustification(event.Payload) != nil {
				return fmt.Errorf("invalid reopen event")
			}
			if (!r.captured && event.ToState != "new") || (r.captured && (event.ToState == "new" || event.ToState == "completed" || event.ToState == "cancelled")) {
				return fmt.Errorf("invalid reopen destination")
			}
		} else if r.state == "failed" {
			return fmt.Errorf("failed maintenance was not explicitly reopened")
		}
		if validTransitionPayload(event.Payload) != nil {
			return fmt.Errorf("invalid transition payload")
		}
		if event.ToState == "captured" && event.Type != "maintenance.reopened" {
			return fmt.Errorf("capture transition used ordinary event")
		}
		if event.ToState == "cancelled" && r.captured && r.physicalTransition {
			return fmt.Errorf("cancel released an exercised lock")
		}
		if event.ToState == "stopped" {
			for _, target := range r.targets {
				if target.original == "running" || target.original == "stopped" {
					if target.state != "stopped" && target.state != "skipped" {
						return fmt.Errorf("maintenance reached stopped while a target was unresolved")
					}
				} else if target.state != "skipped" {
					return fmt.Errorf("maintenance reached stopped without valid capture data")
				}
			}
		}
		if event.ToState == "completed" {
			if event.Type != "maintenance.completed" || len(r.targets) == 0 {
				return fmt.Errorf("invalid completion event")
			}
			for _, target := range r.targets {
				if !targetCompletionAllowed(target.original, target.state) {
					return fmt.Errorf("maintenance completed before every target was restored or skipped")
				}
			}
		} else if event.Type == "maintenance.completed" {
			return fmt.Errorf("misclassified completion event")
		}
		if r.captured {
			for _, target := range r.targets {
				target.version++
			}
		}
		r.state, r.revision = event.ToState, event.ToVersion
	case "target.transitioned", "target.skipped":
		if r.revision == 0 || IsMaintenanceTerminal(r.state) || event.ResourceType != "target" || event.TargetID == "" || event.AggregateVersion != r.revision+1 {
			return fmt.Errorf("target event identifies the wrong resource")
		}
		target := r.targets[event.TargetID]
		if target == nil || event.FromState != target.state || event.FromVersion != target.version || event.ToVersion != target.version+1 || !ValidTargetTransition(target.state, event.ToState) {
			return fmt.Errorf("invalid target transition")
		}
		if event.ToState == "skipped" {
			if event.Type != "target.skipped" || !isAdmin(event.Role) || validEventJustification(event.Payload) != nil {
				return fmt.Errorf("invalid skip event")
			}
		} else {
			if event.Type != "target.transitioned" || target.original == "degraded" || target.original == "unknown" || target.original == "stopped" || !targetPhaseAllowed(r.state, event.ToState) {
				return fmt.Errorf("invalid ordinary target transition")
			}
			r.physicalTransition = true
		}
		if validTransitionPayload(event.Payload) != nil {
			return fmt.Errorf("invalid transition payload")
		}
		target.state, target.version, r.revision = event.ToState, event.ToVersion, event.AggregateVersion
	case "target.observed":
		if IsMaintenanceTerminal(r.state) || event.TargetID == "" || r.targets[event.TargetID] == nil || event.AggregateVersion != 0 || event.FromState != "" || event.ToState != "" || event.FromVersion != 0 || event.ToVersion != 0 {
			return fmt.Errorf("invalid observation")
		}
		if canonical, err := canonicalJSONObject(event.Payload); err != nil || string(canonical) != string(event.Payload) {
			return fmt.Errorf("invalid observation payload")
		}
	default:
		return fmt.Errorf("unknown maintenance event type %s", event.Type)
	}
	return nil
}

func validEventJustification(payload json.RawMessage) error {
	var value struct {
		Justification string `json:"justification"`
	}
	if json.Unmarshal(payload, &value) != nil {
		return ErrInvalid
	}
	return validJustification(value.Justification)
}

func validTransitionPayload(payload json.RawMessage) error {
	var value struct {
		Justification string          `json:"justification"`
		Detail        json.RawMessage `json:"detail"`
	}
	if json.Unmarshal(payload, &value) != nil || len(value.Detail) == 0 || !json.Valid(value.Detail) {
		return ErrInvalid
	}
	return nil
}
