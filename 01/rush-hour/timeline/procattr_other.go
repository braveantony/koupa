//go:build !linux

package timeline

import "syscall"

func childAttr() *syscall.SysProcAttr { return nil }
