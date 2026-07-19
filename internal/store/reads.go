package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

type rowScanner interface{ Scan(...any) error }

const maintenanceColumns = `id,state,change_ticket,workflow_version,metadata_json,version,created_at,updated_at`
const targetColumns = `id,maintenance_id,lock_key,state,original_state,kind,host,observed_at,metadata_json,version,created_at,updated_at`

func scanMaintenance(row rowScanner) (Maintenance, error) {
	var out Maintenance
	var metadata, created, updated string
	if err := row.Scan(&out.ID, &out.State, &out.ChangeTicket, &out.WorkflowVersion, &metadata, &out.Version, &created, &updated); err != nil {
		return Maintenance{}, err
	}
	out.Metadata = json.RawMessage(metadata)
	out.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	out.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	return out, nil
}

func scanTarget(row rowScanner) (Target, error) {
	var out Target
	var observed sql.NullString
	var metadata, created, updated string
	if err := row.Scan(&out.ID, &out.MaintenanceID, &out.LockKey, &out.State, &out.OriginalState, &out.Kind, &out.Host, &observed, &metadata, &out.Version, &created, &updated); err != nil {
		return Target{}, err
	}
	if observed.Valid {
		parsed, _ := time.Parse(time.RFC3339Nano, observed.String)
		out.ObservedAt = &parsed
	}
	out.Metadata = json.RawMessage(metadata)
	out.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	out.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	return out, nil
}

func (s *Store) GetCapture(ctx context.Context, maintenanceID string) (Capture, error) {
	var out Capture
	var at, payload string
	err := s.db.QueryRowContext(ctx, `SELECT maintenance_id,captured_at,payload_json,sha256 FROM maintenance_captures WHERE maintenance_id=?`, maintenanceID).Scan(&out.MaintenanceID, &at, &payload, &out.SHA256)
	if errors.Is(err, sql.ErrNoRows) {
		return Capture{}, ErrNotFound
	}
	if err != nil {
		return Capture{}, err
	}
	out.CapturedAt, _ = time.Parse(time.RFC3339Nano, at)
	out.Payload = json.RawMessage(payload)
	return out, nil
}

func (s *Store) GetTarget(ctx context.Context, maintenanceID, targetID string) (Target, error) {
	if !validID(maintenanceID) || !validID(targetID) {
		return Target{}, fmt.Errorf("%w: invalid target identity", ErrInvalid)
	}
	target, err := scanTarget(s.db.QueryRowContext(ctx, `SELECT `+targetColumns+` FROM targets WHERE maintenance_id=? AND id=?`, maintenanceID, targetID))
	if errors.Is(err, sql.ErrNoRows) {
		return Target{}, ErrNotFound
	}
	return target, err
}

func (s *Store) GetMaintenanceAggregate(ctx context.Context, id string) (MaintenanceAggregate, error) {
	if !validID(id) {
		return MaintenanceAggregate{}, fmt.Errorf("%w: invalid maintenance id", ErrInvalid)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return MaintenanceAggregate{}, err
	}
	defer tx.Rollback()
	m, err := scanMaintenance(tx.QueryRowContext(ctx, `SELECT `+maintenanceColumns+` FROM maintenance WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return MaintenanceAggregate{}, ErrNotFound
	}
	if err != nil {
		return MaintenanceAggregate{}, err
	}
	targets, err := listTargets(ctx, tx, id)
	if err != nil {
		return MaintenanceAggregate{}, err
	}
	if err := tx.Commit(); err != nil {
		return MaintenanceAggregate{}, err
	}
	return MaintenanceAggregate{Maintenance: m, Targets: targets}, nil
}

type queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func listTargets(ctx context.Context, q queryer, maintenanceID string) ([]Target, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+targetColumns+` FROM targets WHERE maintenance_id=? ORDER BY id`, maintenanceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	targets := make([]Target, 0)
	for rows.Next() {
		target, err := scanTarget(rows)
		if err != nil {
			return nil, err
		}
		targets = append(targets, target)
	}
	return targets, rows.Err()
}

func (s *Store) ListMaintenances(ctx context.Context, opts MaintenanceListOptions) (MaintenancePage, error) {
	if opts.State != "" && !ValidMaintenanceState(opts.State) {
		return MaintenancePage{}, fmt.Errorf("%w: invalid state", ErrInvalid)
	}
	if opts.MaintenanceID != "" && !validID(opts.MaintenanceID) {
		return MaintenancePage{}, fmt.Errorf("%w: invalid maintenance id", ErrInvalid)
	}
	cursor, err := decodeEventCursor(opts.Cursor)
	if err != nil {
		return MaintenancePage{}, err
	}
	where, args := make([]string, 0, 5), make([]any, 0, 8)
	for _, filter := range []struct{ column, value string }{
		{"state", opts.State}, {"id", opts.MaintenanceID}, {"change_ticket", opts.ChangeTicket}, {"workflow_version", opts.WorkflowVersion},
	} {
		if filter.value != "" {
			where = append(where, filter.column+"=?")
			args = append(args, filter.value)
		}
	}
	if cursor > 0 {
		where = append(where, `rowid < ?`)
		args = append(args, cursor)
	}
	query := `SELECT rowid,` + maintenanceColumns + ` FROM maintenance`
	if len(where) > 0 {
		query += ` WHERE ` + strings.Join(where, ` AND `)
	}
	limit := pageLimit(opts.Limit)
	query += ` ORDER BY rowid DESC LIMIT ?`
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return MaintenancePage{}, err
	}
	defer rows.Close()
	items := make([]Maintenance, 0, limit+1)
	rowIDs := make([]int64, 0, limit+1)
	for rows.Next() {
		var rowID int64
		var item Maintenance
		var metadata, created, updated string
		err := rows.Scan(&rowID, &item.ID, &item.State, &item.ChangeTicket, &item.WorkflowVersion, &metadata, &item.Version, &created, &updated)
		if err != nil {
			return MaintenancePage{}, err
		}
		item.Metadata = json.RawMessage(metadata)
		item.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		item.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
		items = append(items, item)
		rowIDs = append(rowIDs, rowID)
	}
	if err := rows.Err(); err != nil {
		return MaintenancePage{}, err
	}
	page := MaintenancePage{Items: items}
	if len(items) > limit {
		page.Items = items[:limit]
		page.NextCursor = encodeCursor(strconv.FormatInt(rowIDs[limit-1], 10))
	}
	return page, nil
}

func decodeEventCursor(value string) (int64, error) {
	decoded, err := decodeCursor(value)
	if err != nil || decoded == "" {
		return 0, err
	}
	id, err := strconv.ParseInt(decoded, 10, 64)
	if err != nil || id < 1 {
		return 0, fmt.Errorf("%w: invalid event cursor", ErrInvalid)
	}
	return id, nil
}

func (s *Store) ListEvents(ctx context.Context, opts EventListOptions) (EventPage, error) {
	if opts.MaintenanceID != "" && !validID(opts.MaintenanceID) {
		return EventPage{}, fmt.Errorf("%w: invalid maintenance id", ErrInvalid)
	}
	if opts.TargetID != "" && !validID(opts.TargetID) {
		return EventPage{}, fmt.Errorf("%w: invalid target id", ErrInvalid)
	}
	if opts.TargetID != "" && opts.MaintenanceID == "" {
		return EventPage{}, fmt.Errorf("%w: target filter requires maintenance id", ErrInvalid)
	}
	cursor, err := decodeEventCursor(opts.Cursor)
	if err != nil {
		return EventPage{}, err
	}
	where, args := make([]string, 0, 9), make([]any, 0, 10)
	if cursor > 0 {
		where = append(where, "journal_id < ?")
		args = append(args, cursor)
	}
	for _, filter := range []struct{ column, value string }{
		{"maintenance_id", opts.MaintenanceID}, {"target_id", opts.TargetID}, {"event_type", opts.EventType},
		{"resource_type", opts.ResourceType}, {"resource_id", opts.ResourceID}, {"actor", opts.Actor},
		{"role", opts.Role}, {"request_id", opts.RequestID},
	} {
		if filter.value != "" {
			where = append(where, filter.column+"=?")
			args = append(args, filter.value)
		}
	}
	query := `SELECT journal_id,event_type,occurred_at,request_id,actor,role,resource_type,resource_id,maintenance_id,target_id,from_state,to_state,from_version,to_version,aggregate_version,payload_json FROM journal_events`
	if len(where) > 0 {
		query += ` WHERE ` + strings.Join(where, ` AND `)
	}
	limit := pageLimit(opts.Limit)
	query += ` ORDER BY journal_id DESC LIMIT ?`
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return EventPage{}, err
	}
	defer rows.Close()
	items := make([]Event, 0, limit+1)
	for rows.Next() {
		var event Event
		var at, payload string
		var maintenanceID, targetID, fromState, toState sql.NullString
		var fromVersion, toVersion, aggregateVersion sql.NullInt64
		if err := rows.Scan(&event.ID, &event.Type, &at, &event.RequestID, &event.Actor, &event.Role, &event.ResourceType, &event.ResourceID, &maintenanceID, &targetID, &fromState, &toState, &fromVersion, &toVersion, &aggregateVersion, &payload); err != nil {
			return EventPage{}, err
		}
		event.OccurredAt, _ = time.Parse(time.RFC3339Nano, at)
		event.MaintenanceID, event.TargetID = maintenanceID.String, targetID.String
		event.FromState, event.ToState = fromState.String, toState.String
		event.FromVersion, event.ToVersion, event.AggregateVersion = fromVersion.Int64, toVersion.Int64, aggregateVersion.Int64
		event.Payload = json.RawMessage(payload)
		items = append(items, event)
	}
	if err := rows.Err(); err != nil {
		return EventPage{}, err
	}
	page := EventPage{Items: items}
	if len(items) > limit {
		page.Items = items[:limit]
		page.NextCursor = encodeCursor(strconv.FormatInt(items[limit-1].ID, 10))
	}
	return page, nil
}
