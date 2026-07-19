PRAGMA foreign_keys = ON;

CREATE TABLE maintenance (
    id TEXT PRIMARY KEY NOT NULL,
    state TEXT NOT NULL,
    change_ticket TEXT NOT NULL DEFAULT '',
    workflow_version TEXT NOT NULL DEFAULT '',
    metadata_json TEXT NOT NULL DEFAULT '{}',
    version INTEGER NOT NULL DEFAULT 1 CHECK (version > 0),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    CHECK (length(id) BETWEEN 1 AND 200),
    CHECK (json_valid(metadata_json))
);

CREATE TABLE maintenance_captures (
    maintenance_id TEXT PRIMARY KEY NOT NULL REFERENCES maintenance(id),
    captured_at TEXT NOT NULL,
    payload_json TEXT NOT NULL,
    sha256 TEXT NOT NULL,
    CHECK (json_valid(payload_json))
);

CREATE TABLE targets (
    maintenance_id TEXT NOT NULL REFERENCES maintenance(id),
    id TEXT NOT NULL,
    lock_key TEXT NOT NULL,
    state TEXT NOT NULL,
    original_state TEXT NOT NULL,
    kind TEXT NOT NULL DEFAULT '',
    host TEXT NOT NULL DEFAULT '',
    observed_at TEXT,
    metadata_json TEXT NOT NULL DEFAULT '{}',
    version INTEGER NOT NULL DEFAULT 1 CHECK (version > 0),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (maintenance_id, id),
    CHECK (length(id) BETWEEN 1 AND 200),
    CHECK (length(lock_key) BETWEEN 1 AND 512),
    CHECK (json_valid(metadata_json))
);

CREATE TABLE journal_events (
    journal_id INTEGER PRIMARY KEY,
    event_type TEXT NOT NULL,
    occurred_at TEXT NOT NULL,
    request_id TEXT NOT NULL,
    actor TEXT NOT NULL,
    role TEXT NOT NULL,
    resource_type TEXT NOT NULL,
    resource_id TEXT NOT NULL,
    maintenance_id TEXT REFERENCES maintenance(id),
    target_id TEXT,
    from_state TEXT,
    to_state TEXT,
    from_version INTEGER,
    to_version INTEGER,
    aggregate_version INTEGER,
    payload_json TEXT NOT NULL DEFAULT '{}',
    FOREIGN KEY (maintenance_id, target_id) REFERENCES targets(maintenance_id, id),
    CHECK (json_valid(payload_json)),
    CHECK (target_id IS NULL OR maintenance_id IS NOT NULL),
    CHECK (
        (from_state IS NULL AND to_state IS NULL AND from_version IS NULL AND to_version IS NULL)
        OR
        (from_state IS NOT NULL AND to_state IS NOT NULL AND from_version > 0 AND to_version = from_version + 1)
    )
);

CREATE INDEX idx_maintenance_list ON maintenance(state, updated_at DESC, id DESC);
CREATE INDEX idx_targets_lock ON targets(lock_key, maintenance_id);
CREATE INDEX idx_events_scope ON journal_events(maintenance_id, target_id, journal_id DESC);
CREATE INDEX idx_events_type ON journal_events(event_type, journal_id DESC);
CREATE INDEX idx_events_resource ON journal_events(resource_type, resource_id, journal_id DESC);
CREATE UNIQUE INDEX idx_events_aggregate_version
    ON journal_events(maintenance_id, aggregate_version)
    WHERE aggregate_version IS NOT NULL;
CREATE UNIQUE INDEX idx_events_observation_request
    ON journal_events(maintenance_id, target_id, request_id)
    WHERE event_type = 'target.observed';

CREATE TRIGGER maintenance_identity_immutable
BEFORE UPDATE ON maintenance
WHEN NEW.id <> OLD.id
  OR NEW.change_ticket <> OLD.change_ticket
  OR NEW.workflow_version <> OLD.workflow_version
  OR NEW.metadata_json <> OLD.metadata_json
  OR NEW.created_at <> OLD.created_at
BEGIN
    SELECT RAISE(ABORT, 'maintenance identity is immutable');
END;

CREATE TRIGGER maintenance_captures_immutable_update
BEFORE UPDATE ON maintenance_captures
BEGIN
    SELECT RAISE(ABORT, 'maintenance capture is immutable');
END;

CREATE TRIGGER maintenance_captures_immutable_delete
BEFORE DELETE ON maintenance_captures
BEGIN
    SELECT RAISE(ABORT, 'maintenance capture is immutable');
END;

CREATE TRIGGER targets_identity_immutable
BEFORE UPDATE ON targets
WHEN NEW.maintenance_id <> OLD.maintenance_id
  OR NEW.id <> OLD.id
  OR NEW.lock_key <> OLD.lock_key
  OR NEW.original_state <> OLD.original_state
  OR NEW.kind <> OLD.kind
  OR NEW.host <> OLD.host
  OR coalesce(NEW.observed_at, '') <> coalesce(OLD.observed_at, '')
  OR NEW.metadata_json <> OLD.metadata_json
  OR NEW.created_at <> OLD.created_at
BEGIN
    SELECT RAISE(ABORT, 'target identity is immutable');
END;

CREATE TRIGGER targets_lock_exclusive
BEFORE INSERT ON targets
WHEN EXISTS (
    SELECT 1
    FROM targets existing
    JOIN maintenance owner ON owner.id = existing.maintenance_id
    WHERE existing.lock_key = NEW.lock_key
      AND existing.maintenance_id <> NEW.maintenance_id
      AND owner.state NOT IN ('completed','cancelled')
)
BEGIN
    SELECT RAISE(ABORT, 'lock_key is held by another maintenance');
END;

CREATE TRIGGER targets_immutable_delete
BEFORE DELETE ON targets
BEGIN
    SELECT RAISE(ABORT, 'captured target is immutable');
END;

CREATE TRIGGER journal_events_append_only_update
BEFORE UPDATE ON journal_events
BEGIN
    SELECT RAISE(ABORT, 'journal events are append-only');
END;

CREATE TRIGGER journal_events_append_only_delete
BEFORE DELETE ON journal_events
BEGIN
    SELECT RAISE(ABORT, 'journal events are append-only');
END;
