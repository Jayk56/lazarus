package api

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/jayk56/lazarus/internal/store"
)

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	s.writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "version": s.version})
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	if !s.ready.Load() || s.store == nil || s.store.Ready(r.Context()) != nil {
		s.writeError(w, http.StatusServiceUnavailable, "not_ready")
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"status": "ready", "version": s.version})
}

func (s *Server) handleVersion(w http.ResponseWriter, _ *http.Request) {
	s.writeJSON(w, http.StatusOK, map[string]any{"version": s.version})
}

func (s *Server) handleMetrics(w http.ResponseWriter, _ *http.Request) { s.metrics(w) }

func (s *Server) handleListMaintenances(w http.ResponseWriter, r *http.Request) {
	opts, err := maintenanceListOptions(r)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	page, err := s.store.ListMaintenances(r.Context(), opts)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, page)
}

func (s *Server) handleCreateMaintenance(w http.ResponseWriter, r *http.Request) {
	var request maintenanceRequest
	if err := decodeJSON(r, &request); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid_json")
		return
	}
	if strings.TrimSpace(request.MaintenanceID) == "" {
		s.writeError(w, http.StatusBadRequest, "maintenance_id_required")
		return
	}
	if len(request.Metadata) > 0 && !isJSONObject(request.Metadata) {
		s.writeError(w, http.StatusBadRequest, "metadata_must_be_object")
		return
	}
	maintenance, err := s.store.CreateMaintenance(r.Context(), store.Maintenance{
		ID: request.MaintenanceID, State: "new", ChangeTicket: request.ChangeTicket,
		WorkflowVersion: request.WorkflowVersion, Metadata: request.Metadata,
	}, auditContext(r))
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	aggregate := store.MaintenanceAggregate{Maintenance: maintenance, Targets: []store.Target{}}
	w.Header().Set("ETag", store.VersionETag(maintenance.Version))
	s.writeJSON(w, http.StatusCreated, aggregate)
}

func (s *Server) handleGetMaintenance(w http.ResponseWriter, r *http.Request) {
	aggregate, err := s.store.GetMaintenanceAggregate(r.Context(), r.PathValue("maintenanceID"))
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	w.Header().Set("ETag", store.VersionETag(aggregate.Maintenance.Version))
	s.writeJSON(w, http.StatusOK, aggregate)
}

func (s *Server) handleTransitionMaintenance(w http.ResponseWriter, r *http.Request) {
	var request stateChangeRequest
	if err := decodeJSON(r, &request); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid_json")
		return
	}
	if len(request.Detail) > 0 && !isJSONObject(request.Detail) {
		s.writeError(w, http.StatusBadRequest, "detail_must_be_object")
		return
	}
	precondition, code := versionPrecondition(r)
	if code != "" {
		s.writeIfMatchError(w, code)
		return
	}
	aggregate, err := s.store.TransitionMaintenance(r.Context(), r.PathValue("maintenanceID"), request.storeValue(), precondition, auditContext(r))
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	w.Header().Set("ETag", store.VersionETag(aggregate.Maintenance.Version))
	s.writeJSON(w, http.StatusOK, aggregate)
}

func (s *Server) handleGetCapture(w http.ResponseWriter, r *http.Request) {
	capture, err := s.store.GetCapture(r.Context(), r.PathValue("maintenanceID"))
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, capture)
}

func (s *Server) handleCaptureMaintenance(w http.ResponseWriter, r *http.Request) {
	payload, err := decodeJSONObject(r, 2<<20)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid_json")
		return
	}
	result, err := s.store.CaptureMaintenance(r.Context(), r.PathValue("maintenanceID"), store.Capture{Payload: payload}, auditContext(r))
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	status := http.StatusOK
	if result.Created {
		status = http.StatusCreated
	}
	s.writeJSON(w, status, result.Capture)
}

func (s *Server) handleGetTarget(w http.ResponseWriter, r *http.Request) {
	target, err := s.store.GetTarget(r.Context(), r.PathValue("maintenanceID"), r.PathValue("targetID"))
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	w.Header().Set("ETag", store.VersionETag(target.Version))
	s.writeJSON(w, http.StatusOK, target)
}

func (s *Server) handleTransitionTarget(w http.ResponseWriter, r *http.Request) {
	var request stateChangeRequest
	if err := decodeJSON(r, &request); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid_json")
		return
	}
	if len(request.Detail) > 0 && !isJSONObject(request.Detail) {
		s.writeError(w, http.StatusBadRequest, "detail_must_be_object")
		return
	}
	precondition, code := versionPrecondition(r)
	if code != "" {
		s.writeIfMatchError(w, code)
		return
	}
	result, err := s.store.TransitionTarget(
		r.Context(), r.PathValue("maintenanceID"), r.PathValue("targetID"),
		request.storeValue(), precondition, auditContext(r),
	)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	w.Header().Set("ETag", store.VersionETag(result.Target.Version))
	w.Header().Set("X-Maintenance-ETag", store.VersionETag(result.MaintenanceVersion))
	s.writeJSON(w, http.StatusOK, result.Target)
}

func (s *Server) handleAddTargetObservation(w http.ResponseWriter, r *http.Request) {
	payload, err := decodeJSONObject(r, 64<<10)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid_json")
		return
	}
	precondition, code := versionPrecondition(r)
	if code != "" {
		s.writeIfMatchError(w, code)
		return
	}
	if err := s.store.AddObservation(
		r.Context(), r.PathValue("maintenanceID"), r.PathValue("targetID"),
		payload, precondition, auditContext(r),
	); err != nil {
		s.writeStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListEvents(w http.ResponseWriter, r *http.Request) {
	opts, err := eventListOptions(r)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	page, err := s.store.ListEvents(r.Context(), opts)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, page)
}

func (s *Server) handleCreateBackup(w http.ResponseWriter, r *http.Request) {
	if strings.TrimSpace(s.backupDir) == "" {
		s.writeError(w, http.StatusServiceUnavailable, "backup_unavailable")
		return
	}
	audit := auditContext(r)
	manifest, err := s.store.BackupWithRetention(r.Context(), s.backupDir, s.backupKeep, s.backupMinAge, audit.Actor, audit.Role, audit.RequestID)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	s.writeJSON(w, http.StatusCreated, manifest)
}

func (s *Server) handleBackupArtifact(w http.ResponseWriter, r *http.Request) {
	filename := r.PathValue("filename")
	if strings.TrimSpace(s.backupDir) == "" {
		s.writeError(w, http.StatusServiceUnavailable, "backup_unavailable")
		return
	}
	_, manifest, err := store.BackupArtifact(r.Context(), s.backupDir, filename)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	root, err := os.OpenRoot(s.backupDir)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "internal_error")
		return
	}
	defer root.Close()
	file, err := root.Open(filename)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "internal_error")
		return
	}
	defer file.Close()
	if err := store.ValidateOpenedBackupArtifact(file, filename, manifest); err != nil {
		s.writeStoreError(w, err)
		return
	}
	if err := s.store.RecordBackupDownload(r.Context(), manifest, filename, auditContext(r)); err != nil {
		s.writeStoreError(w, err)
		return
	}
	info, err := file.Stat()
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "internal_error")
		return
	}
	if strings.HasSuffix(filename, ".json") {
		w.Header().Set("Content-Type", "application/json")
	} else {
		w.Header().Set("Content-Type", "application/vnd.sqlite3")
		w.Header().Set("ETag", `"`+manifest.SHA256+`"`)
		if digest, decodeErr := hex.DecodeString(manifest.SHA256); decodeErr == nil {
			w.Header().Set("Digest", "sha-256="+base64.StdEncoding.EncodeToString(digest))
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
	http.ServeContent(w, r, filename, info.ModTime(), file)
}
