package gateway

import (
	"encoding/json"
	"net/http"
	"strings"
)

// handleAdminPool reports the egress pool's per-egress window accounting and
// the newest request fingerprints: the evidence trail behind client-visible
// 429s (which egress served, how much its window had already served, and what
// the failing request looked like).
func (s *Server) handleAdminPool(w http.ResponseWriter, r *http.Request) {
	admin, _, aerr := s.authenticate(r)
	if aerr != nil {
		writeErr(w, aerr.Status, aerr.Code, aerr.Type, aerr.Message)
		return
	}
	if !admin {
		writeErr(w, http.StatusForbidden, "forbidden", "permission_error", "admin token required")
		return
	}
	var pool map[string]any
	if s.router != nil {
		pool = s.router.PoolDiag()
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"pool":         pool,
		"recent_calls": RecentRequests(40),
	})
}

// handleAdminPoolRotate asks the warp-router pool for a fresh identity
// generation (all pooled providers, or ?provider=<name>).
func (s *Server) handleAdminPoolRotate(w http.ResponseWriter, r *http.Request) {
	admin, _, aerr := s.authenticate(r)
	if aerr != nil {
		writeErr(w, aerr.Status, aerr.Code, aerr.Type, aerr.Message)
		return
	}
	if !admin {
		writeErr(w, http.StatusForbidden, "forbidden", "permission_error", "admin token required")
		return
	}
	provider := strings.TrimSpace(r.URL.Query().Get("provider"))
	rotated := false
	if s.router != nil {
		rotated = s.router.ForcePoolRotate(provider)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"rotated": rotated, "provider": provider})
}
