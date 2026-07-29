@echo off
rem LongCat-frontend - cmd.exe 启动脚本
rem 用法:  run.cmd            启动 TUI
rem        run.cmd serve      启动桌面后端 (HTTP API + Web UI)
rem        run.cmd provider list
setlocal
set "ROOT=%~dp0"
set "BIN=%ROOT%bin\LongCat-frontend.exe"

if not exist "%BIN%" (
    echo [build] 正在构建 LongCat-frontend...
    pushd "%ROOT%"
    go build -ldflags "-s -w" -o "%BIN%" ./cmd/LongCat-frontend
    if errorlevel 1 (
        popd
        echo [error] go build 失败
        exit /b 1
    )
    popd
)

"%BIN%" %*
exit /b %errorlevel%
