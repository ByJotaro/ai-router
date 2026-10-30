// Package events defines the cross-cutting event contracts emitted by the
// gateway (ai.request and friends) and consumed by hubmod and the GUI's
// request/usage logging.
package events

// Tokens breaks a completed request's token usage into its components.
type Tokens struct {
	Prompt     int64 `json:"prompt"`
	Completion int64 `json:"completion"`
	CacheRead  int64 `json:"cache_read"`
	CacheWrite int64 `json:"cache_write"`
	Reasoning  int64 `json:"reasoning"`
	Compressed int64 `json:"compressed"`
}

// RequestEvent is the payload of the ai.request event emitted once per chat
// completion (streaming and non-streaming). Fields that are not resolvable
// at this phase (account, connection_id, combo_*) stay empty.
type RequestEvent struct {
	Model          string `json:"model"`
	RequestedModel string `json:"requested_model"`
	Provider       string `json:"provider"`
	Account        string `json:"account"`
	ConnectionID   string `json:"connection_id"`
	DurationMS     int64  `json:"duration_ms"`
	Tokens         Tokens `json:"tokens"`
	ComboName      string `json:"combo_name"`
	ComboStepID    string `json:"combo_step_id"`
	// ComboPath is the full graph walk trace ("N:provider:fail",
	// "N:c1:cond:miss", "2:anthropic:win", "tail:stop"); empty for
	// legacy linear combos and non-combo requests.
	ComboPath  []string `json:"combo_path"`
	Status     string   `json:"status"`
	HTTPStatus int      `json:"http_status"`
	Error      string   `json:"error"`
	Proxy      string   `json:"proxy"`
	// WaitedMS: the pool made the request wait this long for an egress before
	// serving it (0 = immediate). Distinct journal category: a retried-but-
	// successful request is a success, but not a healthy-pool success.
	WaitedMS int64 `json:"waited_ms"`
	RID        string   `json:"rid"`
	// CostCents is the USD spend in cents computed from the model price
	// catalog (0 when pricing is unavailable). Set by the gateway before
	// persistence, so the ai.request event carries the same cost as the log.
	CostCents int64 `json:"cost_cents"`
}
