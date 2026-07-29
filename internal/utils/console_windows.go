//go:build windows

package utils

import (
	"os"
	"syscall"
	"unsafe"
)

var (
	kernel32               = syscall.NewLazyDLL("kernel32.dll")
	procGetConsoleMode     = kernel32.NewProc("GetConsoleMode")
	procSetConsoleMode     = kernel32.NewProc("SetConsoleMode")
	procSetConsoleOutputCP = kernel32.NewProc("SetConsoleOutputCP")
	procSetConsoleCP       = kernel32.NewProc("SetConsoleCP")
)

const (
	enableVirtualTerminalProcessing = 0x0004
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
