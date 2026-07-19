package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

func appendEvent(ctx context.Context, tx *sql.Tx, event Event) error {
	if event.Type == "" || event.ResourceType == "" || event.ResourceID == "" || event.OccurredAt.IsZero() {
		return fmt.Errorf("%w: event identity is required", ErrInvalid)
	}
	event.Actor, event.Role, event.RequestID = auditValues(AuditContext{Actor: event.Actor, Role: event.Role, RequestID: event.RequestID})
	if len(event.Payload) == 0 {
		event.Payload = json.RawMessage(`{}`)
	}
	if !json.Valid(event.Payload) {
		return fmt.Errorf("%w: event payload must be valid JSON", ErrInvalid)
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO journal_events(
		event_type,occurred_at,request_id,actor,role,resource_type,resource_id,
		maintenance_id,target_id,from_state,to_state,from_version,to_version,aggregate_version,payload_json
	) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		event.Type, event.OccurredAt.UTC().Format(time.RFC3339Nano), event.RequestID,
		event.Actor, event.Role, event.ResourceType, event.ResourceID,
		nullText(event.MaintenanceID), nullText(event.TargetID), nullText(event.FromState), nullText(event.ToState),
		nullInt(event.FromVersion), nullInt(event.ToVersion), nullInt(event.AggregateVersion), string(event.Payload))
	return err
}

func eventPayload(detail json.RawMessage, justification string) (json.RawMessage, error) {
	if len(detail) == 0 {
		detail = json.RawMessage(`{}`)
	}
	if !json.Valid(detail) {
		return nil, fmt.Errorf("%w: detail must be valid JSON", ErrInvalid)
	}
	if len(detail) > 64*1024 {
		return nil, fmt.Errorf("%w: detail exceeds 64 KiB", ErrInvalid)
	}
	payload := struct {
		Justification string          `json:"justification,omitempty"`
		Detail        json.RawMessage `json:"detail"`
	}{Justification: justification, Detail: detail}
	return json.Marshal(payload)
}

func nullText(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func nullInt(value int64) any {
	if value == 0 {
		return nil
	}
	return value
}
