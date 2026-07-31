# LongCat-frontend Agent Knowledge

LongCat-frontend is a lightweight frontend-focused AI Agent built with Go and
Tauri v2. It provides a TUI, desktop app, and local HTTP API with an embedded
Web UI.

## Architecture

- `cmd/LongCat-frontend/` contains the CLI entry point and subcommands.
- `internal/agent/` contains the agent loop, workspace-safe tools, skills, and
  sub-agent orchestration.
- `internal/llm/` contains provider management and OpenAI, Anthropic, Ollama,
  and Responses protocol adapters.
- `internal/server/` contains the local HTTP API and embedded Web UI.
- `internal/workspace/` contains workspace/session persistence, file watching,
  diff generation, and undo history.
- `internal/mcp/` contains MCP configuration, health checks, tool discovery,
  and tool routing.
- `internal/cache/` contains reusable in-memory TTL/LRU caches.
- `internal/frontend/` and `frontend-skills/` contain skill loading and skill
  definitions.
- `desktop/src-tauri/` contains the Tauri v2 desktop shell.

## Development rules

- Keep all agent file operations inside the active workspace. Preserve the
  symlink escape checks when changing tools.
- Use the standard library where practical; avoid adding dependencies for a
  small isolated feature.
- User-facing UI copy belongs in the Web UI message catalog or `internal/i18n`.
- Run `gofmt` and `go test ./...` after Go changes.
- Do not commit API keys, provider configuration, local workspace state, or
  build artifacts.
