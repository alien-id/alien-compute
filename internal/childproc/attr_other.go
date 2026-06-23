//go:build !linux

package childproc

import "syscall"

func SysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{}
}
