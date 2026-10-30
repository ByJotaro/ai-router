// Package modmgmt manages dependent module processes (egress / warp-router /
// hub) for the omnigate GUI: health checks, auto-start on toggle, and process
// launch/stop. Toggle-off never stops a process that was already running
// externally — it only clears the "auto-start" flag.
package modmgmt

import (
	"fmt"
	"log"
	"net/http"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"
)

// Module is a runtime-tracked dependent module.
type Module struct {
	Name   string
	Desc   string // что этот модуль даёт omnigate («требуется модуль X для …»)
	Health string // http(s) URL для probe, "" — не проверяется
	Bin    string // путь к исполняемому файлу
	Args   []string
	Auto   bool // авто-старт при включении
}

// Manager tracks modules and their processes.
type Manager struct {
	mu      sync.Mutex
	modules map[string]*Module
	procs   map[string]*exec.Cmd
	log     *log.Logger
	client  *http.Client
}

// New creates a Manager with default module definitions.
func New(log *log.Logger) *Manager {
	return &Manager{
		modules: map[string]*Module{},
		procs:   map[string]*exec.Cmd{},
		log:     log,
		client:  &http.Client{Timeout: 3 * time.Second},
	}
}

// SetAuto updates the auto-start flag and reports unknown module names.
func (m *Manager) SetAuto(name string, auto bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	mod, ok := m.modules[name]
	if !ok {
		return fmt.Errorf("модуль %s не зарегистрирован", name)
	}
	mod.Auto = auto
	return nil
}

// Upsert registers or updates a module definition.
func (m *Manager) Upsert(mod *Module) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.modules[mod.Name] = mod
}

// Status reports each module's state: online if its health probe answers.
func (m *Manager) Status() []map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	names := make([]string, 0, len(m.modules))
	for n := range m.modules {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]map[string]any, 0, len(names))
	for _, n := range names {
		mod := m.modules[n]
		online := m.probe(mod)
		out = append(out, map[string]any{
			"name": mod.Name, "desc": mod.Desc, "auto": mod.Auto,
			"online": online, "running": m.procs[n] != nil,
			"health": mod.Health, "bin": mod.Bin,
		})
	}
	return out
}

func (m *Manager) probe(mod *Module) bool {
	if mod.Health == "" {
		return false
	}
	res, err := m.client.Get(mod.Health)
	if err != nil {
		return false
	}
	res.Body.Close()
	return res.StatusCode < 500
}

// Start launches the module's binary detached. Idempotent: if a process is
// already tracked, it reports that instead of double-launching.
func (m *Manager) Start(name string) error {
	m.mu.Lock()
	mod, ok := m.modules[name]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("модуль %s не зарегистрирован", name)
	}
	if m.procs[name] != nil {
		m.mu.Unlock()
		return fmt.Errorf("модуль %s уже запущен этим менеджером", name)
	}
	if mod.Bin == "" {
		m.mu.Unlock()
		return fmt.Errorf("модуль %s: не задан путь к бинарю", name)
	}
	cmd := exec.Command(mod.Bin, mod.Args...)
	m.procs[name] = cmd
	m.mu.Unlock()
	if err := cmd.Start(); err != nil {
		m.mu.Lock()
		delete(m.procs, name)
		m.mu.Unlock()
		return fmt.Errorf("запуск %s: %w", name, err)
	}
	m.log.Printf("modmgmt: %s started (pid %d)", name, cmd.Process.Pid)
	go func() {
		_ = cmd.Wait()
		m.mu.Lock()
		if m.procs[name] == cmd {
			delete(m.procs, name)
		}
		m.mu.Unlock()
		m.log.Printf("modmgmt: %s exited", name)
	}()
	return nil
}

// Stop terminates a module this manager started. Modules the user launched
// externally are never stopped here (that is the caller's policy).
func (m *Manager) Stop(name string) error {
	m.mu.Lock()
	cmd, ok := m.procs[name]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("модуль %s не запускался этим менеджером", name)
	}
	if cmd != nil && cmd.Process != nil {
		if err := cmd.Process.Kill(); err != nil && !strings.Contains(err.Error(), "process already finished") {
			return fmt.Errorf("остановка %s: %w", name, err)
		}
	}
	m.log.Printf("modmgmt: %s stopped", name)
	return nil
}
