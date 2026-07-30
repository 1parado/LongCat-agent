//go:build linux

package utils

import (
	"os"
	"strconv"
	"syscall"
	"unsafe"
)

const (
	tcGetAttr = 0x5401 // TCGETS
	tcSetAttr = 0x5402 // TCSETS
	tiocGWinsz = 0x5413 // TIOCGWINSZ
)

// EnableConsole 在非 Windows 平台上无需处理，终端天然支持 ANSI。
func EnableConsole() bool { return true }

// EnableRaw 通过 termios 关闭规范模式与回显，进入原始输入。
func EnableRaw() (func(), error) {
	fd := int(os.Stdin.Fd())
	var term syscall.Termios
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), tcGetAttr, uintptr(unsafe.Pointer(&term)))
	if errno != 0 {
		return func() {}, nil
	}
	saved := term
	term.Lflag &^= syscall.ICANON | syscall.ECHO | syscall.ISIG
	term.Oflag &^= syscall.OPOST
	term.Cc[syscall.VMIN] = 1
	term.Cc[syscall.VTIME] = 0
	syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), tcSetAttr, uintptr(unsafe.Pointer(&term)))
	return func() {
		syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), tcSetAttr, uintptr(unsafe.Pointer(&saved)))
	}, nil
}

type winsize struct {
	Rows, Cols, Xpix, Ypix uint16
}

// Size 返回终端宽高。失败时回退到环境变量或默认 80x24。
func Size() (width, height int) {
	fd := int(os.Stdout.Fd())
	var ws winsize
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), tiocGWinsz, uintptr(unsafe.Pointer(&ws)))
	if errno == 0 && ws.Cols > 0 && ws.Rows > 0 {
		return int(ws.Cols), int(ws.Rows)
	}
	if c := os.Getenv("COLUMNS"); c != "" {
		if v, err := strconv.Atoi(c); err == nil && v > 0 {
			width = v
		}
	}
	if c := os.Getenv("LINES"); c != "" {
		if v, err := strconv.Atoi(c); err == nil && v > 0 {
			height = v
		}
	}
	if width == 0 {
		width = 80
	}
	if height == 0 {
		height = 24
	}
	return width, height
}
