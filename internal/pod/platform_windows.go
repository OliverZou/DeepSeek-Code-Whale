//go:build windows

package pod

import "os/exec"

func OpenTerminal(workspacePath string) {
	exec.Command("cmd", "/c", "start", "whale", "--cd", workspacePath).Start()
}
