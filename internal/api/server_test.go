package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jayk56/lazarus/internal/auth"
	"github.com/jayk56/lazarus/internal/store"
)

func testServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	database, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "lazarus.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	authenticator, err := auth.New(map[string]auth.Role{
		"reader-secret": auth.RoleReader, "operator-secret": auth.RoleOperator, "admin-secret": auth.RoleAdmin,
	})
	if err != nil {
		t.Fatal(err)
	}
	return New(Config{Store: database, Auth: authenticator, Version: "test"}), database
}

func doJSON(t *testing.T, handler http.Handler, method, path, token string, body any, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var payload []byte
	if body != nil {
		payload, _ = json.Marshal(body)
	}
	request := httptest.NewRequest(method, path, bytes.NewReader(payload))
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func requireStatus(t *testing.T, response *httptest.ResponseRecorder, status int) {
	t.Helper()
	if response.Code != status {
		t.Fatalf("status=%d want=%d body=%s", response.Code, status, response.Body.String())
	}
}

func resourceETag(t *testing.T, handler http.Handler, path, token string) string {
	t.Helper()
	response := doJSON(t, handler, http.MethodGet, path, token, nil, nil)
	requireStatus(t, response, http.StatusOK)
	if response.Header().Get("ETag") == "" {
		t.Fatalf("missing ETag for %s", path)
	}
	return response.Header().Get("ETag")
}

func createMaintenance(t *testing.T, handler http.Handler, id string) string {
	t.Helper()
	response := doJSON(t, handler, http.MethodPost, "/api/v1/maintenance", "operator-secret", map[string]any{"maintenance_id": id}, nil)
	requireStatus(t, response, http.StatusCreated)
	return response.Header().Get("ETag")
}

func captureMaintenance(t *testing.T, handler http.Handler, id string, targets ...map[string]any) {
	t.Helper()
	response := doJSON(t, handler, http.MethodPut, "/api/v1/maintenance/"+id+"/capture", "operator-secret", map[string]any{"targets": targets}, nil)
	requireStatus(t, response, http.StatusCreated)
}

func TestHTTPProtocolContract(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()

	for _, path := range []string{"/healthz", "/livez", "/startupz", "/readyz", "/version", "/metrics"} {
		response := doJSON(t, handler, http.MethodGet, path, "", nil, map[string]string{"X-Request-ID": "probe"})
		requireStatus(t, response, http.StatusOK)
		if response.Header().Get("X-Request-ID") != "probe" || response.Header().Get("Cache-Control") != "" {
			t.Fatalf("probe headers for %s: %#v", path, response.Header())
		}
	}

	unauthorized := doJSON(t, handler, http.MethodGet, "/api/v1/maintenance", "", nil, map[string]string{"X-Request-ID": "unauthorized"})
	requireStatus(t, unauthorized, http.StatusUnauthorized)
	if unauthorized.Header().Get("Cache-Control") != "no-store" || unauthorized.Header().Get("WWW-Authenticate") == "" || unauthorized.Header().Get("X-Request-ID") != "unauthorized" {
		t.Fatalf("unauthorized headers: %#v", unauthorized.Header())
	}
	if response := doJSON(t, handler, http.MethodPost, "/api/v1/maintenance", "operator-secret", map[string]any{"id": "old-field"}, nil); response.Code != http.StatusBadRequest {
		t.Fatalf("unknown field accepted: %d %s", response.Code, response.Body.String())
	}
	createMaintenance(t, handler, "protocol")
	requireStatus(t, doJSON(t, handler, http.MethodGet, "/api/v1/maintenance", "reader-secret", nil, nil), http.StatusOK)
	requireStatus(t, doJSON(t, handler, http.MethodPost, "/api/v1/admin/backups", "reader-secret", nil, nil), http.StatusForbidden)
	requireStatus(t, doJSON(t, handler, http.MethodGet, "/api/v1/not-a-route", "operator-secret", nil, nil), http.StatusNotFound)
	for _, tc := range []struct {
		path, method, token string
		allow               []string
	}{
		{"/healthz", http.MethodPost, "", []string{"GET"}},
		{"/livez", http.MethodPost, "", []string{"GET"}},
		{"/startupz", http.MethodPost, "", []string{"GET"}},
		{"/readyz", http.MethodPost, "", []string{"GET"}},
		{"/version", http.MethodPost, "", []string{"GET"}},
		{"/metrics", http.MethodPost, "", []string{"GET"}},
		{"/api/v1/maintenance", http.MethodDelete, "operator-secret", []string{"GET", "POST"}},
		{"/api/v1/maintenance/protocol", http.MethodDelete, "operator-secret", []string{"GET", "PATCH"}},
		{"/api/v1/maintenance/protocol/capture", http.MethodPost, "operator-secret", []string{"GET", "PUT"}},
		{"/api/v1/maintenance/protocol/targets/missing", http.MethodDelete, "operator-secret", []string{"GET", "PATCH"}},
		{"/api/v1/maintenance/protocol/targets/missing/observations", http.MethodGet, "operator-secret", []string{"POST"}},
		{"/api/v1/events", http.MethodPost, "operator-secret", []string{"GET"}},
		{"/api/v1/admin/backups", http.MethodGet, "admin-secret", []string{"POST"}},
		{"/api/v1/admin/backups/missing.db", http.MethodPut, "admin-secret", []string{"GET", "HEAD"}},
	} {
		response := doJSON(t, handler, tc.method, tc.path, tc.token, nil, nil)
		requireStatus(t, response, http.StatusMethodNotAllowed)
		for _, allowed := range tc.allow {
			if !strings.Contains(response.Header().Get("Allow"), allowed) {
				t.Fatalf("%s %s Allow=%q", tc.method, tc.path, response.Header().Get("Allow"))
			}
		}
	}
	requireStatus(t, doJSON(t, handler, http.MethodGet, "/api/v1/maintenance?status=new", "reader-secret", nil, nil), http.StatusBadRequest)
	createMaintenance(t, handler, "lock-owner")
	captureMaintenance(t, handler, "lock-owner", map[string]any{"target_id": "owned", "lock_key": "vm/shared", "original_state": "running"})
	createMaintenance(t, handler, "lock-contender")
	lockConflict := doJSON(t, handler, http.MethodPut, "/api/v1/maintenance/lock-contender/capture", "operator-secret", map[string]any{
		"targets": []any{map[string]any{"target_id": "contender", "lock_key": "vm/shared", "original_state": "running"}},
	}, nil)
	requireStatus(t, lockConflict, http.StatusConflict)
	if !strings.Contains(lockConflict.Body.String(), `"holder":"lock-owner"`) {
		t.Fatalf("lock diagnostic=%s", lockConflict.Body.String())
	}

	server.SetReady(false)
	draining := doJSON(t, handler, http.MethodPost, "/api/v1/maintenance", "operator-secret", map[string]any{"maintenance_id": "blocked"}, nil)
	requireStatus(t, draining, http.StatusServiceUnavailable)
	if !strings.Contains(draining.Body.String(), "service_draining") {
		t.Fatalf("drain response=%s", draining.Body.String())
	}
	requireStatus(t, doJSON(t, handler, http.MethodGet, "/api/v1/maintenance", "reader-secret", nil, nil), http.StatusOK)
	requireStatus(t, doJSON(t, handler, http.MethodGet, "/readyz", "", nil, nil), http.StatusServiceUnavailable)
}

func TestHTTPConditionalWritesAndObservations(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	createMaintenance(t, handler, "conditional")
	capture := map[string]any{
		"targets": []any{map[string]any{"target_id": "web", "lock_key": "vm/web", "original_state": "running"}},
	}
	first := doJSON(t, handler, http.MethodPut, "/api/v1/maintenance/conditional/capture", "operator-secret", capture, nil)
	requireStatus(t, first, http.StatusCreated)
	requireStatus(t, doJSON(t, handler, http.MethodPut, "/api/v1/maintenance/conditional/capture", "operator-secret", capture, nil), http.StatusOK)
	requireStatus(t, doJSON(t, handler, http.MethodPut, "/api/v1/maintenance/conditional/capture", "operator-secret", map[string]any{"targets": []any{}}, nil), http.StatusConflict)

	maintenancePath := "/api/v1/maintenance/conditional"
	targetPath := maintenancePath + "/targets/web"
	if etag := resourceETag(t, handler, maintenancePath, "reader-secret"); etag != `"2"` {
		t.Fatalf("maintenance ETag=%q", etag)
	}
	requireStatus(t, doJSON(t, handler, http.MethodPatch, maintenancePath, "operator-secret", map[string]any{"state": "stopping"}, nil), http.StatusPreconditionRequired)
	requireStatus(t, doJSON(t, handler, http.MethodPatch, maintenancePath, "operator-secret", map[string]any{"state": "stopping"}, map[string]string{"If-Match": `v2`}), http.StatusBadRequest)
	requireStatus(t, doJSON(t, handler, http.MethodPatch, maintenancePath, "operator-secret", map[string]any{"state": "stopping"}, map[string]string{"If-Match": `W/"2"`}), http.StatusPreconditionFailed)

	maintenance := doJSON(t, handler, http.MethodPatch, maintenancePath, "operator-secret", map[string]any{"state": "stopping"}, map[string]string{"If-Match": `"opaque,tag", "2"`})
	requireStatus(t, maintenance, http.StatusOK)
	if maintenance.Header().Get("ETag") != `"3"` || resourceETag(t, handler, targetPath, "reader-secret") != `"2"` {
		t.Fatalf("maintenance transition did not invalidate child ETags")
	}
	target := doJSON(t, handler, http.MethodPatch, targetPath, "operator-secret", map[string]any{"state": "stopping"}, map[string]string{"If-Match": `"999", "2"`})
	requireStatus(t, target, http.StatusOK)
	if target.Header().Get("ETag") != `"3"` || target.Header().Get("X-Maintenance-ETag") != `"4"` {
		t.Fatalf("target headers=%#v", target.Header())
	}
	requireStatus(t, doJSON(t, handler, http.MethodPatch, maintenancePath, "operator-secret", map[string]any{"state": "stopped"}, map[string]string{"If-Match": `"3"`}), http.StatusPreconditionFailed)

	observationPath := targetPath + "/observations"
	requireStatus(t, doJSON(t, handler, http.MethodPost, observationPath, "operator-secret", map[string]any{"healthy": false}, nil), http.StatusPreconditionRequired)
	observationHeaders := map[string]string{"If-Match": `"3"`, "X-Request-ID": "observation-1"}
	requireStatus(t, doJSON(t, handler, http.MethodPost, observationPath, "operator-secret", map[string]any{"healthy": false}, observationHeaders), http.StatusNoContent)
	requireStatus(t, doJSON(t, handler, http.MethodPatch, targetPath, "operator-secret", map[string]any{"state": "stopped"}, map[string]string{"If-Match": `"3"`}), http.StatusOK)
	// A lost-response replay wins before the now-stale version check.
	requireStatus(t, doJSON(t, handler, http.MethodPost, observationPath, "operator-secret", map[string]any{"healthy": false}, observationHeaders), http.StatusNoContent)
	requireStatus(t, doJSON(t, handler, http.MethodPost, observationPath, "operator-secret", map[string]any{"healthy": true}, observationHeaders), http.StatusConflict)
	maintenance = doJSON(t, handler, http.MethodPatch, maintenancePath, "operator-secret", map[string]any{"state": "stopped"}, map[string]string{"If-Match": `*`})
	requireStatus(t, maintenance, http.StatusOK)
	if maintenance.Header().Get("ETag") != `"6"` || resourceETag(t, handler, targetPath, "reader-secret") != `"5"` {
		t.Fatalf("second maintenance transition did not invalidate child ETags")
	}
}

func TestHTTPRecoveryPolicyAndTerminalGuard(t *testing.T) {
	server, _ := testServer(t)
	handler := server.Handler()
	createMaintenance(t, handler, "recovery")
	captureMaintenance(t, handler, "recovery", map[string]any{
		"target_id": "unknown", "lock_key": "vm/unknown", "original_state": "unknown",
	})
	maintenancePath := "/api/v1/maintenance/recovery"
	targetPath := maintenancePath + "/targets/unknown"
	observationPath := targetPath + "/observations"
	staleTag := resourceETag(t, handler, targetPath, "reader-secret")
	replayHeaders := map[string]string{"If-Match": staleTag, "X-Request-ID": "recovery-observation"}
	requireStatus(t, doJSON(t, handler, http.MethodPost, observationPath, "operator-secret", map[string]any{"state": "captured"}, replayHeaders), http.StatusNoContent)
	requireStatus(t, doJSON(t, handler, http.MethodPatch, maintenancePath, "operator-secret", map[string]any{"state": "failed"}, map[string]string{"If-Match": `"2"`}), http.StatusOK)
	// Exact replay is checked before terminal state and stale-version checks.
	requireStatus(t, doJSON(t, handler, http.MethodPost, observationPath, "operator-secret", map[string]any{"state": "captured"}, replayHeaders), http.StatusNoContent)
	requireStatus(t, doJSON(t, handler, http.MethodPost, observationPath, "operator-secret", map[string]any{"state": "late"}, map[string]string{"If-Match": staleTag, "X-Request-ID": "terminal-stale"}), http.StatusConflict)
	requireStatus(t, doJSON(t, handler, http.MethodPatch, maintenancePath, "operator-secret", map[string]any{"state": "captured", "justification": "retry"}, map[string]string{"If-Match": `"3"`}), http.StatusForbidden)
	requireStatus(t, doJSON(t, handler, http.MethodPatch, maintenancePath, "admin-secret", map[string]any{"state": "captured"}, map[string]string{"If-Match": `"3"`}), http.StatusBadRequest)
	requireStatus(t, doJSON(t, handler, http.MethodPatch, maintenancePath, "admin-secret", map[string]any{"state": "captured", "justification": "retry from immutable capture"}, map[string]string{"If-Match": `"3"`}), http.StatusOK)
	requireStatus(t, doJSON(t, handler, http.MethodPost, observationPath, "operator-secret", map[string]any{"state": "after-reopen"}, map[string]string{"If-Match": staleTag, "X-Request-ID": "reopen-stale-observation"}), http.StatusPreconditionFailed)
	requireStatus(t, doJSON(t, handler, http.MethodPatch, targetPath, "operator-secret", map[string]any{"state": "stopping"}, map[string]string{"If-Match": staleTag}), http.StatusPreconditionFailed)

	targetTag := resourceETag(t, handler, targetPath, "reader-secret")
	requireStatus(t, doJSON(t, handler, http.MethodPatch, targetPath, "operator-secret", map[string]any{"state": "skipped", "justification": "owner waiver"}, map[string]string{"If-Match": targetTag}), http.StatusForbidden)
	requireStatus(t, doJSON(t, handler, http.MethodPatch, targetPath, "admin-secret", map[string]any{"state": "skipped", "justification": "owner approved irrecoverable target"}, map[string]string{"If-Match": targetTag}), http.StatusOK)

	for _, state := range []string{"stopping", "stopped", "waiting", "starting", "completed"} {
		response := doJSON(t, handler, http.MethodPatch, maintenancePath, "operator-secret", map[string]any{"state": state}, map[string]string{"If-Match": resourceETag(t, handler, maintenancePath, "reader-secret")})
		requireStatus(t, response, http.StatusOK)
	}
	currentTargetTag := resourceETag(t, handler, targetPath, "reader-secret")
	terminalObservation := doJSON(t, handler, http.MethodPost, targetPath+"/observations", "operator-secret", map[string]any{"state": "late"}, map[string]string{"If-Match": currentTargetTag})
	requireStatus(t, terminalObservation, http.StatusConflict)
	if !strings.Contains(terminalObservation.Body.String(), "parent_maintenance_terminal") {
		t.Fatalf("terminal diagnostic=%s", terminalObservation.Body.String())
	}
	requireStatus(t, doJSON(t, handler, http.MethodPatch, targetPath, "admin-secret", map[string]any{"state": "skipped", "justification": "late"}, map[string]string{"If-Match": `*`}), http.StatusConflict)
}

func TestHTTPEventsAndBackupExport(t *testing.T) {
	server, database := testServer(t)
	server.backupDir = filepath.Join(t.TempDir(), "exports")
	server.backupKeep = 3
	handler := server.Handler()
	createMaintenance(t, handler, "records")
	captureMaintenance(t, handler, "records", map[string]any{
		"target_id": "db", "lock_key": "vm/db", "original_state": "running",
	})
	requireStatus(t, doJSON(t, handler, http.MethodGet, "/api/v1/maintenance?state=captured&limit=1", "reader-secret", nil, nil), http.StatusOK)
	events := doJSON(t, handler, http.MethodGet, "/api/v1/events?maintenance_id=records&event_type=maintenance.captured&resource_type=maintenance&resource_id=records&role=operator&limit=1", "reader-secret", nil, nil)
	requireStatus(t, events, http.StatusOK)
	if !strings.Contains(events.Body.String(), `"type":"maintenance.captured"`) {
		t.Fatalf("events=%s", events.Body.String())
	}
	pageResponse := doJSON(t, handler, http.MethodGet, "/api/v1/events?maintenance_id=records&limit=1", "reader-secret", nil, nil)
	requireStatus(t, pageResponse, http.StatusOK)
	var eventPage store.EventPage
	if err := json.Unmarshal(pageResponse.Body.Bytes(), &eventPage); err != nil || len(eventPage.Items) != 1 || eventPage.NextCursor == "" {
		t.Fatalf("event page=%+v err=%v", eventPage, err)
	}
	requireStatus(t, doJSON(t, handler, http.MethodGet, "/api/v1/events?maintenance_id=records&limit=1&cursor="+eventPage.NextCursor, "reader-secret", nil, nil), http.StatusOK)
	requireStatus(t, doJSON(t, handler, http.MethodGet, "/api/v1/events?target_id=db", "reader-secret", nil, nil), http.StatusBadRequest)

	created := doJSON(t, handler, http.MethodPost, "/api/v1/admin/backups", "admin-secret", nil, map[string]string{"X-Request-ID": "backup-1"})
	requireStatus(t, created, http.StatusCreated)
	var manifest store.BackupManifest
	if err := json.Unmarshal(created.Body.Bytes(), &manifest); err != nil || manifest.Filename == "" {
		t.Fatalf("manifest=%#v err=%v", manifest, err)
	}
	downloadPath := "/api/v1/admin/backups/" + manifest.Filename
	download := doJSON(t, handler, http.MethodGet, downloadPath, "admin-secret", nil, nil)
	requireStatus(t, download, http.StatusOK)
	if int64(download.Body.Len()) != manifest.Bytes || download.Header().Get("Digest") == "" || download.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("download bytes=%d headers=%#v", download.Body.Len(), download.Header())
	}
	head := doJSON(t, handler, http.MethodHead, downloadPath, "admin-secret", nil, nil)
	requireStatus(t, head, http.StatusOK)
	if head.Body.Len() != 0 || head.Header().Get("Content-Length") == "" {
		t.Fatalf("HEAD body=%d headers=%#v", head.Body.Len(), head.Header())
	}
	page, err := database.ListEvents(context.Background(), store.EventListOptions{EventType: "backup.downloaded"})
	if err != nil || len(page.Items) != 2 {
		t.Fatalf("download events=%#v err=%v", page.Items, err)
	}
}
