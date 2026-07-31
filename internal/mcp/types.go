// Package mcp contains the local MCP registry and a small HTTP JSON-RPC
// client. It intentionally keeps the protocol surface narrow so a project can
// connect to servers without pulling a full SDK into the desktop binary.
package mcp

import "time"

type HealthTone string

const (
	HealthOK           HealthTone = "ok"
	HealthWarn         HealthTone = "warn"
	HealthError        HealthTone = "error"
	HealthAuthRequired HealthTone = "auth_required"
	HealthUnknown      HealthTone = "unknown"
)

type MCPTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"inputSchema,omitempty"`
}

type MCPServer struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	URL       string            `json:"url"`
	Protocol  string            `json:"protocol"` // http, sse, stdio
	Headers   map[string]string `json:"headers,omitempty"`
	Tools     []MCPTool         `json:"tools,omitempty"`
	Healthy   bool              `json:"healthy"`
	Tone      HealthTone        `json:"tone"`
	Hint      string            `json:"hint,omitempty"`
	LastCheck time.Time         `json:"last_check,omitempty"`
	LastError string            `json:"last_error,omitempty"`
}

type configFile struct {
	Servers []MCPServer `json:"servers"`
}

type DoctorReport struct {
	ServerID string     `json:"server_id"`
	Tone     HealthTone `json:"tone"`
	Message  string     `json:"message"`
	Hint     string     `json:"hint,omitempty"`
	At       time.Time  `json:"at"`
}
