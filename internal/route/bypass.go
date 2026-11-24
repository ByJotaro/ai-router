package route

// Manual emergency enumeration ("аварийный перебор") for the opencode free
// lane. The free gate keys on the request's identity tuple (User-Agent,
// tool-schema parameter names, ...). When a 429 wall follows even a fresh /64,
// the operator opens the proxy-pool tab, draws several new egresses and asks
// the router to probe alternate identities on them. Nothing here runs on its
// own: the sweep starts from a button (GUI binding or /admin/bypass/run), and
// adopting a winner is a second explicit action.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ByJotaro/ai-router/internal/config"
)

// ---------------------------------------------------------------- tool kits --

// toolKit is one schema-parameter naming scheme. Schema maps the names the
// gate refuses (old_string/new_string) to the names put on the wire; Reverse
// maps them back inside tool-call arguments; Pairs is the quoted reverse map
// used by the streamed-argument rewriter.
type toolKit struct {
	ID      string
	Label   string
	Schema  map[string]string
	Reverse map[string]string
	Pairs   [][2]string
}

func newKit(id, label string, schema map[string]string) *toolKit {
	k := &toolKit{ID: id, Label: label, Schema: schema, Reverse: map[string]string{}}
	for from, to := range schema {
		k.Reverse[to] = from
	}
	keys := make([]string, 0, len(k.Reverse))
	for to := range k.Reverse {
		keys = append(keys, to)
	}
	sort.Strings(keys)
	for _, to := range keys {
		k.Pairs = append(k.Pairs, [2]string{`"` + to + `"`, `"` + k.Reverse[to] + `"`})
	}
	return k
}

var (
	kitCamel = newKit("camel", "oldString/newString", map[string]string{
		"old_string": "oldString",
		"new_string": "newString",
	})
	kitText = newKit("text", "oldText/newText", map[string]string{
		"old_string": "oldText",
		"new_string": "newText",
	})
	kitStr = newKit("str", "old_str/new_str", map[string]string{
		"old_string": "old_str",
		"new_string": "new_str",
	})
	// Identity kit: the gate refused snake names in 2026-11-15 measurements.
	// Kept as the negative control of the sweep (it must 429 on a fresh IP).
	kitSnake = newKit("snake", "old_string/new_string (контроль)", map[string]string{})
)

var bypassKits = []*toolKit{kitCamel, kitText, kitStr, kitSnake}

func kitByID(id string) *toolKit {
	for _, k := range bypassKits {
		if k.ID == id {
			return k
		}
	}
	return kitCamel
}

// -------------------------------------------------------------- overrides --

// BypassActive is the identity tuple the live lane currently uses. Empty UA or
// Kit means the built-in default (cliUserAgent / camel kit). KitMap is the
// concrete rename map of the applied kit: custom/template-derived kits cannot
// be reconstructed after a restart without it.
type BypassActive struct {
	UA     string            `json:"ua"`
	Kit    string            `json:"kit"`
	KitMap map[string]string `json:"kit_map,omitempty"`
}

var bypassMu sync.RWMutex
var bypassCur BypassActive
var bypassLoaded bool
var bypassKitCache atomic.Value // *toolKit

func bypassFilePath() string {
	return filepath.Join(config.BaseDir(), "bypass_override.json")
}

func loadBypassOnce() {
	bypassMu.Lock()
	defer bypassMu.Unlock()
	if bypassLoaded {
		return
	}
	bypassLoaded = true
	b, err := os.ReadFile(bypassFilePath())
	if err != nil {
		return
	}
	var a BypassActive
	if json.Unmarshal(b, &a) != nil {
		return
	}
	if a.UA != "" && !strings.HasPrefix(a.UA, "opencode/") {
		a.UA = ""
	}
	if !validKitMap(a.KitMap) {
		a.Kit, a.KitMap = "", nil
	}
	bypassCur = a
	bypassKitCache.Store(resolveKit(a))
}

func copyKitMap(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func validKitMap(m map[string]string) bool {
	if len(m) == 0 {
		return true
	}
	for k, v := range m {
		if strings.TrimSpace(k) == "" || strings.TrimSpace(v) == "" || k == v {
			return false
		}
	}
	return true
}

func resolveKit(a BypassActive) *toolKit {
	if len(a.KitMap) == 0 {
		if a.Kit == "" {
			return kitCamel
		}
		return kitByID(a.Kit)
	}
	id := a.Kit
	if id == "" {
		id = "custom"
	}
	return newKit(id, id, copyKitMap(a.KitMap))
}

func activeBypass() BypassActive {
	loadBypassOnce()
	bypassMu.RLock()
	defer bypassMu.RUnlock()
	return bypassCur
}

func activeBypassUA() string {
	a := activeBypass()
	if a.UA != "" {
		return a.UA
	}
	return cliUserAgent
}

func activeToolKit() *toolKit {
	loadBypassOnce()
	if v := bypassKitCache.Load(); v != nil {
		return v.(*toolKit)
	}
	return kitCamel
}

func setBypassActive(a BypassActive) (BypassActive, error) {
	loadBypassOnce()
	if a.UA != "" && !strings.HasPrefix(a.UA, "opencode/") {
		return BypassActive{}, fmt.Errorf("UA должен начинаться с opencode/: %q", a.UA)
	}
	if len(a.KitMap) > 0 && !validKitMap(a.KitMap) {
		return BypassActive{}, errors.New("некорректная карта переименований")
	}
	bypassMu.Lock()
	bypassCur = a
	bypassMu.Unlock()
	bypassKitCache.Store(resolveKit(a))
	b, _ := json.MarshalIndent(a, "", "  ")
	if err := os.MkdirAll(config.BaseDir(), 0o755); err != nil {
		return a, err
	}
	if err := os.WriteFile(bypassFilePath(), b, 0o644); err != nil {
		return a, err
	}
	return a, nil
}

// ApplyBypassVariant adopts one sweep variant: a UA or a kit (base resets).
// The concrete rename map is persisted, so a template-derived kit survives a
// restart without re-reading the captured request.
func ApplyBypassVariant(id string) (BypassActive, error) {
	v, ok := variantByID(id)
	if !ok {
		return BypassActive{}, fmt.Errorf("неизвестный вариант: %q", id)
	}
	a := activeBypass()
	switch v.Axis {
	case "ua":
		a.UA = v.Value
	case "kit":
		k, err := kitForVariant(v, loadProbeTemplate())
		if err != nil {
			return BypassActive{}, err
		}
		a.Kit = k.ID
		a.KitMap = copyKitMap(k.Schema)
	default:
		a = BypassActive{}
	}
	return setBypassActive(a)
}

// ResetBypassOverrides returns the lane to the built-in identity.
func ResetBypassOverrides() (BypassActive, error) {
	_ = os.Remove(bypassFilePath())
	return setBypassActive(BypassActive{})
}

// --------------------------------------------------------------- catalogue --

// BypassVariant is one single-axis probe: base, a UA tuple, or a tool kit.
type BypassVariant struct {
	ID    string `json:"id"`
	Axis  string `json:"axis"`
	Value string `json:"value"`
	Label string `json:"label"`
}

var bypassUASuffix = "ai-sdk/provider-utils/4.0.23 runtime/bun/1.4.4"

func bypassCatalog() []BypassVariant {
	out := []BypassVariant{{ID: "current", Axis: "base", Label: "текущие параметры"}}
	for _, ver := range []string{"1.19.4", "1.19.5", "1.19.7", "1.20.0"} {
		out = append(out, BypassVariant{
			ID: "ua:" + ver, Axis: "ua", Value: "opencode/" + ver + " " + bypassUASuffix,
			Label: "UA opencode/" + ver,
		})
	}
	for _, k := range bypassKits {
		if k.ID == "camel" {
			continue // the built-in default; "current" already covers it
		}
		out = append(out, BypassVariant{ID: "kit:" + k.ID, Axis: "kit", Value: k.ID, Label: "параметры: " + k.Label})
	}
	if tpl := loadProbeTemplate(); tpl != nil && len(tpl.clientProps(activeToolKit())) > 0 {
		out = append(out, BypassVariant{ID: "kit:all-camel", Axis: "kit", Value: "all-camel", Label: "все snake-параметры → camel"})
		out = append(out, BypassVariant{ID: "kit:all-prefix", Axis: "kit", Value: "all-prefix", Label: "все параметры → p_*"})
	}
	return out
}

func variantByID(id string) (BypassVariant, bool) {
	for _, v := range bypassCatalog() {
		if v.ID == id {
			return v, true
		}
	}
	if p := strings.TrimPrefix(id, "prop:"); p != id && p != "" {
		tpl := loadProbeTemplate()
		if tpl != nil && tpl.hasClientProp(p, activeToolKit()) {
			return BypassVariant{ID: id, Axis: "kit", Value: id, Label: "параметр " + p}, true
		}
	}
	return BypassVariant{}, false
}

// kitForVariant resolves a variant to its concrete rename map. Fixed kits need
// no template; all-* and prop:* kits are derived from the latest captured
// request (prop:* needs only the property name, so an applied override keeps
// working after the capture is gone).
func kitForVariant(v BypassVariant, tpl *probeTemplate) (*toolKit, error) {
	switch v.Value {
	case "camel":
		return kitCamel, nil
	case "text":
		return kitText, nil
	case "str":
		return kitStr, nil
	case "snake":
		return kitSnake, nil
	}
	if strings.HasPrefix(v.Value, "prop:") {
		p := strings.TrimPrefix(v.Value, "prop:")
		if p == "" {
			return nil, errors.New("пустое имя параметра")
		}
		return newKit("prop:"+p, "параметр "+p, map[string]string{p: altPropName(p)}), nil
	}
	if tpl == nil || len(tpl.props) == 0 {
		return nil, errors.New("нет шаблона последнего запроса: перебор всех параметров недоступен")
	}
	switch v.Value {
	case "all-camel":
		m := map[string]string{}
		for _, p := range tpl.clientProps(activeToolKit()) {
			if c := camelPropName(p); c != "" && c != p {
				m[p] = c
			}
		}
		if len(m) == 0 {
			return nil, errors.New("в шаблоне нет snake-параметров")
		}
		return newKit("all-camel", "все snake→camel", m), nil
	case "all-prefix":
		m := map[string]string{}
		for _, p := range tpl.clientProps(activeToolKit()) {
			m[p] = altPropName(p)
		}
		return newKit("all-prefix", "все параметры → p_*", m), nil
	}
	return nil, fmt.Errorf("неизвестный набор: %q", v.Value)
}

// sweepVariants is the catalogue for one run. all=true adds one variant per
// real tool property, so "any other parameter got blocked" is covered.
func sweepVariants(all bool) []BypassVariant {
	out := bypassCatalog()
	if !all {
		return out
	}
	tpl := loadProbeTemplate()
	if tpl == nil {
		return out
	}
	for _, p := range tpl.clientProps(activeToolKit()) {
		if p == "old_string" || p == "new_string" {
			continue
		}
		out = append(out, BypassVariant{ID: "prop:" + p, Axis: "kit", Value: "prop:" + p, Label: "параметр " + p})
	}
	return out
}

// --------------------------------------------------------------- template --

// probeTemplate is the latest real request captured from the CLI lane. Only
// the tool schemas are reused: the message history is what varies per turn,
// the tools are what the gate inspects.
type probeTemplate struct {
	source  string
	model   string
	tools   []json.RawMessage
	props   []string
	hasProp map[string]bool
	bytes   int
}

var (
	tplMu    sync.Mutex
	tplCache *probeTemplate
	tplAt    time.Time
)

// loadProbeTemplate finds the newest *.req capture. Lookup order: the router's
// own MITM dir (env), the per-user dir, then %TEMP%. Cached for two minutes; a
// capture older than 24 h still loads but is reported by age in the state.
func loadProbeTemplate() *probeTemplate {
	tplMu.Lock()
	defer tplMu.Unlock()
	if tplCache != nil && time.Since(tplAt) < 2*time.Minute {
		return tplCache
	}
	dirs := []string{os.Getenv("AI_ROUTER_MITM_DIR"), filepath.Join(config.BaseDir(), "mitm"), filepath.Join(os.TempDir(), "mitm")}
	var best string
	var bestT time.Time
	for _, dir := range dirs {
		if strings.TrimSpace(dir) == "" {
			continue
		}
		ents, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range ents {
			if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".req") {
				continue
			}
			info, ierr := e.Info()
			if ierr != nil {
				continue
			}
			if info.ModTime().After(bestT) {
				best = filepath.Join(dir, e.Name())
				bestT = info.ModTime()
			}
		}
	}
	if best == "" {
		tplCache = nil
		tplAt = time.Now()
		return nil
	}
	tpl := parseTemplateFile(best)
	tplCache = tpl
	tplAt = time.Now()
	return tpl
}

func parseTemplateFile(path string) *probeTemplate {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	body := raw
	if i := bytes.Index(raw, []byte("\r\n\r\n")); i >= 0 {
		body = raw[i+4:]
	} else if i := bytes.Index(raw, []byte("\n\n")); i >= 0 {
		body = raw[i+2:]
	}
	var doc struct {
		Model string          `json:"model"`
		Tools json.RawMessage `json:"tools"`
	}
	if json.Unmarshal(body, &doc) != nil {
		return nil
	}
	var tools []json.RawMessage
	if len(doc.Tools) > 0 {
		_ = json.Unmarshal(doc.Tools, &tools)
	}
	tpl := &probeTemplate{source: path, model: doc.Model, tools: tools, bytes: len(body), hasProp: map[string]bool{}}
	seen := map[string]bool{}
	for _, rawT := range tools {
		var t struct {
			Function struct {
				Parameters struct {
					Properties map[string]json.RawMessage `json:"properties"`
				} `json:"parameters"`
			} `json:"function"`
		}
		if json.Unmarshal(rawT, &t) != nil {
			continue
		}
		for p := range t.Function.Parameters.Properties {
			tpl.hasProp[p] = true
			if !seen[p] {
				seen[p] = true
				tpl.props = append(tpl.props, p)
			}
		}
	}
	sort.Strings(tpl.props)
	return tpl
}

func (t *probeTemplate) info(active *toolKit) *BypassTemplate {
	if t == nil {
		return nil
	}
	return &BypassTemplate{Source: t.source, Model: t.model, Tools: len(t.tools), Props: t.clientProps(active), Bytes: t.bytes}
}

// clientProps translates the captured wire property names back to the names
// the client (OMP) sent: the capture was taken AFTER the active kit's rename,
// so testing another kit must start from the client-space schema.
func (t *probeTemplate) clientProps(active *toolKit) []string {
	if t == nil {
		return nil
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(t.props))
	for _, p := range t.props {
		if c, ok := active.Reverse[p]; ok {
			p = c
		}
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

func (t *probeTemplate) hasClientProp(p string, active *toolKit) bool {
	for _, cp := range t.clientProps(active) {
		if cp == p {
			return true
		}
	}
	return false
}

func camelPropName(p string) string {
	if !strings.Contains(p, "_") {
		return ""
	}
	var b strings.Builder
	up := false
	for _, r := range p {
		if r == '_' {
			up = true
			continue
		}
		if up {
			b.WriteString(strings.ToUpper(string(r)))
			up = false
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func altPropName(p string) string {
	if c := camelPropName(p); c != "" && c != p {
		return c
	}
	return "p_" + p
}

// ------------------------------------------------------------------ sweep --

// BypassOptions controls one manual sweep. Fresh is how many brand-new egress
// addresses (per-slot redraws) to draw; 0 falls back to current clean lanes.
type BypassOptions struct {
	Fresh int    `json:"fresh"`
	Model string `json:"model"`
	// AllParams adds one variant per tool-schema property found in the
	// template request (the "any other parameter got blocked" sweep).
	AllParams bool `json:"all_params"`
	// BaseOnly runs just the baseline variant: the minimal live check that a
	// lane admits the current identity without spending the whole matrix.
	BaseOnly bool `json:"base_only"`
	// CurrentOnly probes the lanes already in the pool view instead of
	// drawing fresh egresses: those lanes carry the live pins, so the probe
	// can prove it reuses a warm ses_* rather than minting a new one.
	CurrentOnly bool `json:"current_only"`
}

type BypassLane struct {
	Proxy string `json:"proxy"`
	IP    string `json:"ip"`
}

type BypassLaneResult struct {
	Proxy  string `json:"proxy"`
	IP     string `json:"ip"`
	Status int    `json:"status"`
	// BaseBefore/BaseAfter are the baseline probes around the variant: a pass
	// counts as an unlock only when the base failed on both sides of it, so
	// lane recovery is not crowned as a variant win.
	BaseBefore int    `json:"base_before"`
	BaseAfter  int    `json:"base_after"`
	Verdict    string `json:"verdict"`
	MS         int64  `json:"ms"`
	Err        string `json:"err,omitempty"`
	Body       string `json:"body,omitempty"`
}

type BypassRow struct {
	Variant   string             `json:"variant"`
	Label     string             `json:"label"`
	Axis      string             `json:"axis"`
	Pass      int                `json:"pass"`
	Fail      int                `json:"fail"`
	Unlocked  int                `json:"unlocked"`
	Drift     int                `json:"drift"`
	Uncertain int                `json:"uncertain"`
	Results   []BypassLaneResult `json:"results"`
}

// BypassTemplate describes the latest real (MITM) request used as the probe
// shape. Messages are replaced with a one-token probe; the tool schemas are
// kept, because their property names are what the gate checks.
type BypassTemplate struct {
	Source string   `json:"source"`
	Model  string   `json:"model"`
	Tools  int      `json:"tools"`
	Props  []string `json:"props"`
	Bytes  int      `json:"bytes"`
}

type BypassSweepState struct {
	Running    bool            `json:"running"`
	Phase      string          `json:"phase"`
	StartedAt  string          `json:"started_at"`
	FinishedAt string          `json:"finished_at,omitempty"`
	Fresh      int             `json:"fresh"`
	AllParams  bool            `json:"all_params"`
	Template   *BypassTemplate `json:"template,omitempty"`
	Lanes      []BypassLane    `json:"lanes"`
	Rows       []BypassRow     `json:"rows"`
	Active     BypassActive    `json:"active"`
	Error      string          `json:"error,omitempty"`
}

func nowRFC3339() string { return time.Now().Format(time.RFC3339) }

// StartBypass launches the sweep in the background and returns the initial
// state. Only one sweep runs at a time.
func (t *CLITransport) StartBypass(opts BypassOptions) (*BypassSweepState, error) {
	t.bypassMu.Lock()
	if t.bypassRunning {
		t.bypassMu.Unlock()
		return nil, errors.New("перебор уже идёт")
	}
	t.bypassRunning = true
	if opts.Fresh == 0 {
		if opts.CurrentOnly {
			opts.Fresh = 2
		} else {
			opts.Fresh = 3
		}
	}
	if opts.Fresh > 5 {
		opts.Fresh = 5
	}
	if strings.TrimSpace(opts.Model) == "" {
		opts.Model = "deepseek-v4.1-flash-free"
	}
	tpl := loadProbeTemplate()
	t.bypassState = &BypassSweepState{
		Running:   true,
		Phase:     "рисую новые IP",
		StartedAt: nowRFC3339(),
		Fresh:     opts.Fresh,
		AllParams: opts.AllParams,
		Template:  tpl.info(activeToolKit()),
		Active:    activeBypass(),
	}
	t.bypassMu.Unlock()
	go t.runBypass(opts)
	return t.BypassState(), nil
}

// BypassState returns a deep copy safe to marshal while the sweep runs.
func (t *CLITransport) BypassState() *BypassSweepState {
	t.bypassMu.Lock()
	defer t.bypassMu.Unlock()
	if t.bypassState == nil {
		return &BypassSweepState{Active: activeBypass()}
	}
	cp := *t.bypassState
	cp.Lanes = append([]BypassLane(nil), t.bypassState.Lanes...)
	cp.Rows = make([]BypassRow, len(t.bypassState.Rows))
	for i := range cp.Rows {
		cp.Rows[i] = t.bypassState.Rows[i]
		cp.Rows[i].Results = append([]BypassLaneResult(nil), t.bypassState.Rows[i].Results...)
	}
	return &cp
}

func (t *CLITransport) bypassPhase(phase string, lanes []BypassLane) {
	t.bypassMu.Lock()
	if t.bypassState != nil {
		t.bypassState.Phase = phase
		if lanes != nil {
			t.bypassState.Lanes = append([]BypassLane(nil), lanes...)
		}
	}
	t.bypassMu.Unlock()
}

func (t *CLITransport) bypassPutRow(idx int, row BypassRow) {
	t.bypassMu.Lock()
	defer t.bypassMu.Unlock()
	if t.bypassState == nil {
		return
	}
	row.Results = append([]BypassLaneResult(nil), row.Results...)
	if idx < len(t.bypassState.Rows) {
		t.bypassState.Rows[idx] = row
		return
	}
	t.bypassState.Rows = append(t.bypassState.Rows, row)
}

func (t *CLITransport) bypassFinish(err error) {
	t.bypassMu.Lock()
	defer t.bypassMu.Unlock()
	if t.bypassState != nil {
		t.bypassState.Running = false
		t.bypassState.FinishedAt = nowRFC3339()
		if err != nil {
			t.bypassState.Error = err.Error()
			t.bypassState.Phase = "ошибка"
		} else {
			t.bypassState.Phase = "готово"
		}
	}
	t.bypassRunning = false
}

func (t *CLITransport) runBypass(opts BypassOptions) {
	var runErr error
	defer func() { t.bypassFinish(runErr) }()

	var lanes []BypassLane
	if !opts.CurrentOnly && opts.Fresh > 0 {
		lanes = t.drawFreshLanes(opts.Fresh)
	}
	if len(lanes) == 0 {
		n := opts.Fresh
		if n < 1 {
			n = 3
		}
		lanes = t.currentProbeLanes(n)
	}
	if len(lanes) == 0 {
		runErr = errors.New("нет ни одного свежего или чистого канала")
		return
	}
	tpl := loadProbeTemplate()
	t.bypassPhase("опрашиваю варианты (база → вариант → база)", lanes)
	for i, v := range sweepVariants(opts.AllParams) {
		if opts.BaseOnly && v.Axis != "base" {
			continue
		}
		row := BypassRow{Variant: v.ID, Label: v.Label, Axis: v.Axis}
		t.bypassPutRow(i, row)
		// One lane is enough to catch "this property is blocked"; the full
		// matrix over every fresh IP would multiply the token cost by lanes.
		vl := lanes
		if strings.HasPrefix(v.ID, "prop:") && len(vl) > 1 {
			vl = vl[:1]
		}
		for _, ln := range vl {
			res := t.probeAB(opts.Model, v, ln, tpl)
			if res.Status == 200 {
				row.Pass++
			} else {
				row.Fail++
			}
			switch res.Verdict {
			case "unlocked":
				row.Unlocked++
			case "drift":
				row.Drift++
			case "uncertain":
				row.Uncertain++
			}
			row.Results = append(row.Results, res)
			t.bypassPutRow(i, row)
			time.Sleep(250 * time.Millisecond)
		}
	}
}

// forceLanes drops the 60 s lane cache and re-reads the pool view.
func (t *CLITransport) forceLanes() []lane {
	t.mu.Lock()
	t.fetched = time.Time{}
	t.lanes = nil
	t.mu.Unlock()
	return t.lanesNow()
}

// drawFreshLanes redraws up to n main-pool slots and waits for each to come
// back on a different address. Redraws are serialized process-wide inside
// redrawLane, and a slot with a live stream is skipped (its restart would RST
// the answer).
func (t *CLITransport) drawFreshLanes(n int) []BypassLane {
	if n <= 0 {
		return nil
	}
	lanes := t.forceLanes()
	t.mu.Lock()
	poolPorts := make(map[int]bool, len(t.poolPorts))
	for p := range t.poolPorts {
		poolPorts[p] = true
	}
	active := make(map[string]int, len(t.active))
	for k, v := range t.active {
		active[k] = v
	}
	t.mu.Unlock()
	out := make([]BypassLane, 0, n)
	for _, l := range lanes {
		if len(out) >= n {
			break
		}
		port := lanePort(l.proxy)
		if len(poolPorts) > 0 {
			if !poolPorts[port] {
				continue
			}
		} else if port < 8750 || port > 8769 {
			continue
		}
		if active[l.proxy] > 0 || l.ip == "" || !proxyReachable(l.proxy) {
			continue
		}
		oldIP := l.ip
		t.mu.Lock()
		delete(t.redrawAt, l.proxy)
		t.mu.Unlock()
		t.redrawLane(l.proxy, true)
		deadline := time.Now().Add(30 * time.Second)
		got := false
		for !got && time.Now().Before(deadline) {
			time.Sleep(time.Second)
			for _, nl := range t.forceLanes() {
				if nl.proxy == l.proxy && nl.ip != "" && nl.ip != oldIP {
					out = append(out, BypassLane{Proxy: l.proxy, IP: nl.ip})
					got = true
					break
				}
			}
		}
	}
	return out
}

// proxyReachable checks the local egress listener before spending a probe on
// it. After a router restart the pool view can list a slot whose backend is
// still starting; dialling it only produced a matrix of 0s (observed 17:52).
func proxyReachable(proxy string) bool {
	if strings.TrimSpace(proxy) == "" {
		return true // direct leg: cliDo dials the upstream itself
	}
	host := strings.TrimPrefix(strings.TrimPrefix(proxy, "http://"), "https://")
	if i := strings.Index(host, "/"); i >= 0 {
		host = host[:i]
	}
	c, err := net.DialTimeout("tcp", host, 700*time.Millisecond)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// currentProbeLanes falls back to up to n clean lanes (unparked, one per /64)
// when a redraw yields nothing.
func (t *CLITransport) currentProbeLanes(n int) []BypassLane {
	out := make([]BypassLane, 0, n)
	seen := map[string]bool{}
	now := time.Now().UnixMilli()
	for _, l := range t.lanesNow() {
		if len(out) >= n {
			break
		}
		if l.ip == "" || !proxyReachable(l.proxy) {
			continue
		}
		g := group64(l.ip)
		if seen[g] {
			continue
		}
		t.mu.Lock()
		st := t.book[l.proxy]
		parked := st != nil && st.Until > now
		t.mu.Unlock()
		if parked {
			continue
		}
		seen[g] = true
		out = append(out, BypassLane{Proxy: l.proxy, IP: l.ip})
	}
	return out
}

// probeModelBody builds the probe. With a captured template it reuses the
// REAL tool schemas (all property names preserved, descriptions truncated to
// keep the quota cost of a manual sweep sane) and the canonical system prefix;
// without one it falls back to the validated gate probe plus an edit tool.
// Messages and session ids are always replaced: those are what varies per turn.
func probeModelBody(model string, kit *toolKit, tpl *probeTemplate) []byte {
	active := activeToolKit()
	m := map[string]any{
		"model":      model,
		"messages":   []any{map[string]any{"role": "system", "content": canonSystemPrefix()}, map[string]any{"role": "user", "content": "hi"}},
		"max_tokens": 1,
		"stream":     true,
	}
	if tpl != nil && len(tpl.tools) > 0 {
		tools := templateTools(tpl, active, kit)
		if !tpl.hasClientProp("old_string", active) && !tpl.hasClientProp("new_string", active) {
			tools = append(tools, editProbeTool())
		}
		m["tools"] = tools
	} else {
		var base map[string]any
		if json.Unmarshal(gateProbeBody(model), &base) == nil {
			m = base
		}
		tools, _ := m["tools"].([]any)
		m["tools"] = append(tools, editProbeTool())
	}
	b, err := json.Marshal(m)
	if err != nil {
		return gateProbeBody(model)
	}
	if nb, changed := renameForbiddenPropsWith(b, kit); changed {
		b = nb
	}
	return b
}

func editProbeTool() map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        "edit",
			"description": "Edit a file",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"filePath":   map[string]any{"type": "string"},
					"old_string": map[string]any{"type": "string"},
					"new_string": map[string]any{"type": "string"},
				},
				"required": []string{"filePath", "old_string", "new_string"},
			},
		},
	}
}

// templateTools reuses the captured schemas with descriptions shortened. The
// gate keys on names and shapes; full descriptions only cost input tokens.
func templateTools(tpl *probeTemplate, active, variant *toolKit) []any {
	out := make([]any, 0, len(tpl.tools))
	for _, rawT := range tpl.tools {
		var t map[string]any
		if json.Unmarshal(rawT, &t) != nil {
			continue
		}
		if fn, ok := t["function"].(map[string]any); ok {
			if d, ok := fn["description"].(string); ok && len(d) > 40 {
				fn["description"] = d[:40]
			}
			if params, ok := fn["parameters"].(map[string]any); ok {
				compactSchema(params)
				renameSchemaProps(params, active.Reverse, variant.Schema)
			}
		}
		out = append(out, t)
		if len(out) >= 60 {
			break
		}
	}
	return out
}

// compactSchema keeps every property NAME (the gate keys on those) and the
// declared type, dropping descriptions/examples/defaults: a real 26 KB toolset
// shrinks to a couple of KB, which matters because a manual sweep is quota.
func compactSchema(params map[string]any) {
	props, ok := params["properties"].(map[string]any)
	if !ok {
		return
	}
	for k, v := range props {
		if pm, ok := v.(map[string]any); ok {
			if t, ok := pm["type"].(string); ok {
				props[k] = map[string]any{"type": t}
				continue
			}
		}
		props[k] = map[string]any{"type": "string"}
	}
	params["properties"] = props
}

// renameSchemaProps walks properties (and required) translating wire names back
// to client space and then into the variant's wire names.
func renameSchemaProps(params map[string]any, from, to map[string]string) {
	if props, ok := params["properties"].(map[string]any); ok {
		np := make(map[string]any, len(props))
		for k, v := range props {
			nk := k
			if c, hit := from[k]; hit {
				nk = c
			}
			if n, hit := to[nk]; hit {
				nk = n
			}
			np[nk] = v
		}
		params["properties"] = np
	}
	if req, ok := params["required"].([]any); ok {
		for i, r := range req {
			s, ok := r.(string)
			if !ok {
				continue
			}
			if c, hit := from[s]; hit {
				s = c
			}
			if n, hit := to[s]; hit {
				s = n
			}
			req[i] = s
		}
		params["required"] = req
	}
}

// probeSession reuses a session that already served on this egress: the zen
// allowance binds to (session, egress), so a fresh minted ses_* on a lane whose
// new-conversation budget is spent answers 429 even when warm traffic still
// serves. Falls back to the process-stable per-egress id. The second return
// names the source ("pin" / "fallback") for the probe log.
func (t *CLITransport) probeSession(ln BypassLane) (string, string) {
	t.mu.Lock()
	best := ""
	var bestAt int64
	for _, pins := range t.pins {
		for _, p := range pins {
			if p.SID == "" {
				continue
			}
			if p.Proxy == ln.Proxy || (ln.IP != "" && p.IP == ln.IP) {
				if p.At >= bestAt {
					best, bestAt = p.SID, p.At
				}
			}
		}
	}
	t.mu.Unlock()
	if best != "" {
		return best, "pin"
	}
	return fallbackSessionFor(ln.Proxy), "fallback"
}

// probeOnce sends one probe on one explicit lane. It never marks or parks the
// lane: the sweep is diagnostic and must not poison the live book.
func (t *CLITransport) probeOnce(model, ua string, kit *toolKit, ln BypassLane, _ string, tpl *probeTemplate) BypassLaneResult {
	res := BypassLaneResult{Proxy: ln.Proxy, IP: ln.IP}
	body := probeModelBody(model, kit, tpl)
	req, err := http.NewRequest(http.MethodPost, "http://probe/inference/v1/chat/completions", nil)
	if err != nil {
		res.Err = err.Error()
		return res
	}
	sid, sidSource := t.probeSession(ln)
	slog.Default().Info("bypass probe session", "lane", ln.Proxy, "sid", sid, "source", sidSource)
	start := time.Now()
	resp, err := cliDoUA(req, body, t.upstreamAddr(), ln.Proxy, sid, 8*time.Second, 20*time.Second, 12*time.Second, ua)
	res.MS = time.Since(start).Milliseconds()
	if err != nil {
		res.Err = err.Error()
		return res
	}
	defer resp.Body.Close()
	res.Status = resp.StatusCode
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	b = redactCanonMarkerForOutput(b)
	if s := strings.TrimSpace(string(b)); s != "" {
		if len(s) > 220 {
			s = s[:220]
		}
		res.Body = s
	}
	return res
}

// probeAB is the drift control: baseline, variant, baseline on the SAME lane.
// A variant pass is attributed to the variant (unlocked) only when both
// baselines in the same window refused; any baseline 200 means the lane was
// serving, so the pass is drift and must not be crowned.
func (t *CLITransport) probeAB(model string, v BypassVariant, ln BypassLane, tpl *probeTemplate) BypassLaneResult {
	res := BypassLaneResult{Proxy: ln.Proxy, IP: ln.IP}
	if v.Axis == "base" {
		r := t.probeOnce(model, activeBypassUA(), activeToolKit(), ln, v.ID+"-base", tpl)
		r.Verdict = "base"
		return r
	}
	before := t.probeOnce(model, activeBypassUA(), activeToolKit(), ln, v.ID+"-b1", tpl)
	res.BaseBefore = before.Status
	kit := activeToolKit()
	if v.Axis == "kit" {
		k, err := kitForVariant(v, tpl)
		if err != nil {
			res.Verdict = "error"
			res.Err = err.Error()
			return res
		}
		kit = k
	}
	ua := activeBypassUA()
	if v.Axis == "ua" {
		ua = v.Value
	}
	mid := t.probeOnce(model, ua, kit, ln, v.ID+"-v", tpl)
	res.Status, res.MS, res.Err, res.Body = mid.Status, mid.MS, mid.Err, mid.Body
	switch {
	case mid.Status != 200:
		res.Verdict = "blocked"
	case before.Status == 200:
		res.Verdict = "drift"
	default:
		after := t.probeOnce(model, activeBypassUA(), activeToolKit(), ln, v.ID+"-b2", tpl)
		res.BaseAfter = after.Status
		switch {
		case after.Status == 200:
			res.Verdict = "drift"
		case before.Status == 429 && after.Status == 429:
			res.Verdict = "unlocked"
		default:
			res.Verdict = "uncertain"
		}
	}
	return res
}

// poolGroup / Router exposure for the GUI and the admin endpoint.
func (g *poolGroup) BypassTransport() *CLITransport {
	for _, t := range g.byProv {
		if c, ok := t.(*CLITransport); ok {
			return c
		}
	}
	return nil
}

// BypassTransport returns the CLI lane that can run the manual sweep, or nil
// when the active transport is not the in-process opencode CLI transport.
func (r *Router) BypassTransport() *CLITransport {
	r.mu.RLock()
	tr := r.transport
	r.mu.RUnlock()
	if g, ok := tr.(interface{ BypassTransport() *CLITransport }); ok {
		return g.BypassTransport()
	}
	if c, ok := tr.(*CLITransport); ok {
		return c
	}
	return nil
}

// BypassCatalog lists the probe variants (base, UA bumps, tool kits).
func BypassCatalog() []BypassVariant { return bypassCatalog() }

// CurrentBypass reports the identity tuple the live lane uses right now.
func CurrentBypass() BypassActive { return activeBypass() }

// TemplateInfo describes the latest captured request used as the probe shape.
func TemplateInfo() *BypassTemplate { return loadProbeTemplate().info(activeToolKit()) }
