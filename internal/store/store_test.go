package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var testContext = context.Background()

func testStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "lazarus.db")
	s, err := Open(testContext, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, path
}

func actor(role, request string) AuditContext {
	return AuditContext{Actor: "test-" + role, Role: role, RequestID: request}
}
func exact(version int64) VersionPrecondition { return VersionPrecondition{Versions: []int64{version}} }

func createRun(t *testing.T, s *Store, id string) Maintenance {
	t.Helper()
	m, err := s.CreateMaintenance(testContext, Maintenance{ID: id, ChangeTicket: "CHG-1", WorkflowVersion: "test", Metadata: json.RawMessage(`{"owner":"ops"}`)}, actor("operator", id+"-create"))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func captureBody(targets string) json.RawMessage {
	return json.RawMessage(`{"source":"test","targets":[` + targets + `]}`)
}

func standardCapture() json.RawMessage {
	return captureBody(strings.Join([]string{
		`{"target_id":"web","lock_key":"vm:web","kind":"vm","host":"web.example","original_state":"running","metadata":{"zone":"a"}}`,
		`{"target_id":"db","lock_key":"vm:db","kind":"vm","host":"db.example","original_state":"stopped"}`,
		`{"target_id":"legacy","lock_key":"dependency:legacy","kind":"dependency","original_state":"unknown"}`,
	}, ","))
}

func captureRun(t *testing.T, s *Store, id string, payload json.RawMessage) CaptureResult {
	t.Helper()
	result, err := s.CaptureMaintenance(testContext, id, Capture{Payload: payload}, actor("operator", id+"-capture"))
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func transitionMaintenance(t *testing.T, s *Store, id, state string, version int64) MaintenanceAggregate {
	t.Helper()
	aggregate, err := s.TransitionMaintenance(testContext, id, StateChange{State: state, Detail: json.RawMessage(`{}`)}, exact(version), actor("operator", fmt.Sprintf("%s-%s", id, state)))
	if err != nil {
		t.Fatal(err)
	}
	return aggregate
}

func transitionTarget(t *testing.T, s *Store, maintenanceID, targetID, state string, version int64) TargetTransitionResult {
	t.Helper()
	result, err := s.TransitionTarget(testContext, maintenanceID, targetID, StateChange{State: state, Detail: json.RawMessage(`{}`)}, exact(version), actor("operator", fmt.Sprintf("%s-%s-%s", maintenanceID, targetID, state)))
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestSchemaIdentityAndShape(t *testing.T) {
	s, path := testStore(t)
	var appID, version int
	if err := s.DB().QueryRow(`PRAGMA application_id`).Scan(&appID); err != nil {
		t.Fatal(err)
	}
	if err := s.DB().QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if appID != applicationID || version != currentSchema {
		t.Fatalf("identity = %x/%d", appID, version)
	}
	rows, err := s.DB().Query(`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	if got := strings.Join(names, ","); got != "journal_events,maintenance,maintenance_captures,targets" {
		t.Fatalf("tables = %s", got)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(testContext, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.DB().Exec(`CREATE TABLE unexpected_application_table(id INTEGER)`); err != nil {
		t.Fatal(err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(testContext, path); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("extra application table accepted: %v", err)
	}
}

func TestExistingUnknownDatabaseIsRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "unknown.db")
	if err := os.WriteFile(path, []byte("not sqlite"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(testContext, path); err == nil {
		t.Fatal("expected rejection")
	}
}

func TestCaptureMaterializesAllTargetsAndIsIdempotent(t *testing.T) {
	s, _ := testStore(t)
	m := createRun(t, s, "capture")
	result := captureRun(t, s, m.ID, standardCapture())
	if !result.Created || result.Capture.MaintenanceID != m.ID || result.Capture.SHA256 == "" {
		t.Fatalf("capture result = %+v", result)
	}
	aggregate, err := s.GetMaintenanceAggregate(testContext, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if aggregate.Maintenance.Version != 2 || aggregate.Maintenance.State != "captured" || len(aggregate.Targets) != 3 {
		t.Fatalf("aggregate = %+v", aggregate)
	}
	wantStates := map[string]string{"web": "captured", "db": "stopped", "legacy": "captured"}
	for _, target := range aggregate.Targets {
		if target.State != wantStates[target.ID] || target.Version != 1 || target.LockKey == "" {
			t.Fatalf("target = %+v", target)
		}
	}
	transitionMaintenance(t, s, m.ID, "stopping", 2)
	replay, err := s.CaptureMaintenance(testContext, m.ID, Capture{Payload: standardCapture()}, actor("operator", "capture-retry"))
	if err != nil || replay.Created || replay.Capture.SHA256 != result.Capture.SHA256 {
		t.Fatalf("replay = %+v, %v", replay, err)
	}
	_, err = s.CaptureMaintenance(testContext, m.ID, Capture{Payload: captureBody(`{"target_id":"other","lock_key":"vm:other","original_state":"running"}`)}, actor("operator", "capture-conflict"))
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("mismatched replay = %v", err)
	}
}

func TestCaptureValidationAndLockExclusivity(t *testing.T) {
	s, _ := testStore(t)
	createRun(t, s, "owner")
	captureRun(t, s, "owner", captureBody(`{"target_id":"a","lock_key":"shared","original_state":"running"},{"target_id":"b","lock_key":"shared","original_state":"running"}`))
	createRun(t, s, "blocked")
	_, err := s.CaptureMaintenance(testContext, "blocked", Capture{Payload: captureBody(`{"target_id":"x","lock_key":"shared","original_state":"running"}`)}, actor("operator", "blocked-capture"))
	var conflict *ConflictError
	if !errors.As(err, &conflict) || conflict.Reason != "lock_in_use" {
		t.Fatalf("overlap = %v", err)
	}
	if _, err := s.TransitionMaintenance(testContext, "owner", StateChange{State: "cancelled"}, exact(2), actor("operator", "cancel")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CaptureMaintenance(testContext, "blocked", Capture{Payload: captureBody(`{"target_id":"x","lock_key":"shared","original_state":"running"}`)}, actor("operator", "blocked-capture-2")); err != nil {
		t.Fatal(err)
	}

	createRun(t, s, "failed-owner")
	captureRun(t, s, "failed-owner", captureBody(`{"target_id":"held","lock_key":"failed-lock","original_state":"running"}`))
	transitionMaintenance(t, s, "failed-owner", "failed", 2)
	createRun(t, s, "failed-blocked")
	_, err = s.CaptureMaintenance(testContext, "failed-blocked", Capture{Payload: captureBody(`{"target_id":"waiter","lock_key":"failed-lock","original_state":"running"}`)}, actor("operator", "failed-blocked-capture"))
	if !errors.As(err, &conflict) || conflict.Reason != "lock_in_use" {
		t.Fatalf("failed maintenance released lock: %v", err)
	}

	createRun(t, s, "bad-lock")
	if _, err := s.CaptureMaintenance(testContext, "bad-lock", Capture{Payload: captureBody(`{"target_id":"x","original_state":"running"}`)}, actor("operator", "bad")); !errors.Is(err, ErrInvalid) {
		t.Fatalf("missing lock = %v", err)
	}
}

func TestTargetPhasesBarriersAndAggregateRevision(t *testing.T) {
	s, _ := testStore(t)
	createRun(t, s, "flow")
	captureRun(t, s, "flow", standardCapture())
	if _, err := s.TransitionTarget(testContext, "flow", "web", StateChange{State: "stopping"}, exact(1), actor("operator", "too-early")); !errors.Is(err, ErrConflict) {
		t.Fatalf("phase gate = %v", err)
	}
	aggregate := transitionMaintenance(t, s, "flow", "stopping", 2)
	if aggregate.Maintenance.Version != 3 {
		t.Fatalf("revision = %d", aggregate.Maintenance.Version)
	}
	web, _ := s.GetTarget(testContext, "flow", "web")
	db, _ := s.GetTarget(testContext, "flow", "db")
	if web.Version != 2 || db.Version != 2 {
		t.Fatalf("parent invalidation versions web=%d db=%d", web.Version, db.Version)
	}
	first := transitionTarget(t, s, "flow", "web", "stopping", web.Version)
	if first.MaintenanceVersion != 4 || first.Target.Version != 3 {
		t.Fatalf("versions = %+v", first)
	}
	if currentDB, _ := s.GetTarget(testContext, "flow", "db"); currentDB.Version != db.Version {
		t.Fatalf("target A invalidated target B: before=%d after=%d", db.Version, currentDB.Version)
	}
	if _, err := s.TransitionTarget(testContext, "flow", "web", StateChange{State: "stopped"}, exact(web.Version), actor("operator", "same-target-stale")); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("same-target stale write = %v", err)
	}
	second := transitionTarget(t, s, "flow", "web", "stopped", 3)
	if second.MaintenanceVersion != 5 {
		t.Fatalf("revision = %d", second.MaintenanceVersion)
	}
	if _, err := s.TransitionMaintenance(testContext, "flow", StateChange{State: "stopped"}, exact(5), actor("operator", "barrier")); !errors.Is(err, ErrConflict) {
		t.Fatalf("unresolved barrier = %v", err)
	}
	legacy, _ := s.GetTarget(testContext, "flow", "legacy")
	skipped, err := s.TransitionTarget(testContext, "flow", "legacy", StateChange{State: "skipped", Justification: "cannot query legacy system"}, exact(legacy.Version), actor("admin", "skip"))
	if err != nil {
		t.Fatal(err)
	}
	aggregate = transitionMaintenance(t, s, "flow", "stopped", skipped.MaintenanceVersion)
	if aggregate.Maintenance.State != "stopped" {
		t.Fatalf("state = %s", aggregate.Maintenance.State)
	}
}

func TestStoppedAndUnresolvedTargetsOnlyPermitSkip(t *testing.T) {
	s, _ := testStore(t)
	createRun(t, s, "skip")
	captureRun(t, s, "skip", standardCapture())
	transitionMaintenance(t, s, "skip", "stopping", 2)
	db, _ := s.GetTarget(testContext, "skip", "db")
	if _, err := s.TransitionTarget(testContext, "skip", "db", StateChange{State: "starting"}, exact(db.Version), actor("operator", "start-stopped")); !errors.Is(err, ErrConflict) {
		t.Fatalf("stopped original = %v", err)
	}
	legacy, _ := s.GetTarget(testContext, "skip", "legacy")
	if _, err := s.TransitionTarget(testContext, "skip", "legacy", StateChange{State: "stopping"}, exact(legacy.Version), actor("operator", "legacy-stop")); !errors.Is(err, ErrConflict) {
		t.Fatalf("unresolved ordinary transition = %v", err)
	}
	if _, err := s.TransitionTarget(testContext, "skip", "legacy", StateChange{State: "skipped", Justification: "waive"}, exact(legacy.Version), actor("operator", "not-admin")); !errors.Is(err, ErrForbidden) {
		t.Fatalf("operator skip = %v", err)
	}
	if _, err := s.TransitionTarget(testContext, "skip", "legacy", StateChange{State: "skipped"}, exact(legacy.Version), actor("admin", "no-why")); !errors.Is(err, ErrInvalid) {
		t.Fatalf("missing justification = %v", err)
	}
}

func TestFailureReopenInvalidatesTargetETags(t *testing.T) {
	s, _ := testStore(t)
	createRun(t, s, "aba")
	captureRun(t, s, "aba", captureBody(`{"target_id":"web","lock_key":"vm:web","original_state":"running"}`))
	transitionMaintenance(t, s, "aba", "stopping", 2)
	before, _ := s.GetTarget(testContext, "aba", "web")
	failed := transitionMaintenance(t, s, "aba", "failed", 3)
	reopened, err := s.TransitionMaintenance(testContext, "aba", StateChange{State: "stopping", Justification: "resume captured workflow"}, exact(failed.Maintenance.Version), actor("admin", "reopen"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.TransitionTarget(testContext, "aba", "web", StateChange{State: "stopping"}, exact(before.Version), actor("operator", "delayed")); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("ABA write = %v", err)
	}
	current, _ := s.GetTarget(testContext, "aba", "web")
	if current.Version != before.Version+2 || reopened.Maintenance.Version != 5 {
		t.Fatalf("versions before=%d current=%d maintenance=%d", before.Version, current.Version, reopened.Maintenance.Version)
	}

	createRun(t, s, "uncaptured")
	failedNew := transitionMaintenance(t, s, "uncaptured", "failed", 1)
	if _, err := s.TransitionMaintenance(testContext, "uncaptured", StateChange{State: "captured", Justification: "bad"}, exact(failedNew.Maintenance.Version), actor("admin", "bad-reopen")); !errors.Is(err, ErrConflict) {
		t.Fatalf("uncaptured reopen = %v", err)
	}
	if _, err := s.TransitionMaintenance(testContext, "uncaptured", StateChange{State: "new", Justification: "retry discovery"}, exact(failedNew.Maintenance.Version), actor("admin", "good-reopen")); err != nil {
		t.Fatal(err)
	}
}

func TestCancellationCannotReleaseExercisedLockAfterReopen(t *testing.T) {
	s, path := testStore(t)
	createRun(t, s, "cancel-guard")
	captureRun(t, s, "cancel-guard", captureBody(`{"target_id":"web","lock_key":"vm:web","original_state":"running"}`))
	transitionMaintenance(t, s, "cancel-guard", "stopping", 2)
	transitionTarget(t, s, "cancel-guard", "web", "stopping", 2)
	failed := transitionMaintenance(t, s, "cancel-guard", "failed", 4)
	reopened, err := s.TransitionMaintenance(testContext, "cancel-guard", StateChange{State: "captured", Justification: "return to captured phase"}, exact(failed.Maintenance.Version), actor("admin", "reopen-captured"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyPath(testContext, path); err != nil {
		t.Fatalf("verify reopened-to-captured: %v", err)
	}
	if _, err := s.TransitionMaintenance(testContext, "cancel-guard", StateChange{State: "cancelled"}, exact(reopened.Maintenance.Version), actor("operator", "cancel-after-work")); !errors.Is(err, ErrConflict) {
		t.Fatalf("cancel after exercise = %v", err)
	}
}

func TestObservationPreconditionsAndReplay(t *testing.T) {
	s, _ := testStore(t)
	createRun(t, s, "observe")
	captureRun(t, s, "observe", captureBody(`{"target_id":"web","lock_key":"vm:web","original_state":"running"}`))
	payload := json.RawMessage(`{"health":"green"}`)
	audit := actor("operator", "observation-1")
	if err := s.AddObservation(testContext, "observe", "web", payload, exact(1), audit); err != nil {
		t.Fatal(err)
	}
	failed := transitionMaintenance(t, s, "observe", "failed", 2)
	if err := s.AddObservation(testContext, "observe", "web", payload, exact(1), audit); err != nil {
		t.Fatalf("lost-response replay = %v", err)
	}
	if err := s.AddObservation(testContext, "observe", "web", json.RawMessage(`{"health":"red"}`), exact(1), audit); !errors.Is(err, ErrConflict) {
		t.Fatalf("request reuse = %v", err)
	}
	if err := s.AddObservation(testContext, "observe", "web", payload, exact(1), actor("operator", "fresh-terminal-stale")); !errors.Is(err, ErrConflict) || errors.Is(err, ErrVersionConflict) {
		t.Fatalf("terminal parent must win over stale observation = %v", err)
	}
	current, _ := s.GetTarget(testContext, "observe", "web")
	if err := s.AddObservation(testContext, "observe", "web", payload, exact(current.Version), actor("operator", "fresh-terminal")); !errors.Is(err, ErrConflict) {
		t.Fatalf("terminal observation = %v", err)
	}
	if _, err := s.TransitionMaintenance(testContext, "observe", StateChange{State: "captured", Justification: "resume immutable capture"}, exact(failed.Maintenance.Version), actor("admin", "observe-reopen")); err != nil {
		t.Fatal(err)
	}
	if err := s.AddObservation(testContext, "observe", "web", payload, exact(1), actor("operator", "fresh-after-reopen-stale")); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("stale observation after reopen = %v", err)
	}
	current, _ = s.GetTarget(testContext, "observe", "web")
	if err := s.AddObservation(testContext, "observe", "web", payload, exact(current.Version), actor("operator", "fresh-after-reopen")); err != nil {
		t.Fatalf("current observation after reopen = %v", err)
	}
}

func TestEventFeedOrderingFilteringAndPagination(t *testing.T) {
	s, _ := testStore(t)
	createRun(t, s, "events")
	captureRun(t, s, "events", captureBody(`{"target_id":"web","lock_key":"vm:web","original_state":"running"}`))
	transitionMaintenance(t, s, "events", "stopping", 2)
	transitionTarget(t, s, "events", "web", "stopping", 2)
	page, err := s.ListEvents(testContext, EventListOptions{MaintenanceID: "events", Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 || page.NextCursor == "" || page.Items[0].ID <= page.Items[1].ID {
		t.Fatalf("page = %+v", page)
	}
	next, err := s.ListEvents(testContext, EventListOptions{MaintenanceID: "events", Cursor: page.NextCursor, Limit: 2})
	if err != nil || len(next.Items) != 2 {
		t.Fatalf("next = %+v, %v", next, err)
	}
	targets, err := s.ListEvents(testContext, EventListOptions{MaintenanceID: "events", TargetID: "web", EventType: "target.transitioned"})
	if err != nil || len(targets.Items) != 1 || targets.Items[0].AggregateVersion != 4 {
		t.Fatalf("target events = %+v, %v", targets, err)
	}
	for name, opts := range map[string]EventListOptions{
		"resource type": {ResourceType: "target"},
		"resource id":   {ResourceID: "web"},
		"actor":         {Actor: "test-operator"},
		"role":          {Role: "operator"},
		"request id":    {RequestID: "events-web-stopping"},
	} {
		t.Run(name, func(t *testing.T) {
			filtered, err := s.ListEvents(testContext, opts)
			if err != nil || len(filtered.Items) == 0 {
				t.Fatalf("filtered events = %+v, %v", filtered, err)
			}
		})
	}
}

func TestCompletionAndTerminalParentGuard(t *testing.T) {
	s, path := testStore(t)
	createRun(t, s, "complete")
	captureRun(t, s, "complete", captureBody(`{"target_id":"web","lock_key":"vm:web","original_state":"running"}`))
	transitionMaintenance(t, s, "complete", "stopping", 2)
	transitionTarget(t, s, "complete", "web", "stopping", 2)
	stopped := transitionTarget(t, s, "complete", "web", "stopped", 3)
	aggregate := transitionMaintenance(t, s, "complete", "stopped", stopped.MaintenanceVersion)
	aggregate = transitionMaintenance(t, s, "complete", "waiting", aggregate.Maintenance.Version)
	aggregate = transitionMaintenance(t, s, "complete", "starting", aggregate.Maintenance.Version)
	if _, err := s.TransitionMaintenance(testContext, "complete", StateChange{State: "completed"}, exact(aggregate.Maintenance.Version), actor("operator", "premature-complete")); !errors.Is(err, ErrConflict) {
		t.Fatalf("unreconciled completion = %v", err)
	}
	web, _ := s.GetTarget(testContext, "complete", "web")
	transitionTarget(t, s, "complete", "web", "starting", web.Version)
	web, _ = s.GetTarget(testContext, "complete", "web")
	healthy := transitionTarget(t, s, "complete", "web", "healthy", web.Version)
	aggregate, err := s.TransitionMaintenance(testContext, "complete", StateChange{State: "completed"}, exact(healthy.MaintenanceVersion), actor("operator", "complete"))
	if err != nil {
		t.Fatal(err)
	}
	if aggregate.Maintenance.State != "completed" {
		t.Fatalf("state = %s", aggregate.Maintenance.State)
	}
	web, _ = s.GetTarget(testContext, "complete", "web")
	if _, err := s.TransitionTarget(testContext, "complete", "web", StateChange{State: "failed"}, exact(web.Version), actor("operator", "late")); !errors.Is(err, ErrConflict) {
		t.Fatalf("late target = %v", err)
	}
	if _, err := VerifyPath(testContext, path); err != nil {
		t.Fatalf("verify after rejected late target: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(testContext, path)
	if err != nil {
		t.Fatalf("reopen after rejected late target: %v", err)
	}
	_ = reopened.Close()
}

func TestDeepVerifyReplaysHistoryAndImmutableEvidence(t *testing.T) {
	s, path := testStore(t)
	createRun(t, s, "verify")
	captureRun(t, s, "verify", captureBody(`{"target_id":"web","lock_key":"vm:web","kind":"vm","host":"web","original_state":"running","metadata":{"a":1}}`))
	transitionMaintenance(t, s, "verify", "stopping", 2)
	if _, err := VerifyPath(testContext, path); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`DELETE FROM targets WHERE maintenance_id='verify' AND id='web'`); err == nil {
		t.Fatal("target delete should be blocked")
	}
	if _, err := s.DB().Exec(`UPDATE targets SET state='failed' WHERE maintenance_id='verify' AND id='web'`); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyPath(testContext, path); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("projection corruption = %v", err)
	}
}

func TestDeepVerifyRejectsCorruptEvidenceIdentityAndScope(t *testing.T) {
	for name, tc := range map[string]struct {
		trigger string
		update  string
	}{
		"capture hash":   {"maintenance_captures_immutable_update", `UPDATE maintenance_captures SET sha256='bad' WHERE maintenance_id='corrupt'`},
		"event identity": {"journal_events_append_only_update", `UPDATE journal_events SET request_id='' WHERE maintenance_id='corrupt' AND event_type='maintenance.created'`},
		"event scope":    {"journal_events_append_only_update", `UPDATE journal_events SET resource_id='wrong' WHERE maintenance_id='corrupt' AND event_type='maintenance.created'`},
	} {
		t.Run(name, func(t *testing.T) {
			s, path := testStore(t)
			createRun(t, s, "corrupt")
			captureRun(t, s, "corrupt", captureBody(`{"target_id":"web","lock_key":"vm:web","original_state":"running"}`))
			var triggerSQL string
			if err := s.DB().QueryRow(`SELECT sql FROM sqlite_master WHERE type='trigger' AND name=?`, tc.trigger).Scan(&triggerSQL); err != nil {
				t.Fatal(err)
			}
			if _, err := s.DB().Exec(`DROP TRIGGER ` + tc.trigger); err != nil {
				t.Fatal(err)
			}
			if _, err := s.DB().Exec(tc.update); err != nil {
				t.Fatal(err)
			}
			if _, err := s.DB().Exec(triggerSQL); err != nil {
				t.Fatal(err)
			}
			if _, err := VerifyPath(testContext, path); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("corruption accepted: %v", err)
			}
		})
	}
}

func TestBackupManifestReplayAndRetention(t *testing.T) {
	s, _ := testStore(t)
	createRun(t, s, "backup")
	dir := t.TempDir()
	manifest, err := s.BackupWithRetention(testContext, dir, 2, 0, "admin", "admin", "backup-request")
	if err != nil {
		t.Fatal(err)
	}
	if !manifest.AuditRecorded || manifest.SHA256 == "" {
		t.Fatalf("manifest = %+v", manifest)
	}
	replay, err := s.BackupWithRetention(testContext, dir, 2, 0, "admin", "admin", "backup-request")
	if err != nil || replay.Filename != manifest.Filename {
		t.Fatalf("replay = %+v, %v", replay, err)
	}
	path, validated, err := BackupArtifact(testContext, dir, manifest.Filename)
	if err != nil || path == "" || validated.SHA256 != manifest.SHA256 {
		t.Fatalf("artifact = %s %+v %v", path, validated, err)
	}
}

func TestBackupAuditFailureAndIndependentServiceConnection(t *testing.T) {
	t.Run("published backup survives audit failure", func(t *testing.T) {
		s, _ := testStore(t)
		createRun(t, s, "audit-failure")
		if _, err := s.DB().Exec(`CREATE TRIGGER reject_backup_audit BEFORE INSERT ON journal_events WHEN NEW.event_type='backup.created' BEGIN SELECT RAISE(ABORT, 'forced audit failure'); END`); err != nil {
			t.Fatal(err)
		}
		dir := t.TempDir()
		manifest, err := s.Backup(testContext, dir, 2, "admin", "admin", "audit-failure-backup")
		if err != nil || manifest.AuditRecorded {
			t.Fatalf("backup = %+v, %v", manifest, err)
		}
		if _, verified, err := BackupArtifact(testContext, dir, manifest.Filename); err != nil || verified.SHA256 != manifest.SHA256 {
			t.Fatalf("published artifact = %+v, %v", verified, err)
		}
	})

	t.Run("backup leaves readiness and writes available", func(t *testing.T) {
		s, _ := testStore(t)
		createRun(t, s, "before-backup")
		entered, release := make(chan struct{}), make(chan struct{})
		var calls atomic.Int64
		now := time.Now().UTC()
		s.now = func() time.Time {
			if calls.Add(1) == 1 {
				close(entered)
				<-release
			}
			return now
		}
		dir := t.TempDir()
		backupDone := make(chan error, 1)
		go func() {
			_, err := s.Backup(testContext, dir, 2, "admin", "admin", "concurrent-backup")
			backupDone <- err
		}()
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			close(release)
			<-backupDone
			t.Fatal("backup did not reach independent-connection phase")
		}
		readyErr := s.Ready(testContext)
		_, writeErr := s.CreateMaintenance(testContext, Maintenance{ID: "during-backup"}, actor("operator", "during-backup-create"))
		close(release)
		backupErr := <-backupDone
		if readyErr != nil || writeErr != nil || backupErr != nil {
			t.Fatalf("ready=%v write=%v backup=%v", readyErr, writeErr, backupErr)
		}
	})
}
