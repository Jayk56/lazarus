package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

func (s *Store) CreateMaintenance(ctx context.Context, m Maintenance, audit AuditContext) (Maintenance, error) {
	if m.State == "" {
		m.State = "new"
	}
	if !validID(m.ID) || m.State != "new" {
		return Maintenance{}, fmt.Errorf("%w: invalid maintenance fields", ErrInvalid)
	}
	if len(m.ChangeTicket) > 200 || len(m.WorkflowVersion) > 100 {
		return Maintenance{}, fmt.Errorf("%w: metadata fields too long", ErrInvalid)
	}
	metadata, err := canonicalJSONDefault(m.Metadata)
	if err != nil {
		return Maintenance{}, err
	}
	if len(metadata) > 64*1024 {
		return Maintenance{}, fmt.Errorf("%w: metadata exceeds 64 KiB", ErrInvalid)
	}
	now := s.now()
	m.Metadata, m.Version, m.CreatedAt, m.UpdatedAt = metadata, 1, now, now
	err = s.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO maintenance(id,state,change_ticket,workflow_version,metadata_json,version,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?)`,
			m.ID, m.State, m.ChangeTicket, m.WorkflowVersion, string(m.Metadata), m.Version, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano))
		if err != nil {
			if isUnique(err) {
				return conflictError(ErrConflict, "maintenance_id_exists", "maintenance", m.ID, 0, 1, "", "new")
			}
			return err
		}
		created, _ := json.Marshal(m)
		actor, role, requestID := auditValues(audit)
		return appendEvent(ctx, tx, Event{Type: "maintenance.created", OccurredAt: now, RequestID: requestID, Actor: actor, Role: role, ResourceType: "maintenance", ResourceID: m.ID, MaintenanceID: m.ID, AggregateVersion: 1, Payload: created})
	})
	if err != nil {
		return Maintenance{}, err
	}
	return m, nil
}

func canonicalJSONDefault(raw json.RawMessage) (json.RawMessage, error) {
	normalized, err := normalizeJSON(raw)
	if err != nil {
		return nil, err
	}
	canonical, err := canonicalJSON(normalized)
	return json.RawMessage(canonical), err
}

func canonicalJSONObject(raw json.RawMessage) (json.RawMessage, error) {
	canonical, err := canonicalJSONDefault(raw)
	if err != nil {
		return nil, err
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(canonical, &object) != nil || object == nil {
		return nil, fmt.Errorf("%w: payload must be a JSON object", ErrInvalid)
	}
	return canonical, nil
}

type captureTarget struct {
	TargetID      string          `json:"target_id"`
	LockKey       string          `json:"lock_key"`
	Kind          string          `json:"kind"`
	Host          string          `json:"host"`
	OriginalState string          `json:"original_state"`
	ObservedAt    *time.Time      `json:"observed_at"`
	Metadata      json.RawMessage `json:"metadata"`
}

func parseCaptureTargets(payload []byte) ([]captureTarget, error) {
	var envelope struct {
		Targets json.RawMessage `json:"targets"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return nil, fmt.Errorf("%w: invalid capture envelope", ErrInvalid)
	}
	if len(envelope.Targets) == 0 {
		return []captureTarget{}, nil
	}
	var targets []captureTarget
	if err := json.Unmarshal(envelope.Targets, &targets); err != nil || targets == nil {
		return nil, fmt.Errorf("%w: capture targets must be an array", ErrInvalid)
	}
	seen := make(map[string]bool, len(targets))
	for i := range targets {
		target := &targets[i]
		if !validID(target.TargetID) || seen[target.TargetID] {
			return nil, fmt.Errorf("%w: capture target id is missing or duplicated", ErrInvalid)
		}
		seen[target.TargetID] = true
		target.LockKey = strings.TrimSpace(target.LockKey)
		if target.LockKey == "" || len(target.LockKey) > 512 {
			return nil, fmt.Errorf("%w: capture target lock_key is required", ErrInvalid)
		}
		if len(target.Kind) > 100 || len(target.Host) > 512 {
			return nil, fmt.Errorf("%w: capture target fields too long", ErrInvalid)
		}
		switch target.OriginalState {
		case "running", "stopped", "degraded", "unknown":
		default:
			return nil, fmt.Errorf("%w: capture target %s has invalid original_state", ErrInvalid, target.TargetID)
		}
		metadata, err := canonicalJSONDefault(target.Metadata)
		if err != nil {
			return nil, err
		}
		if len(metadata) > 64*1024 {
			return nil, fmt.Errorf("%w: target metadata exceeds 64 KiB", ErrInvalid)
		}
		target.Metadata = metadata
	}
	return targets, nil
}

func (s *Store) CaptureMaintenance(ctx context.Context, maintenanceID string, capture Capture, audit AuditContext) (CaptureResult, error) {
	if !validID(maintenanceID) {
		return CaptureResult{}, fmt.Errorf("%w: invalid maintenance id", ErrInvalid)
	}
	payload, err := canonicalJSONObject(capture.Payload)
	if err != nil {
		return CaptureResult{}, err
	}
	targets, err := parseCaptureTargets(payload)
	if err != nil {
		return CaptureResult{}, err
	}
	sum := sha256.Sum256(payload)
	capture.MaintenanceID, capture.Payload, capture.SHA256 = maintenanceID, payload, hex.EncodeToString(sum[:])
	if capture.CapturedAt.IsZero() {
		capture.CapturedAt = s.now()
	} else {
		capture.CapturedAt = capture.CapturedAt.UTC()
	}
	var result CaptureResult
	err = s.write(ctx, func(tx *sql.Tx) error {
		m, err := scanMaintenance(tx.QueryRowContext(ctx, `SELECT `+maintenanceColumns+` FROM maintenance WHERE id=?`, maintenanceID))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		var existing Capture
		var at, existingPayload string
		err = tx.QueryRowContext(ctx, `SELECT maintenance_id,captured_at,payload_json,sha256 FROM maintenance_captures WHERE maintenance_id=?`, maintenanceID).Scan(&existing.MaintenanceID, &at, &existingPayload, &existing.SHA256)
		if err == nil {
			existing.CapturedAt, _ = time.Parse(time.RFC3339Nano, at)
			existing.Payload = json.RawMessage(existingPayload)
			if existing.SHA256 == capture.SHA256 && existingPayload == string(capture.Payload) {
				result = CaptureResult{Capture: existing}
				return nil
			}
			return conflictError(ErrConflict, "immutable_capture_mismatch", "capture", maintenanceID, 0, 0, m.State, "captured")
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if m.State != "new" {
			return conflictError(ErrConflict, "capture_requires_new_maintenance", "maintenance", maintenanceID, 0, m.Version, m.State, "captured")
		}
		for _, target := range targets {
			var owner string
			err := tx.QueryRowContext(ctx, `SELECT t.maintenance_id FROM targets t JOIN maintenance m ON m.id=t.maintenance_id WHERE t.lock_key=? AND m.state NOT IN ('completed','cancelled') LIMIT 1`, target.LockKey).Scan(&owner)
			if err == nil {
				return &ConflictError{Base: ErrConflict, Reason: "lock_in_use", ResourceType: "lock", ResourceID: target.LockKey, Holder: owner}
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO maintenance_captures(maintenance_id,captured_at,payload_json,sha256) VALUES(?,?,?,?)`, maintenanceID, capture.CapturedAt.Format(time.RFC3339Nano), string(capture.Payload), capture.SHA256); err != nil {
			return err
		}
		stamp := capture.CapturedAt.Format(time.RFC3339Nano)
		for _, input := range targets {
			state := "captured"
			if input.OriginalState == "stopped" {
				state = "stopped"
			}
			var observed any
			if input.ObservedAt != nil {
				observed = input.ObservedAt.UTC().Format(time.RFC3339Nano)
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO targets(maintenance_id,id,lock_key,state,original_state,kind,host,observed_at,metadata_json,version,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,1,?,?)`, maintenanceID, input.TargetID, input.LockKey, state, input.OriginalState, input.Kind, input.Host, observed, string(input.Metadata), stamp, stamp); err != nil {
				return err
			}
		}
		now := s.now()
		newRevision := m.Version + 1
		updated, err := tx.ExecContext(ctx, `UPDATE maintenance SET state='captured',version=?,updated_at=? WHERE id=? AND version=?`, newRevision, now.Format(time.RFC3339Nano), maintenanceID, m.Version)
		if err != nil {
			return err
		}
		if n, _ := updated.RowsAffected(); n != 1 {
			return conflictError(ErrVersionConflict, "maintenance_version_changed", "maintenance", maintenanceID, m.Version, m.Version, m.State, "captured")
		}
		detail, _ := json.Marshal(map[string]any{"sha256": capture.SHA256, "targets": len(targets)})
		actor, role, requestID := auditValues(audit)
		if err := appendEvent(ctx, tx, Event{Type: "maintenance.captured", OccurredAt: now, RequestID: requestID, Actor: actor, Role: role, ResourceType: "maintenance", ResourceID: maintenanceID, MaintenanceID: maintenanceID, FromState: "new", ToState: "captured", FromVersion: m.Version, ToVersion: newRevision, AggregateVersion: newRevision, Payload: detail}); err != nil {
			return err
		}
		result = CaptureResult{Capture: capture, Created: true}
		return nil
	})
	if err != nil {
		return CaptureResult{}, err
	}
	return result, nil
}

func (s *Store) TransitionMaintenance(ctx context.Context, id string, change StateChange, precondition VersionPrecondition, audit AuditContext) (MaintenanceAggregate, error) {
	if !validID(id) || !ValidMaintenanceState(change.State) {
		return MaintenanceAggregate{}, fmt.Errorf("%w: invalid maintenance transition", ErrInvalid)
	}
	payload, err := eventPayload(change.Detail, strings.TrimSpace(change.Justification))
	if err != nil {
		return MaintenanceAggregate{}, err
	}
	var aggregate MaintenanceAggregate
	err = s.write(ctx, func(tx *sql.Tx) error {
		m, err := scanMaintenance(tx.QueryRowContext(ctx, `SELECT `+maintenanceColumns+` FROM maintenance WHERE id=?`, id))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if err := precondition.require(m.Version, "maintenance", id, m.State, change.State); err != nil {
			return err
		}
		eventType := "maintenance.transitioned"
		if m.State == "failed" {
			if !isAdmin(audit.Role) {
				return ErrForbidden
			}
			if err := validJustification(change.Justification); err != nil {
				return err
			}
			var captureCount int
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM maintenance_captures WHERE maintenance_id=?`, id).Scan(&captureCount); err != nil {
				return err
			}
			if captureCount == 0 && change.State != "new" {
				return conflictError(ErrConflict, "uncaptured_failure_reopens_to_new", "maintenance", id, 0, m.Version, m.State, change.State)
			}
			if captureCount == 1 && (change.State == "new" || change.State == "completed" || change.State == "cancelled") {
				return conflictError(ErrConflict, "captured_failure_reopens_to_active_phase", "maintenance", id, 0, m.Version, m.State, change.State)
			}
			eventType = "maintenance.reopened"
		} else if IsMaintenanceTerminal(m.State) {
			return conflictError(ErrConflict, "maintenance_terminal", "maintenance", id, 0, m.Version, m.State, change.State)
		}
		if change.State == "captured" && m.State != "failed" {
			return fmt.Errorf("%w: capture owns the captured transition", ErrInvalid)
		}
		if !ValidMaintenanceTransition(m.State, change.State) {
			return fmt.Errorf("%w: %s -> %s is not allowed", ErrInvalid, m.State, change.State)
		}
		if change.State == "cancelled" && m.State == "captured" {
			var exercised int
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM journal_events WHERE maintenance_id=? AND event_type='target.transitioned'`, id).Scan(&exercised); err != nil {
				return err
			}
			if exercised != 0 {
				return conflictError(ErrConflict, "cancel_requires_unexercised_targets", "maintenance", id, 0, m.Version, m.State, change.State)
			}
		}
		if change.State == "stopped" {
			if err := validateStoppedBarrier(ctx, tx, id); err != nil {
				return err
			}
		}
		if change.State == "completed" {
			if err := validateCompletion(ctx, tx, id); err != nil {
				return err
			}
			eventType = "maintenance.completed"
		}
		now, newRevision := s.now(), m.Version+1
		var captureCount int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM maintenance_captures WHERE maintenance_id=?`, id).Scan(&captureCount); err != nil {
			return err
		}
		if captureCount == 1 {
			if _, err := tx.ExecContext(ctx, `UPDATE targets SET version=version+1,updated_at=? WHERE maintenance_id=?`, now.Format(time.RFC3339Nano), id); err != nil {
				return err
			}
		}
		updated, err := tx.ExecContext(ctx, `UPDATE maintenance SET state=?,version=?,updated_at=? WHERE id=? AND version=?`, change.State, newRevision, now.Format(time.RFC3339Nano), id, m.Version)
		if err != nil {
			return err
		}
		if n, _ := updated.RowsAffected(); n != 1 {
			return conflictError(ErrVersionConflict, "maintenance_version_changed", "maintenance", id, m.Version, m.Version, m.State, change.State)
		}
		actor, role, requestID := auditValues(audit)
		if err := appendEvent(ctx, tx, Event{Type: eventType, OccurredAt: now, RequestID: requestID, Actor: actor, Role: role, ResourceType: "maintenance", ResourceID: id, MaintenanceID: id, FromState: m.State, ToState: change.State, FromVersion: m.Version, ToVersion: newRevision, AggregateVersion: newRevision, Payload: payload}); err != nil {
			return err
		}
		m.State, m.Version, m.UpdatedAt = change.State, newRevision, now
		targets, err := listTargets(ctx, tx, id)
		if err != nil {
			return err
		}
		aggregate = MaintenanceAggregate{Maintenance: m, Targets: targets}
		return nil
	})
	if err != nil {
		return MaintenanceAggregate{}, err
	}
	return aggregate, nil
}

func validateCompletion(ctx context.Context, tx *sql.Tx, maintenanceID string) error {
	var captures int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM maintenance_captures WHERE maintenance_id=?`, maintenanceID).Scan(&captures); err != nil {
		return err
	}
	if captures != 1 {
		return conflictError(ErrConflict, "maintenance_has_no_capture", "maintenance", maintenanceID, 0, 0, "", "completed")
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,state,original_state,version FROM targets WHERE maintenance_id=? ORDER BY id`, maintenanceID)
	if err != nil {
		return err
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var id, state, original string
		var version int64
		if err := rows.Scan(&id, &state, &original, &version); err != nil {
			return err
		}
		count++
		if !targetCompletionAllowed(original, state) {
			return conflictError(ErrConflict, "target_not_reconciled", "target", id, 0, version, state, "completed")
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if count == 0 {
		return conflictError(ErrConflict, "maintenance_has_no_targets", "maintenance", maintenanceID, 0, 0, "", "completed")
	}
	return nil
}

func validateStoppedBarrier(ctx context.Context, tx *sql.Tx, maintenanceID string) error {
	var unresolved int
	err := tx.QueryRowContext(ctx, `SELECT count(*) FROM targets WHERE maintenance_id=? AND CASE
		WHEN original_state IN ('running','stopped') THEN state NOT IN ('stopped','skipped')
		ELSE state <> 'skipped' END`, maintenanceID).Scan(&unresolved)
	if err != nil {
		return err
	}
	if unresolved != 0 {
		return conflictError(ErrConflict, "targets_not_stopped", "maintenance", maintenanceID, 0, 0, "stopping", "stopped")
	}
	return nil
}

func (s *Store) TransitionTarget(ctx context.Context, maintenanceID, targetID string, change StateChange, precondition VersionPrecondition, audit AuditContext) (TargetTransitionResult, error) {
	if !validID(maintenanceID) || !validID(targetID) || !ValidTargetState(change.State) {
		return TargetTransitionResult{}, fmt.Errorf("%w: invalid target transition", ErrInvalid)
	}
	payload, err := eventPayload(change.Detail, strings.TrimSpace(change.Justification))
	if err != nil {
		return TargetTransitionResult{}, err
	}
	var result TargetTransitionResult
	err = s.write(ctx, func(tx *sql.Tx) error {
		m, err := scanMaintenance(tx.QueryRowContext(ctx, `SELECT `+maintenanceColumns+` FROM maintenance WHERE id=?`, maintenanceID))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if IsMaintenanceTerminal(m.State) {
			return conflictError(ErrConflict, "parent_maintenance_terminal", "maintenance", maintenanceID, 0, m.Version, m.State, change.State)
		}
		target, err := scanTarget(tx.QueryRowContext(ctx, `SELECT `+targetColumns+` FROM targets WHERE maintenance_id=? AND id=?`, maintenanceID, targetID))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if err := precondition.require(target.Version, "target", targetID, target.State, change.State); err != nil {
			return err
		}
		eventType := "target.transitioned"
		if change.State == "skipped" {
			if !isAdmin(audit.Role) {
				return ErrForbidden
			}
			if err := validJustification(change.Justification); err != nil {
				return err
			}
			eventType = "target.skipped"
		} else if target.OriginalState == "degraded" || target.OriginalState == "unknown" {
			return conflictError(ErrConflict, "unresolved_target_requires_skip", "target", targetID, 0, target.Version, target.State, change.State)
		}
		if target.State == "healthy" || target.State == "skipped" {
			return conflictError(ErrConflict, "target_terminal", "target", targetID, 0, target.Version, target.State, change.State)
		}
		if !ValidTargetTransition(target.State, change.State) {
			return fmt.Errorf("%w: %s -> %s is not allowed", ErrInvalid, target.State, change.State)
		}
		if target.OriginalState == "stopped" && change.State != "skipped" {
			return conflictError(ErrConflict, "originally_stopped_target", "target", targetID, 0, target.Version, target.State, change.State)
		}
		if change.State != "skipped" && !targetPhaseAllowed(m.State, change.State) {
			return conflictError(ErrConflict, "target_phase_mismatch", "target", targetID, 0, target.Version, target.State, change.State)
		}
		now, newTargetVersion, newRevision := s.now(), target.Version+1, m.Version+1
		updated, err := tx.ExecContext(ctx, `UPDATE targets SET state=?,version=?,updated_at=? WHERE maintenance_id=? AND id=? AND version=?`, change.State, newTargetVersion, now.Format(time.RFC3339Nano), maintenanceID, targetID, target.Version)
		if err != nil {
			return err
		}
		if n, _ := updated.RowsAffected(); n != 1 {
			return conflictError(ErrVersionConflict, "target_version_changed", "target", targetID, target.Version, target.Version, target.State, change.State)
		}
		parent, err := tx.ExecContext(ctx, `UPDATE maintenance SET version=?,updated_at=? WHERE id=? AND version=?`, newRevision, now.Format(time.RFC3339Nano), maintenanceID, m.Version)
		if err != nil {
			return err
		}
		if n, _ := parent.RowsAffected(); n != 1 {
			return conflictError(ErrVersionConflict, "maintenance_version_changed", "maintenance", maintenanceID, m.Version, m.Version, m.State, change.State)
		}
		actor, role, requestID := auditValues(audit)
		if err := appendEvent(ctx, tx, Event{Type: eventType, OccurredAt: now, RequestID: requestID, Actor: actor, Role: role, ResourceType: "target", ResourceID: targetID, MaintenanceID: maintenanceID, TargetID: targetID, FromState: target.State, ToState: change.State, FromVersion: target.Version, ToVersion: newTargetVersion, AggregateVersion: newRevision, Payload: payload}); err != nil {
			return err
		}
		target.State, target.Version, target.UpdatedAt = change.State, newTargetVersion, now
		result = TargetTransitionResult{Target: target, MaintenanceVersion: newRevision}
		return nil
	})
	if err != nil {
		return TargetTransitionResult{}, err
	}
	return result, nil
}

func validJustification(value string) error {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 4096 {
		return fmt.Errorf("%w: justification must be between 1 and 4096 characters", ErrInvalid)
	}
	return nil
}

func (s *Store) AddObservation(ctx context.Context, maintenanceID, targetID string, payload json.RawMessage, precondition VersionPrecondition, audit AuditContext) error {
	if !validID(maintenanceID) || !validID(targetID) {
		return fmt.Errorf("%w: invalid target identity", ErrInvalid)
	}
	canonical, err := canonicalJSONObject(payload)
	if err != nil {
		return err
	}
	if len(canonical) > 64*1024 {
		return fmt.Errorf("%w: observation exceeds 64 KiB", ErrInvalid)
	}
	actor, role, requestID := auditValues(audit)
	return s.write(ctx, func(tx *sql.Tx) error {
		var existing string
		err := tx.QueryRowContext(ctx, `SELECT payload_json FROM journal_events WHERE event_type='target.observed' AND maintenance_id=? AND target_id=? AND request_id=?`, maintenanceID, targetID, requestID).Scan(&existing)
		if err == nil {
			if existing == string(canonical) {
				return nil
			}
			return conflictError(ErrConflict, "observation_request_id_reused", "target", targetID, 0, 0, "", "observed")
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		m, err := scanMaintenance(tx.QueryRowContext(ctx, `SELECT `+maintenanceColumns+` FROM maintenance WHERE id=?`, maintenanceID))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		target, err := scanTarget(tx.QueryRowContext(ctx, `SELECT `+targetColumns+` FROM targets WHERE maintenance_id=? AND id=?`, maintenanceID, targetID))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if IsMaintenanceTerminal(m.State) {
			return conflictError(ErrConflict, "parent_maintenance_terminal", "maintenance", maintenanceID, 0, m.Version, m.State, "observed")
		}
		if err := precondition.require(target.Version, "target", targetID, target.State, "observed"); err != nil {
			return err
		}
		return appendEvent(ctx, tx, Event{Type: "target.observed", OccurredAt: s.now(), RequestID: requestID, Actor: actor, Role: role, ResourceType: "target", ResourceID: targetID, MaintenanceID: maintenanceID, TargetID: targetID, Payload: canonical})
	})
}
