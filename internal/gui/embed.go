package gui

import "embed"

// frontendAssets — статический фронт (без npm-сборки): wails build копирует
// frontend/dist в бинарь; runtime wails инжектится сам.
//
//go:embed all:frontend/dist
var frontendAssets embed.FS
