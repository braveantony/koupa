//go:build !linux

package timeline

import "syscall"

func childAttr() *syscall.SysProcAttr { return nil }

// childExited cannot peek without reaping here; the child counts as running.
func childExited(pid int) bool { return false }
