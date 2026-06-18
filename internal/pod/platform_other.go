//go:build !windows

package pod

import "os/exec"

func OpenTerminal(workspacePath string) {
	exec.Command("x-terminal-emulator", "-e", "whale --cd "+workspacePath).Start()
}
