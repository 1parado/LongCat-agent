//go:build !windows

package utils

// EnableConsole 在非 Windows 平台上无需处理，终端天然支持 ANSI。
func EnableConsole() bool { return true }
