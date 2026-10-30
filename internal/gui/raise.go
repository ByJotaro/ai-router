package gui

import (
	"fmt"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32dll               = windows.NewLazySystemDLL("user32.dll")
	procEnumWindows         = user32dll.NewProc("EnumWindows")
	procGetWindowTextW      = user32dll.NewProc("GetWindowTextW")
	procGetWindowProcId     = user32dll.NewProc("GetWindowThreadProcessId")
	procShowWindowAsync     = user32dll.NewProc("ShowWindowAsync")
	procSetForegroundWindow = user32dll.NewProc("SetForegroundWindow")
)

const (
	swShow    = 5
	swRestore = 9
)

// isPanelProcess: hwnd принадлежит процессу ai-router.exe (а не браузеру с
// «ai-router» в заголовке вкладки).
func isPanelProcess(hwnd uintptr) bool {
	var pid uint32
	procGetWindowProcId.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
	if pid == 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	buf := make([]uint16, windows.MAX_PATH)
	size := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &size); err != nil {
		return false
	}
	exe := strings.ToLower(windows.UTF16ToString(buf[:size]))
	// Промоут кладёт сборку под версионным именем ai-router.<yyyyMMdd_HHmmss>.exe
	// рядом с ai-router.exe, и живой процесс — именно версионный. Матч по
	// префиксу базового имени: `ai-router.exe` и `ai-router.20261021_122452.exe`
	// оба считаются панелью, иначе второй клик по exe уходит в «уже запущен».
	base := exe
	if i := strings.LastIndexByte(exe, '\\'); i >= 0 {
		base = exe[i+1:]
	}
	if !strings.HasPrefix(base, "ai-router") || !strings.HasSuffix(base, ".exe") {
		return false
	}
	return !strings.Contains(strings.TrimSuffix(strings.TrimPrefix(base, "ai-router"), ".exe"), ".")

}

// RaiseExistingWindow ищет живое окно панели (процесс ai-router.exe на этом
// рабочем столе: видимое или скрытое/свёрнутое) и поднимает его на передний
// план. true — окно найдено и показано.
// Нужен второму запуску exe: двойной клик по exe обязан открыть панель, а не
// молча умереть на занятом порту (2026-10-19: юзер кликал — панель не
// появлялась; демон жил хедлессом после смерти окна).
func RaiseExistingWindow() bool {
	found := false
	cb := syscall.NewCallback(func(hwnd uintptr, _ uintptr) uintptr {
		if found || !isPanelProcess(hwnd) {
			return 1
		}
		var buf [256]uint16
		n, _, _ := procGetWindowTextW.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
		if n == 0 {
			return 1
		}
		title := syscall.UTF16ToString(buf[:n])
		if !strings.Contains(title, "ai-router") {
			return 1
		}
		procShowWindowAsync.Call(hwnd, swRestore)
		procShowWindowAsync.Call(hwnd, swShow)
		procSetForegroundWindow.Call(hwnd)
		found = true
		return 0
	})
	procEnumWindows.Call(cb, 0)
	return found
}

// ShowAlreadyRunning — фолбэк, когда окна нет (демон живёт без GUI):
// диалог, чтобы двойной клик по exe никогда не заканчивался «ничем».
func ShowAlreadyRunning(addr string) {
	text := fmt.Sprintf("ai-router уже запущен на %s, но его панель недоступна (процесс живёт без GUI).\n\nЗавершите старый процесс ai-router*.exe и запустите снова.", addr)
	caption, _ := windows.UTF16PtrFromString("ai-router")
	body, _ := windows.UTF16PtrFromString(text)
	_, _ = windows.MessageBox(0, body, caption, windows.MB_OK|windows.MB_ICONINFORMATION)
}
