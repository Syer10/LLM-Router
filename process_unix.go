//go:build !windows

package main

import (
	"syscall"
	"time"
)

// procGroupAttr puts the child in its own process group so the whole tree
// can be signalled at once.
func procGroupAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

// killProcessTree sends SIGTERM to the process group, then SIGKILL after a
// grace period.
func killProcessTree(pid int) error {
	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil {
		return err
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(-pid, 0); err != nil {
			return nil // group is gone
		}
		time.Sleep(200 * time.Millisecond)
	}
	return syscall.Kill(-pid, syscall.SIGKILL)
}
