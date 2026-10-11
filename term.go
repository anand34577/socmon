//go:build linux

package main

import (
	"os"
	"syscall"
	"unsafe"
)

type winsize struct{ Row, Col, X, Y uint16 }

func termSize() (w, h int) {
	var ws winsize
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, os.Stdout.Fd(), syscall.TIOCGWINSZ, uintptr(unsafe.Pointer(&ws))); e != 0 || ws.Col == 0 {
		return 100, 30
	}
	return int(ws.Col), int(ws.Row)
}

func isTTY() bool {
	for _, f := range []*os.File{os.Stdin, os.Stdout} {
		var t syscall.Termios
		if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), syscall.TCGETS, uintptr(unsafe.Pointer(&t))); e != 0 {
			return false
		}
	}
	return true
}

// makeRaw disables echo and line buffering (ISIG stays on so Ctrl-C works)
// and returns a function that restores the terminal.
func makeRaw() func() {
	var old syscall.Termios
	fd := os.Stdin.Fd()
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TCGETS, uintptr(unsafe.Pointer(&old))); e != 0 {
		return func() {}
	}
	raw := old
	raw.Lflag &^= syscall.ECHO | syscall.ICANON
	raw.Cc[syscall.VMIN], raw.Cc[syscall.VTIME] = 1, 0
	syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TCSETS, uintptr(unsafe.Pointer(&raw)))
	return func() { syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TCSETS, uintptr(unsafe.Pointer(&old))) }
}
