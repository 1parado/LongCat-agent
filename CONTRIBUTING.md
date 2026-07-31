# 贡献指南（CONTRIBUTING）

感谢你考虑为 **LongCat-frontend** 做出贡献。本文档说明本地开发、构建、测试与提交规范。

## 1. 项目概览

LongCat-frontend 是一个轻量、前端向的 AI Agent，基于 Go + Tauri v2 构建，提供：

- **TUI**：终端交互界面（`internal/ui`）
- **桌面应用**：Tauri v2 外壳（`desktop/src-tauri`）
- **本地 HTTP API + 内嵌 Web UI**：`internal/server`

核心 Agent 循环、工具与安全文件操作位于 `internal/agent`，LLM 供应商适配位于 `internal/llm`。

## 2. 开发环境

- **Go**：1.22+（CI 使用 `go1.26`）
- **Node.js**：用于 Tauri 桌面前端（见 `desktop/`）
- **Rust 工具链**：仅构建桌面端时需要（Tauri v2）
- **推荐**：PowerShell 7 + Windows Terminal（Windows）；pnpm / uv 等按子项目需要

## 3. 构建与运行

### Go 核心（CLI / TUI / API）

```bash
# 本地构建到 bin/ 并直接运行（首次或源码变更后自动构建）
powershell -File ./run.ps1            # 启动 TUI
powershell -File ./run.ps1 serve      # 启动 HTTP API + Web UI

# 或等价地手动构建
go build -ldflags "-s -w" -o bin/LongCat-frontend.exe ./cmd/LongCat-frontend
```

> 也可用 `./run.cmd` 作为 cmd 等价入口。

### 桌面端（Tauri）

```bash
cd desktop
pnpm install
pnpm tauri dev      # 开发模式
pnpm tauri build    # 打包安装包
```

## 4. 测试与代码规范

提交前**必须**通过以下检查（CI 也会执行）：

```bash
gofmt -l .                 # 应输出为空；否则运行 gofmt -w . 格式化
go vet ./...               # 静态检查
go test ./...              # 运行全部 Go 测试
go build ./...             # 确保可编译
```

- 优先使用标准库；小功能不要随意引入新依赖。
- 所有 Agent 文件操作必须限定在激活的工作区内，保留符号链接逃逸检查。
- 用户可见 UI 文案走国际化层（`internal/i18n` 或 Web UI 的 `data-i18n`），**不要**硬编码中文。
- 不要提交 API Key、供应商配置、本地工作区状态或构建产物（见 `.gitignore`）。

## 5. 分支与提交

- 从 `main`（或当前主分支）切出特性分支：`feat/xxx`、`fix/xxx`、`docs/xxx`。
- 提交信息建议遵循约定式提交（Conventional Commits）：
  - `feat:` 新功能
  - `fix:` 缺陷修复
  - `docs:` 文档
  - `refactor:` 重构
  - `test:` 测试
  - `chore:` 杂项
- 保持提交原子化，一个提交只做一件事。

## 6. Pull Request

- 在 PR 描述中说明：解决了什么问题、改动范围、如何验证。
- 确保 CI（build / test / lint）全绿。
- 关联相关 Issue（如 `Closes #123`）。
- 涉及破坏性改动时，请在描述中明确标注并说明迁移方式。

## 7. 报告问题

- 使用 Issue 模板（如适用），提供复现步骤、环境信息（OS / Go 版本）、日志片段。
- 涉及敏感信息（密钥、私有代码路径）请勿公开张贴。

## 8. 行为准则

请保持友善、尊重与就事论事。我们致力于建设一个对所有人友好的协作环境。
