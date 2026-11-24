package gateway

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/ByJotaro/ai-router/internal/route"
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

// handleAdminBypass reports the manual bypass sweep state and the active
// identity tuple.
func (s *Server) handleAdminBypass(w http.ResponseWriter, r *http.Request) {
	if _, _, aerr := s.authenticate(r); aerr != nil {
		writeErr(w, aerr.Status, aerr.Code, aerr.Type, aerr.Message)
		return
	}
	out := map[string]any{"active": route.CurrentBypass(), "catalog": route.BypassCatalog(), "template": route.TemplateInfo()}
	if tr := s.router.BypassTransport(); tr != nil {
		out["state"] = tr.BypassState()
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// handleAdminBypassRun starts one manual sweep: POST /admin/bypass/run?fresh=3&model=...
func (s *Server) handleAdminBypassRun(w http.ResponseWriter, r *http.Request) {
	if _, _, aerr := s.authenticate(r); aerr != nil {
		writeErr(w, aerr.Status, aerr.Code, aerr.Type, aerr.Message)
		return
	}
	tr := s.router.BypassTransport()
	if tr == nil {
		writeErr(w, http.StatusConflict, "no_cli_transport", "invalid_request_error", "CLI-транспорт не активен")
		return
	}
	fresh := 3
	if v := r.URL.Query().Get("fresh"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			fresh = n
		}
	}
	all := r.URL.Query().Get("all") == "1" || r.URL.Query().Get("all") == "true"
	baseOnly := r.URL.Query().Get("base") == "1" || r.URL.Query().Get("base") == "true"
	currentOnly := r.URL.Query().Get("current") == "1" || r.URL.Query().Get("current") == "true"
	st, err := tr.StartBypass(route.BypassOptions{
		Fresh: fresh, Model: r.URL.Query().Get("model"), AllParams: all,
		BaseOnly: baseOnly, CurrentOnly: currentOnly,
	})
	if err != nil {
		writeErr(w, http.StatusConflict, "bypass_running", "invalid_request_error", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(st)
}

// handleAdminBypassApply adopts one variant (UA or tool kit).
func (s *Server) handleAdminBypassApply(w http.ResponseWriter, r *http.Request) {
	if _, _, aerr := s.authenticate(r); aerr != nil {
		writeErr(w, aerr.Status, aerr.Code, aerr.Type, aerr.Message)
		return
	}
	a, err := route.ApplyBypassVariant(r.URL.Query().Get("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_variant", "invalid_request_error", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"active": a})
}

// handleAdminBypassReset returns to the built-in identity tuple.
func (s *Server) handleAdminBypassReset(w http.ResponseWriter, r *http.Request) {
	if _, _, aerr := s.authenticate(r); aerr != nil {
		writeErr(w, aerr.Status, aerr.Code, aerr.Type, aerr.Message)
		return
	}
	a, err := route.ResetBypassOverrides()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "reset_failed", "server_error", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"active": a})
}
