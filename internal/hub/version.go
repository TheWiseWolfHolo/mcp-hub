package hub

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const (
	Version            = "0.3.0"
	DefaultControlAddr = "127.0.0.1:18470"
	DefaultGatewayAddr = "127.0.0.1:18471"
	DefaultLogMaxBytes = 5 * 1024 * 1024 // 5 MiB
	DefaultMutexName   = `Local\McpHubSupervisor`
)

// GitCommit may be injected at build time via -ldflags "-X <module>/internal/hub.GitCommit=<sha>".
var GitCommit = ""

// DefaultConfigPath and DefaultLogDir are resolved portably at startup using environment
// overrides, an adjacent installation.json receipt, or the platform user config directory.
var (
	DefaultConfigPath = ResolveDefaultConfigPath()
	DefaultLogDir     = ResolveDefaultLogDir()
)

var executablePathFn = os.Executable

// ResolveDefaultConfigPath resolves the default config.json path in precedence order:
//  1. MCP_HUB_CONFIG environment variable (if non-empty)
//  2. installation.json adjacent to the current executable (config_path field)
//  3. OS-appropriate user config directory (%LOCALAPPDATA%\McpHub\config.json on Windows;
//     $XDG_CONFIG_HOME/mcp-hub/config.json or os.UserConfigDir()/mcp-hub/config.json on Unix).
func ResolveDefaultConfigPath() string {
	return resolveDefaultConfigPath(runtime.GOOS, executablePathFn)
}

// ResolveDefaultLogDir resolves the default log directory in precedence order:
//  1. MCP_HUB_LOG_DIR environment variable (if non-empty)
//  2. <dir of ResolveDefaultConfigPath()>/logs
func ResolveDefaultLogDir() string {
	if envDir := strings.TrimSpace(os.Getenv("MCP_HUB_LOG_DIR")); envDir != "" {
		return filepath.Clean(envDir)
	}
	return filepath.Join(filepath.Dir(ResolveDefaultConfigPath()), "logs")
}

func resolveDefaultConfigPath(goos string, exeFn func() (string, error)) string {
	if envCfg := strings.TrimSpace(os.Getenv("MCP_HUB_CONFIG")); envCfg != "" {
		return filepath.Clean(envCfg)
	}

	if exeFn != nil {
		if exePath, err := exeFn(); err == nil && strings.TrimSpace(exePath) != "" {
			if resolved, err := filepath.EvalSymlinks(exePath); err == nil && strings.TrimSpace(resolved) != "" {
				exePath = resolved
			}
			receiptPath := filepath.Join(filepath.Dir(exePath), "installation.json")
			if cfgPath := readInstallReceiptConfigPath(receiptPath); cfgPath != "" {
				return filepath.Clean(cfgPath)
			}
		}
	}

	if goos == "windows" {
		if localAppData := strings.TrimSpace(os.Getenv("LOCALAPPDATA")); localAppData != "" {
			return filepath.Join(localAppData, "McpHub", "config.json")
		}
		if cfgDir, err := os.UserConfigDir(); err == nil && strings.TrimSpace(cfgDir) != "" {
			return filepath.Join(cfgDir, "McpHub", "config.json")
		}
		return filepath.Join("McpHub", "config.json")
	}

	if xdg := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); xdg != "" {
		return filepath.Join(xdg, "mcp-hub", "config.json")
	}
	if cfgDir, err := os.UserConfigDir(); err == nil && strings.TrimSpace(cfgDir) != "" {
		return filepath.Join(cfgDir, "mcp-hub", "config.json")
	}
	if home, err := os.UserHomeDir(); err == nil && strings.TrimSpace(home) != "" {
		return filepath.Join(home, ".config", "mcp-hub", "config.json")
	}
	return filepath.Join("mcp-hub", "config.json")
}

func readInstallReceiptConfigPath(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	// Strip UTF-8 BOM if present.
	if len(raw) >= 3 && raw[0] == 0xEF && raw[1] == 0xBB && raw[2] == 0xBF {
		raw = raw[3:]
	}
	var receipt struct {
		ConfigPath      string `json:"config_path"`
		ConfigPathCamel string `json:"configPath"`
	}
	if err := json.Unmarshal(raw, &receipt); err != nil {
		return ""
	}
	if p := strings.TrimSpace(receipt.ConfigPath); p != "" {
		return p
	}
	return strings.TrimSpace(receipt.ConfigPathCamel)
}
