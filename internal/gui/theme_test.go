package gui

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ByJotaro/ai-router/internal/config"
)

func TestAppSetThemeGetTheme(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(`server:
    host: 127.0.0.1
    port: 9001
admin_token: abc123
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	app := NewApp(fakeRouter(t, nil), nil, cfg, path, "test", testLog())

	if got, err := app.GetTheme(); err != nil || got != "" {
		t.Fatalf("GetTheme before SetTheme = %q, %v; want \"\", nil", got, err)
	}
	if err := app.SetTheme("nebula"); err != nil {
		t.Fatalf("SetTheme(nebula): %v", err)
	}
	got, err := app.GetTheme()
	if err != nil || got != "nebula" {
		t.Fatalf("GetTheme after SetTheme = %q, %v; want nebula", got, err)
	}
	// Legacy mist маппится на nebula.
	if err := app.SetTheme("mist"); err != nil {
		t.Fatalf("SetTheme(mist legacy): %v", err)
	}
	got, err = app.GetTheme()
	if err != nil || got != "nebula" {
		t.Fatalf("GetTheme after legacy mist = %q, %v; want nebula", got, err)
	}
	// Переживает перезагрузку конфига (читается с диска).
	cfg2, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	app2 := NewApp(fakeRouter(t, nil), nil, cfg2, path, "test", testLog())
	got, err = app2.GetTheme()
	if err != nil || got != "nebula" {
		t.Fatalf("GetTheme after reload = %q, %v; want nebula", got, err)
	}
	// Ember — полноценная тема с 2026-09-26 (дефолт главной).
	if err := app2.SetTheme("ember"); err != nil {
		t.Fatalf("SetTheme(ember): %v", err)
	}
	got, err = app2.GetTheme()
	if err != nil || got != "ember" {
		t.Fatalf("GetTheme after SetTheme ember = %q, %v; want ember", got, err)
	}
	if err := app2.SetTheme("abyss"); err != nil {
		t.Fatalf("SetTheme(abyss): %v", err)
	}
	// Невалидная тема отклоняется.
	if err := app2.SetTheme("sunset"); err == nil {
		t.Fatal("SetTheme(sunset) must fail")
	}
}
