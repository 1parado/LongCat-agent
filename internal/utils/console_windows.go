//go:build windows

package utils

import (
	"errors"
	"os"
	"strconv"
	"syscall"
	"unsafe"
)

var (
	kernel32                       = syscall.NewLazyDLL("kernel32.dll")
	procGetConsoleMode             = kernel32.NewProc("GetConsoleMode")
	procSetConsoleMode             = kernel32.NewProc("SetConsoleMode")
	procSetConsoleOutputCP         = kernel32.NewProc("SetConsoleOutputCP")
	procSetConsoleCP               = kernel32.NewProc("SetConsoleCP")
	procGetConsoleScreenBufferInfo = kernel32.NewProc("GetConsoleScreenBufferInfo")
)

const (
	enableVirtualTerminalProcessing = 0x0004
	enableVirtualTerminalInput      = 0x0200
	enableLineInput                 = 0x0002
	enableEchoInput                 = 0x0004
	enableProcessedInput            = 0x0001
	cpUTF8                          = 65001
)

// EnableConsole 让 cmd.exe / PowerShell 5 / PowerShell 7 / Windows Terminal
// 都能正确渲染 ANSI 转义序列与 UTF-8 字符。
//
//   - cmd.exe 与 PowerShell 5 默认不开启 VT 处理，这里通过
//     SetConsoleMode(ENABLE_VIRTUAL_TERMINAL_PROCESSING) 显式开启；
//   - 同时把输入/输出代码页切到 UTF-8，避免中文与 emoji 乱码。
//
// 返回 true 表示 ANSI 可用。
func EnableConsole() bool {
	procSetConsoleOutputCP.Call(uintptr(cpUTF8))
	procSetConsoleCP.Call(uintptr(cpUTF8))

	ok := true
	for _, f := range []*os.File{os.Stdout, os.Stderr} {
		h := syscall.Handle(f.Fd())
		var mode uint32
		r, _, _ := procGetConsoleMode.Call(uintptr(h), uintptr(unsafe.Pointer(&mode)))
		if r == 0 {
			// 非控制台（重定向/管道），无需处理。
			continue
		}
		if mode&enableVirtualTerminalProcessing == 0 {
			r, _, _ = procSetConsoleMode.Call(uintptr(h), uintptr(mode|enableVirtualTerminalProcessing))
			if r == 0 {
				ok = false
			}
		}
	}
	return ok
}

// EnableRaw 切换到原始输入模式：关闭行编辑、回显与处理，开启 stdin 的 VT
// 输入，使方向键等以 ANSI 转义序列返回。返回用于恢复的闭包。
//
// 若 stdin 不是控制台（管道/重定向），无法开启原始模式，返回空恢复函数。
// 若 SetConsoleMode 被拒绝或未生效，返回错误，调用方应回退到行模式。
func EnableRaw() (func(), error) {
	hIn := syscall.Handle(os.Stdin.Fd())
	var mode uint32
	r, _, _ := procGetConsoleMode.Call(uintptr(hIn), uintptr(unsafe.Pointer(&mode)))
	if r == 0 {
		// 非控制台：无需处理。
		return func() {}, nil
	}
	saved := mode
	raw := mode &^ (enableLineInput | enableEchoInput | enableProcessedInput)
	raw |= enableVirtualTerminalInput
	r2, _, _ := procSetConsoleMode.Call(uintptr(hIn), uintptr(raw))
	if r2 == 0 {
		return func() {}, errors.New("SetConsoleMode 被拒绝")
	}
	// 验证 mode 确实改变（部分宿主可能拒绝但仍返回非零）
	var verify uint32
	procGetConsoleMode.Call(uintptr(hIn), uintptr(unsafe.Pointer(&verify)))
	if verify&enableLineInput != 0 || verify&enableEchoInput != 0 {
		return func() {}, errors.New("raw mode 未生效（行输入/回显仍开启）")
	}
	return func() {
		procSetConsoleMode.Call(uintptr(hIn), uintptr(saved))
	}, nil
}

type coord struct{ X, Y int16 }

type smallRect struct{ Left, Top, Right, Bottom int16 }

type consoleScreenBufferInfo struct {
	dwSize              coord
	dwCursorPosition    coord
	wAttributes         uint16
	srWindow            smallRect
	dwMaximumWindowSize coord
}

// Size 返回终端宽度与高度（字符数）。非控制台时回退到环境变量或默认 80x24。
func Size() (width, height int) {
	hOut := syscall.Handle(os.Stdout.Fd())
	var info consoleScreenBufferInfo
	r, _, _ := procGetConsoleScreenBufferInfo.Call(uintptr(hOut), uintptr(unsafe.Pointer(&info)))
	if r != 0 {
		w := int(info.srWindow.Right - info.srWindow.Left + 1)
		h := int(info.srWindow.Bottom - info.srWindow.Top + 1)
		if w > 0 && h > 0 {
			return w, h
		}
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
