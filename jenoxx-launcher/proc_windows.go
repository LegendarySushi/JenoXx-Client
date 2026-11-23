//go:build windows

package main

import (
	"os/exec"
	"syscall"
)

// hideWindow verhindert ein aufpoppendes Konsolenfenster beim Starten von Gradle.
func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
}
