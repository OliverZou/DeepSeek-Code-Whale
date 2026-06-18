//go:build windows

package main

import (
	"syscall"
	"unsafe"
)

var (
	user32              = syscall.NewLazyDLL("user32.dll")
	procFindWindowW      = user32.NewProc("FindWindowW")
	procReleaseCapture   = user32.NewProc("ReleaseCapture")
	procSendMessage      = user32.NewProc("SendMessageW")
	procGetForegroundWindow = user32.NewProc("GetForegroundWindow")
)

const (
	WM_NCLBUTTONDOWN = 0x00A1
	HTCAPTION        = 0x0002
)

// StartWindowDrag triggers native window dragging via Win32 API.
func (a *App) StartWindowDrag() {
	// Get the foreground window (our Wails window).
	hwnd, _, _ := procGetForegroundWindow.Call()
	if hwnd == 0 {
		return
	}
	procReleaseCapture.Call()
	procSendMessage.Call(
		hwnd,
		uintptr(WM_NCLBUTTONDOWN),
		uintptr(HTCAPTION),
		uintptr(0),
	)
	// Keep compiler happy for unsafe.
	_ = unsafe.Sizeof(0)
}
