//go:build windows

package main

import (
	"os/exec"
	"strconv"
	"syscall"
)

// procGroupAttr gives the child its own process group (CREATE_NEW_PROCESS_GROUP).
func procGroupAttr() *syscall.SysProcAttr {
	const createNewProcessGroup = 0x00000200
	return &syscall.SysProcAttr{CreationFlags: createNewProcessGroup}
}

// killProcessTree uses taskkill to terminate the whole process tree.
func killProcessTree(pid int) error {
	return exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(pid)).Run()
}
