package sdk

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
)

// ErrAccepted signals that the handler started async work and the hub should
// treat the call as accepted (202) rather than completed.
var ErrAccepted = errors.New("accepted")

// Job is a handle for a module-side job started via a hub job call.
type Job struct {
	ID string
	c  *Client
}

// Progress reports progress to the hub.
func (j *Job) Progress(p float64, stage, msg string) error {
	return j.c.ReportProgress(context.Background(), j.ID, p, stage, msg)
}

// Complete reports success with the given result.
func (j *Job) Complete(result any) error {
	return j.c.Complete(context.Background(), j.ID, result)
}

// Fail reports failure.
func (j *Job) Fail(code, message string) error {
	return j.c.Fail(context.Background(), j.ID, code, message)
}

// ActionFunc implements a module action. params is the raw params object from
// the hub. job is non-nil when the call is part of a hub job; return
// (nil, ErrAccepted) to run asynchronously, or (result, nil) to complete
// inline.
type ActionFunc func(ctx context.Context, params json.RawMessage, job *Job) (any, error)

// ActionMux routes /actions/{name} to registered ActionFuncs.
type ActionMux struct {
	actions      map[string]ActionFunc
	hubToken     string
	version      string
	capabilities []string
	client       *Client
}

// NewActionMux creates an action router.
func NewActionMux() *ActionMux {
	return &ActionMux{actions: map[string]ActionFunc{}}
}

// Options configures the SDK HTTP server.
type Options struct {
	Token        string   // hub-side token this module is registered with
	Version      string   // reported on /health
	Capabilities []string // reported on /health
	// Client is used to report job callbacks back to the hub.
	Client *Client
}

// Handle registers an action function (action names may contain slashes, e.g.
// "slow/cancel").
func (m *ActionMux) Handle(action string, fn ActionFunc) {
	m.actions[action] = fn
}

// Serve starts an HTTP server on addr serving /health and /actions/{name}.
func (m *ActionMux) Serve(addr string, opts Options) error {
	m.hubToken = opts.Token
	m.version = opts.Version
	m.capabilities = opts.Capabilities
	m.client = opts.Client
	srv := &http.Server{Addr: addr, Handler: m}
	return srv.ListenAndServe()
}

// ServeHTTP implements http.Handler.
func (m *ActionMux) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/health":
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "version": m.version, "capabilities": m.capabilities,
		})
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/actions/"):
		if !m.authenticate(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": map[string]any{"code": "unauthorized", "message": "invalid hub token"}})
			return
		}
		m.dispatch(w, r, strings.TrimPrefix(r.URL.Path, "/actions/"))
	default:
		http.NotFound(w, r)
	}
}

func (m *ActionMux) authenticate(r *http.Request) bool {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return false
	}
	return strings.TrimSpace(strings.TrimPrefix(h, "Bearer ")) == m.hubToken
}

func (m *ActionMux) dispatch(w http.ResponseWriter, r *http.Request, action string) {
	fn, ok := m.actions[action]
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]any{"code": "not_found", "message": "action " + action + " not found"}})
		return
	}
	var body struct {
		Params json.RawMessage `json:"params"`
		Hub    string          `json:"hub"`
		JobID  string          `json:"job_id"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"code": "bad_request", "message": err.Error()}})
		return
	}
	if len(body.Params) == 0 {
		body.Params = json.RawMessage("{}")
	}
	var job *Job
	if body.JobID != "" && m.client != nil {
		job = &Job{ID: body.JobID, c: m.client}
	}
	result, err := fn(r.Context(), body.Params, job)
	if err != nil {
		if errors.Is(err, ErrAccepted) {
			writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "accepted": true})
			return
		}
		e := toSDKError(err)
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": e})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "result": result})
}

func toSDKError(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return &Error{Code: "action_failed", Message: err.Error()}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
