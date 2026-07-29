# LongCat-frontend - PowerShell 启动脚本
# 用法:  .\run.ps1            # 启动 TUI
#        .\run.ps1 serve      # 启动桌面后端 (HTTP API + Web UI)
#        .\run.ps1 provider list
[CmdletBinding()]
param(
    [Parameter(ValueFromRemainingArguments = $true)]
    [string[]]$AgentArgs
)

$ErrorActionPreference = "Stop"
$root = $PSScriptRoot
$bin = Join-Path $root "bin\LongCat-frontend.exe"

# 需要重新编译时自动构建
$needBuild = -not (Test-Path $bin)
if (-not $needBuild) {
    $binTime = (Get-Item $bin).LastWriteTime
    $srcNewer = Get-ChildItem -Path $root -Recurse -Include *.go |
        Where-Object { $_.LastWriteTime -gt $binTime } |
        Select-Object -First 1
    if ($srcNewer) { $needBuild = $true }
}

if ($needBuild) {
    Write-Host "⚙  正在构建 LongCat-frontend..." -ForegroundColor Cyan
    Push-Location $root
    try {
        go build -ldflags "-s -w" -o $bin ./cmd/LongCat-frontend
        if ($LASTEXITCODE -ne 0) { throw "go build 失败" }
    }
    finally { Pop-Location }
}

& $bin @AgentArgs
exit $LASTEXITCODE
