package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/jayk56/lazarus/internal/auth"
)

func (s *Server) buildRoutes() http.Handler {
	mux := http.NewServeMux()
	for _, path := range []string{"/healthz", "/livez", "/startupz"} {
		mux.HandleFunc("GET "+path, s.handleHealth)
	}
	mux.HandleFunc("GET /readyz", s.handleReady)
	mux.HandleFunc("GET /version", s.handleVersion)
	mux.HandleFunc("GET /metrics", s.handleMetrics)

	mux.HandleFunc("GET /api/v1/maintenance", s.require(auth.RoleReader, s.handleListMaintenances))
	mux.HandleFunc("POST /api/v1/maintenance", s.require(auth.RoleOperator, s.handleCreateMaintenance))
	mux.HandleFunc("GET /api/v1/maintenance/{maintenanceID}", s.require(auth.RoleReader, s.handleGetMaintenance))
	mux.HandleFunc("PATCH /api/v1/maintenance/{maintenanceID}", s.require(auth.RoleOperator, s.handleTransitionMaintenance))
	mux.HandleFunc("GET /api/v1/maintenance/{maintenanceID}/capture", s.require(auth.RoleReader, s.handleGetCapture))
	mux.HandleFunc("PUT /api/v1/maintenance/{maintenanceID}/capture", s.require(auth.RoleOperator, s.handleCaptureMaintenance))
	mux.HandleFunc("GET /api/v1/maintenance/{maintenanceID}/targets/{targetID}", s.require(auth.RoleReader, s.handleGetTarget))
	mux.HandleFunc("PATCH /api/v1/maintenance/{maintenanceID}/targets/{targetID}", s.require(auth.RoleOperator, s.handleTransitionTarget))
	mux.HandleFunc("POST /api/v1/maintenance/{maintenanceID}/targets/{targetID}/observations", s.require(auth.RoleOperator, s.handleAddTargetObservation))
	mux.HandleFunc("GET /api/v1/events", s.require(auth.RoleReader, s.handleListEvents))
	mux.HandleFunc("POST /api/v1/admin/backups", s.require(auth.RoleAdmin, s.handleCreateBackup))
	mux.HandleFunc("GET /api/v1/admin/backups/{filename}", s.require(auth.RoleAdmin, s.handleBackupArtifact))
	mux.HandleFunc("HEAD /api/v1/admin/backups/{filename}", s.require(auth.RoleAdmin, s.handleBackupArtifact))
	return mux
}

func isAPIPath(path string) bool {
	return path == "/api/v1" || strings.HasPrefix(path, "/api/v1/")
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	if !isAPIPath(r.URL.Path) {
		s.routes.ServeHTTP(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if s.auth == nil {
		s.writeError(w, http.StatusServiceUnavailable, "authentication_unavailable")
		return
	}
	principal, err := s.auth.Authenticate(r)
	if err != nil {
		w.Header().Set("WWW-Authenticate", `Bearer realm="lazarus"`)
		s.writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if s.store == nil {
		s.writeError(w, http.StatusServiceUnavailable, "store_unavailable")
		return
	}
	if !s.ready.Load() && r.Method != http.MethodGet && r.Method != http.MethodHead {
		s.writeError(w, http.StatusServiceUnavailable, "service_draining")
		return
	}
	r = r.WithContext(context.WithValue(r.Context(), principalKey{}, principal))
	s.routes.ServeHTTP(w, r)
}

func (s *Server) require(role auth.Role, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		principal, ok := r.Context().Value(principalKey{}).(auth.Principal)
		if !ok {
			s.writeError(w, http.StatusServiceUnavailable, "authentication_unavailable")
			return
		}
		if auth.Require(principal, role) != nil {
			s.writeError(w, http.StatusForbidden, "forbidden")
			return
		}
		next(w, r)
	}
}
