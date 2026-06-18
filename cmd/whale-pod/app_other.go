//go:build !windows

package main

// StartWindowDrag is a no-op on non-Windows platforms.
func (a *App) StartWindowDrag() {}
