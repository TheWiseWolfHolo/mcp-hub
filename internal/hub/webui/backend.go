package webui

import (
	"context"
	"encoding/json"
	"errors"
)

// ErrReloadNotSupported may be returned by Backend.ReloadService when a service
// does not configure a reload endpoint.
var ErrReloadNotSupported = errors.New("service does not support reload")

// Event represents a real-time hub lifecycle notification pushed over SSE.
type Event struct {
	Type      string `json:"type"`
	Service   string `json:"service,omitempty"`
	Message   string `json:"message,omitempty"`
	Timestamp string `json:"timestamp"`
}

// LogEntry represents one redacted log line from the hub or a managed service.
type LogEntry struct {
	Seq       uint64 `json:"seq,omitempty"`
	Timestamp string `json:"timestamp"`
	Stream    string `json:"stream"`
	Line      string `json:"line"`
}

// ToolInfo describes one MCP tool returned by a live tools/list probe.
type ToolInfo struct {
	Name         string          `json:"name"`
	Description  string          `json:"description,omitempty"`
	ReadOnlyHint *bool           `json:"readOnlyHint,omitempty"`
	InputSchema  json.RawMessage `json:"inputSchema,omitempty"`
}

// ToolsProbeResult captures the outcome of probing one service's MCP tools/list endpoint.
type ToolsProbeResult struct {
	Service   string     `json:"service"`
	Endpoint  string     `json:"endpoint,omitempty"`
	Tools     []ToolInfo `json:"tools"`
	Skipped   bool       `json:"skipped,omitempty"`
	Reason    string     `json:"reason,omitempty"`
	LatencyMs int64      `json:"latencyMs,omitempty"`
}

// Backend is the narrow integration contract implemented by the hub control plane
// so package webui remains decoupled from package hub without circular imports.
type Backend interface {
	VerifyControlToken(token string) bool
	AllowNoAuth() bool
	StatusJSON() ([]byte, error)
	CatalogJSON() ([]byte, error)
	HealthzJSON() ([]byte, error)
	LogsTail(name string, lines int) ([]LogEntry, bool, error)
	SubscribeLogs(name string) (<-chan LogEntry, func(), bool)
	SubscribeEvents() (<-chan Event, func())
	StartService(ctx context.Context, name string) ([]byte, bool, error)
	StopService(ctx context.Context, name string) ([]byte, bool, error)
	RestartService(ctx context.Context, name string, waitHealthy bool) ([]byte, bool, error)
	ReloadService(ctx context.Context, name string) ([]byte, bool, error)
	SleepService(ctx context.Context, name string) ([]byte, bool, error)
	WakeService(ctx context.Context, name string) ([]byte, bool, error)
	StartAll(ctx context.Context) ([]byte, error)
	StopAll(ctx context.Context) ([]byte, error)
	RestartAll(ctx context.Context) ([]byte, error)
	ReloadHub(ctx context.Context) ([]byte, error)
	ProbeTools(ctx context.Context, service string, wake bool) (ToolsProbeResult, error)
	ClientSnippet(service, client string) (string, error)
	Redact(s string) string
}
