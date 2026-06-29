//go:build windows

package cmd

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"unsafe"
)

var (
	kernel32          = syscall.NewLazyDLL("kernel32.dll")
	procOpenProcess   = kernel32.NewProc("OpenProcess")
	procTerminateProc = kernel32.NewProc("TerminateProcess")
	procCloseHandle   = kernel32.NewProc("CloseHandle")
)

const (
	processTerminate = 0x0001
	processQueryInfo = 0x0400
)

func readPIDFile(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(data)))
}

func isProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, _, _ := procOpenProcess.Call(processQueryInfo, 0, uintptr(pid))
	if h == 0 {
		return false
	}
	procCloseHandle.Call(h)
	return true
}

func terminateProcess(pid int) error {
	h, _, _ := procOpenProcess.Call(processTerminate, 0, uintptr(pid))
	if h == 0 {
		return fmt.Errorf("open process %d: %v", pid, syscall.GetLastError())
	}
	defer procCloseHandle.Call(h)

	ret, _, _ := procTerminateProc.Call(h, 0)
	if ret == 0 {
		return fmt.Errorf("terminate process %d: %v", pid, syscall.GetLastError())
	}
	return nil
}

func killProcess(pid int) error {
	// On Windows, TerminateProcess is equivalent to SIGKILL.
	return terminateProcess(pid)
}

// Ensure syscall is used.
var _ = unsafe.Pointer(nil)
