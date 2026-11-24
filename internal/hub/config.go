package hub

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	maxSecretFileBytes            = 8 << 10 // 8 KiB
	defaultStartTimeoutSec        = 15
	defaultMaxConsecutiveFailures = 5
	defaultClientPath             = "/mcp"
)

var validNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)

// Config defines the complete mcp-hub supervisor, gateway, and reverse proxy configuration.
type Config struct {
	ControlAddr                string          `json:"controlAddr,omitempty"`
	ControlTokenFile           string          `json:"controlTokenFile,omitempty"`
	AllowInsecureNoControlAuth bool            `json:"allowInsecureNoControlAuth,omitempty"`
	LogDir                     string          `json:"logDir,omitempty"`
	LogMaxBytes                int64           `json:"logMaxBytes,omitempty"`
	Tray                       *bool           `json:"tray,omitempty"`
	Gateway                    *GatewayConfig  `json:"gateway,omitempty"`
	Services                   []ServiceConfig `json:"services"`
	Proxies                    []ProxyConfig   `json:"proxies"`
}

// ShouldShowTray reports whether the Windows System Tray icon should be displayed (default true).
func (c *Config) ShouldShowTray() bool {
	if c == nil || c.Tray == nil {
		return true
	}
	return *c.Tray
}

// RouteTokenConfig defines one Bearer token profile with optional per-token MCP tool ACLs.
type RouteTokenConfig struct {
	Name         string   `json:"name,omitempty"`
	TokenFile    string   `json:"tokenFile"`
	AllowTools   []string `json:"allowTools,omitempty"`
	DenyTools    []string `json:"denyTools,omitempty"`
	ReadOnlyOnly bool     `json:"readOnlyOnly,omitempty"`
}

// GatewayConfig defines the unified single-port loopback gateway serving /s/<slug>/*.
type GatewayConfig struct {
	ListenAddr   string               `json:"listenAddr,omitempty"`
	AllowedHosts []string             `json:"allowedHosts,omitempty"`
	Routes       []GatewayRouteConfig `json:"routes"`
}

// GatewayRouteConfig maps one /s/<slug> gateway path to either a stdio-bridge or an HTTP upstream.
type GatewayRouteConfig struct {
	Slug              string             `json:"slug"`
	Service           string             `json:"service,omitempty"`
	TargetURL         string             `json:"targetUrl,omitempty"`
	TokenFile         string             `json:"tokenFile,omitempty"`
	Tokens            []RouteTokenConfig `json:"tokens,omitempty"`
	UpstreamTokenFile string             `json:"upstreamTokenFile,omitempty"`
	HealthPath        string             `json:"healthPath,omitempty"`
	AllowNonLoopback  bool               `json:"allowNonLoopback,omitempty"`
	Description       string             `json:"description,omitempty"`
	ClientPath        string             `json:"clientPath,omitempty"`
}

// EffectiveClientPath returns the client MCP endpoint path suffix (default "/mcp").
func (r GatewayRouteConfig) EffectiveClientPath() string {
	if strings.TrimSpace(r.ClientPath) != "" {
		return strings.TrimSpace(r.ClientPath)
	}
	return defaultClientPath
}

// ServiceConfig defines one managed child MCP daemon or STDIO-to-HTTP bridge.
type ServiceConfig struct {
	Name                     string            `json:"name"`
	Mode                     string            `json:"mode,omitempty"`
	StartMode                string            `json:"startMode,omitempty"`
	Command                  string            `json:"command"`
	Args                     []string          `json:"args,omitempty"`
	Cwd                      string            `json:"cwd,omitempty"`
	Env                      map[string]string `json:"env,omitempty"`
	EnvFiles                 map[string]string `json:"envFiles,omitempty"`
	HealthURL                string            `json:"healthUrl,omitempty"`
	HealthIntervalSec        int               `json:"healthIntervalSec,omitempty"`
	StripProxyEnv            *bool             `json:"stripProxyEnv,omitempty"`
	ShutdownURL              string            `json:"shutdownUrl,omitempty"`
	ShutdownTokenFile        string            `json:"shutdownTokenFile,omitempty"`
	ReloadURL                string            `json:"reloadUrl,omitempty"`
	ReloadTokenFile          string            `json:"reloadTokenFile,omitempty"`
	Port                     int               `json:"port,omitempty"`
	IdleSleepSec             int               `json:"idleSleepSec,omitempty"`
	AbandonedSessionSleepSec int               `json:"abandonedSessionSleepSec,omitempty"`
	StartTimeoutSec          int               `json:"startTimeoutSec,omitempty"`
	MaxConsecutiveFailures   int               `json:"maxConsecutiveFailures,omitempty"`
	RequestTimeoutSec        int               `json:"requestTimeoutSec,omitempty"`
	InitTimeoutSec           int               `json:"initTimeoutSec,omitempty"`
	MaxConcurrent            int               `json:"maxConcurrent,omitempty"`
	MaxLineBytes             int               `json:"maxLineBytes,omitempty"`
	AllowNonLoopback         bool              `json:"allowNonLoopback,omitempty"`
	Description              string            `json:"description,omitempty"`
	Tags                     []string          `json:"tags,omitempty"`
	ClientPath               string            `json:"clientPath,omitempty"`
}

// EffectiveMode returns "stdio-bridge" or "http" (default).
func (s ServiceConfig) EffectiveMode() string {
	if strings.EqualFold(strings.TrimSpace(s.Mode), "stdio-bridge") {
		return "stdio-bridge"
	}
	return "http"
}

// EffectiveStartMode returns "lazy" or "eager". stdio-bridge defaults to "lazy"; http defaults to "eager".
func (s ServiceConfig) EffectiveStartMode() string {
	switch strings.ToLower(strings.TrimSpace(s.StartMode)) {
	case "lazy":
		return "lazy"
	case "eager":
		return "eager"
	default:
		if s.EffectiveMode() == "stdio-bridge" {
			return "lazy"
		}
		return "eager"
	}
}

// EffectiveIdleSleep returns the idle sleep duration (0 disables idle sleep).
// For stdio-bridge, 0 defaults to 600s and -1 explicitly disables idle sleep.
func (s ServiceConfig) EffectiveIdleSleep() time.Duration {
	if s.EffectiveMode() == "stdio-bridge" {
		if s.IdleSleepSec < 0 {
			return 0
		}
		if s.IdleSleepSec == 0 {
			return DefaultBridgeIdleTimeout
		}
		return time.Duration(s.IdleSleepSec) * time.Second
	}
	if s.IdleSleepSec <= 0 {
		return 0
	}
	return time.Duration(s.IdleSleepSec) * time.Second
}

// EffectiveAbandonedSessionSleep returns the abandoned-session idle sleep duration (0 disables).
func (s ServiceConfig) EffectiveAbandonedSessionSleep() time.Duration {
	if s.AbandonedSessionSleepSec <= 0 {
		return 0
	}
	return time.Duration(s.AbandonedSessionSleepSec) * time.Second
}

// EffectiveStartTimeout returns the startup health-readiness wait timeout (default 15s).
func (s ServiceConfig) EffectiveStartTimeout() time.Duration {
	if s.StartTimeoutSec <= 0 {
		return defaultStartTimeoutSec * time.Second
	}
	return time.Duration(s.StartTimeoutSec) * time.Second
}

// EffectiveMaxConsecutiveFailures returns the circuit-breaker crash threshold (default 5).
func (s ServiceConfig) EffectiveMaxConsecutiveFailures() int {
	if s.MaxConsecutiveFailures <= 0 {
		return defaultMaxConsecutiveFailures
	}
	return s.MaxConsecutiveFailures
}

// EffectiveClientPath returns the MCP HTTP path suffix for this service (default "/mcp").
func (s ServiceConfig) EffectiveClientPath() string {
	if strings.TrimSpace(s.ClientPath) != "" {
		return strings.TrimSpace(s.ClientPath)
	}
	return defaultClientPath
}

// ProxyConfig defines one built-in authenticated localhost reverse proxy.
type ProxyConfig struct {
	Name              string             `json:"name"`
	ListenAddr        string             `json:"listenAddr"`
	TargetURL         string             `json:"targetUrl"`
	TokenFile         string             `json:"tokenFile,omitempty"`
	Tokens            []RouteTokenConfig `json:"tokens,omitempty"`
	UpstreamTokenFile string             `json:"upstreamTokenFile,omitempty"`
	UpstreamService   string             `json:"upstreamService,omitempty"`
	AllowedHosts      []string           `json:"allowedHosts,omitempty"`
	HealthPath        string             `json:"healthPath,omitempty"`
	AllowNonLoopback  bool               `json:"allowNonLoopback,omitempty"`
	ClientPath        string             `json:"clientPath,omitempty"`
}

// EffectiveClientPath returns the MCP HTTP path suffix for this proxy (default "/mcp").
func (p ProxyConfig) EffectiveClientPath() string {
	if strings.TrimSpace(p.ClientPath) != "" {
		return strings.TrimSpace(p.ClientPath)
	}
	return defaultClientPath
}

// ShouldStripProxyEnv returns true unless explicitly set to false in config.
func (s ServiceConfig) ShouldStripProxyEnv() bool {
	if s.StripProxyEnv == nil {
		return true
	}
	return *s.StripProxyEnv
}

// LoadConfig reads, parses, applies defaults to, and validates a JSON config file.
func LoadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %q: %w", path, err)
	}
	return ParseConfig(raw)
}

// ParseConfig parses JSON config bytes, strips UTF-8 BOM if present, applies defaults, and validates.
func ParseConfig(raw []byte) (*Config, error) {
	raw = bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF})
	var cfg Config
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse config JSON: %w", err)
	}
	ApplyConfigDefaults(&cfg)
	if err := ValidateConfig(&cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// ApplyConfigDefaults populates omitted top-level defaults and infers linked upstream services/URLs.
func ApplyConfigDefaults(cfg *Config) {
	if cfg == nil {
		return
	}
	if strings.TrimSpace(cfg.ControlAddr) == "" {
		cfg.ControlAddr = DefaultControlAddr
	}
	if strings.TrimSpace(cfg.LogDir) == "" {
		cfg.LogDir = DefaultLogDir
	}
	if cfg.LogMaxBytes <= 0 {
		cfg.LogMaxBytes = DefaultLogMaxBytes
	}
	if cfg.Gateway != nil && len(cfg.Gateway.Routes) > 0 && strings.TrimSpace(cfg.Gateway.ListenAddr) == "" {
		cfg.Gateway.ListenAddr = DefaultGatewayAddr
	}

	svcByName := make(map[string]ServiceConfig, len(cfg.Services))
	for _, svc := range cfg.Services {
		svcByName[svc.Name] = svc
	}

	for i := range cfg.Proxies {
		if strings.TrimSpace(cfg.Proxies[i].UpstreamService) == "" {
			if inferred := InferServiceForTargetURL(cfg.Services, cfg.Proxies[i].TargetURL); inferred != "" {
				cfg.Proxies[i].UpstreamService = inferred
			}
		}
		if strings.TrimSpace(cfg.Proxies[i].ClientPath) == "" {
			if svc, ok := svcByName[cfg.Proxies[i].UpstreamService]; ok {
				cfg.Proxies[i].ClientPath = svc.ClientPath
			}
		}
	}

	if cfg.Gateway != nil {
		for i := range cfg.Gateway.Routes {
			r := &cfg.Gateway.Routes[i]
			if strings.TrimSpace(r.Service) == "" && strings.TrimSpace(r.TargetURL) != "" {
				if inferred := InferServiceForTargetURL(cfg.Services, r.TargetURL); inferred != "" {
					r.Service = inferred
				}
			} else if strings.TrimSpace(r.Service) != "" && strings.TrimSpace(r.TargetURL) == "" {
				if svc, ok := svcByName[r.Service]; ok && svc.EffectiveMode() == "http" {
					if inferred := InferServiceBaseURL(svc); inferred != "" {
						r.TargetURL = inferred
					}
				}
			}
			if strings.TrimSpace(r.ClientPath) == "" {
				if svc, ok := svcByName[r.Service]; ok && svc.EffectiveMode() == "http" {
					r.ClientPath = svc.ClientPath
				}
			}
		}
	}
}

// InferServiceForTargetURL matches a loopback targetURL's port against managed HTTP services.
func InferServiceForTargetURL(services []ServiceConfig, targetURL string) string {
	u, err := url.Parse(strings.TrimSpace(targetURL))
	if err != nil || u.Host == "" {
		return ""
	}
	if !IsLoopbackHost(u.Hostname()) {
		return ""
	}
	port := urlPort(u)
	if port <= 0 {
		return ""
	}
	matched := ""
	for _, svc := range services {
		if svc.EffectiveMode() != "http" {
			continue
		}
		if InferServicePort(svc) == port {
			if matched != "" {
				// Ambiguous port match.
				return ""
			}
			matched = svc.Name
		}
	}
	return matched
}

// InferServiceBaseURL infers an HTTP base URL (e.g. "http://127.0.0.1:18486") from a ServiceConfig.
func InferServiceBaseURL(svc ServiceConfig) string {
	if svc.EffectiveMode() == "stdio-bridge" {
		return ""
	}
	for _, rawURL := range []string{svc.HealthURL, svc.ShutdownURL, svc.ReloadURL} {
		rawURL = strings.TrimSpace(rawURL)
		if rawURL == "" {
			continue
		}
		u, err := url.Parse(rawURL)
		if err != nil || u.Host == "" {
			continue
		}
		if u.Scheme == "http" || u.Scheme == "https" {
			return u.Scheme + "://" + u.Host
		}
		if u.Scheme == "tcp" {
			return "http://" + u.Host
		}
	}
	if svc.Port > 0 {
		return fmt.Sprintf("http://127.0.0.1:%d", svc.Port)
	}
	return ""
}

func urlPort(u *url.URL) int {
	if pStr := u.Port(); pStr != "" {
		if p, err := strconv.Atoi(pStr); err == nil && p > 0 {
			return p
		}
	}
	switch u.Scheme {
	case "http":
		return 80
	case "https":
		return 443
	default:
		return 0
	}
}

func inferServiceBindKey(svc ServiceConfig) string {
	if svc.EffectiveMode() == "stdio-bridge" {
		return ""
	}
	if svc.Port > 0 {
		return normalizeBindKey(fmt.Sprintf("127.0.0.1:%d", svc.Port))
	}
	for _, rawURL := range []string{svc.HealthURL, svc.ShutdownURL, svc.ReloadURL} {
		rawURL = strings.TrimSpace(rawURL)
		if rawURL == "" {
			continue
		}
		u, err := url.Parse(rawURL)
		if err != nil || u.Host == "" {
			continue
		}
		host := u.Hostname()
		if !IsLoopbackHost(host) {
			return ""
		}
		port := urlPort(u)
		if port > 0 {
			return normalizeBindKey(net.JoinHostPort(host, strconv.Itoa(port)))
		}
	}
	return ""
}

// ValidateConfig verifies addresses, names, URLs, auth requirements, and duplicate bindings.
func ValidateConfig(cfg *Config) error {
	if cfg == nil {
		return errors.New("config is nil")
	}
	if err := validateLoopbackAddr("controlAddr", cfg.ControlAddr); err != nil {
		return err
	}
	if strings.TrimSpace(cfg.ControlTokenFile) == "" && !cfg.AllowInsecureNoControlAuth {
		return errors.New("controlTokenFile is required (or explicitly set allowInsecureNoControlAuth: true)")
	}
	if strings.TrimSpace(cfg.LogDir) == "" {
		return errors.New("logDir must not be empty")
	}

	usedAddrs := map[string]string{}
	if norm := normalizeBindKey(cfg.ControlAddr); norm != "" {
		usedAddrs[norm] = "controlAddr"
	}

	svcByName := make(map[string]ServiceConfig, len(cfg.Services))
	for i, svc := range cfg.Services {
		if !validNameRe.MatchString(svc.Name) {
			return fmt.Errorf("services[%d].name %q is invalid (use 1-64 chars of [a-zA-Z0-9._-], starting with alphanumeric)", i, svc.Name)
		}
		if _, dup := svcByName[svc.Name]; dup {
			return fmt.Errorf("duplicate service name %q", svc.Name)
		}
		svcByName[svc.Name] = svc

		mode := strings.ToLower(strings.TrimSpace(svc.Mode))
		if mode != "" && mode != "http" && mode != "stdio-bridge" {
			return fmt.Errorf("service %q: mode %q must be \"http\" or \"stdio-bridge\"", svc.Name, svc.Mode)
		}
		startMode := strings.ToLower(strings.TrimSpace(svc.StartMode))
		if startMode != "" && startMode != "eager" && startMode != "lazy" {
			return fmt.Errorf("service %q: startMode %q must be \"eager\" or \"lazy\"", svc.Name, svc.StartMode)
		}
		if strings.TrimSpace(svc.Command) == "" {
			return fmt.Errorf("service %q: command must not be empty", svc.Name)
		}
		if svc.HealthIntervalSec < 0 {
			return fmt.Errorf("service %q: healthIntervalSec must be >= 0", svc.Name)
		}
		if svc.Port < 0 || svc.Port > 65535 {
			return fmt.Errorf("service %q: port %d out of range", svc.Name, svc.Port)
		}
		if svc.EffectiveMode() == "stdio-bridge" {
			if svc.IdleSleepSec < -1 {
				return fmt.Errorf("service %q: idleSleepSec must be >= -1", svc.Name)
			}
		} else {
			if svc.IdleSleepSec < 0 {
				return fmt.Errorf("service %q: idleSleepSec must be >= 0", svc.Name)
			}
		}
		if svc.AbandonedSessionSleepSec < 0 {
			return fmt.Errorf("service %q: abandonedSessionSleepSec must be >= 0", svc.Name)
		}
		if svc.StartTimeoutSec < 0 {
			return fmt.Errorf("service %q: startTimeoutSec must be >= 0", svc.Name)
		}
		if svc.MaxConsecutiveFailures < 0 {
			return fmt.Errorf("service %q: maxConsecutiveFailures must be >= 0", svc.Name)
		}
		if svc.RequestTimeoutSec < 0 {
			return fmt.Errorf("service %q: requestTimeoutSec must be >= 0", svc.Name)
		}
		if svc.InitTimeoutSec < 0 {
			return fmt.Errorf("service %q: initTimeoutSec must be >= 0", svc.Name)
		}
		if svc.MaxConcurrent < 0 {
			return fmt.Errorf("service %q: maxConcurrent must be >= 0", svc.Name)
		}
		if svc.MaxLineBytes < 0 {
			return fmt.Errorf("service %q: maxLineBytes must be >= 0", svc.Name)
		}
		if svc.ClientPath != "" && !strings.HasPrefix(strings.TrimSpace(svc.ClientPath), "/") {
			return fmt.Errorf("service %q: clientPath %q must start with '/'", svc.Name, svc.ClientPath)
		}
		if svc.HealthURL != "" {
			if err := validateHealthURL(svc.HealthURL, svc.AllowNonLoopback); err != nil {
				return fmt.Errorf("service %q: invalid healthUrl: %w", svc.Name, err)
			}
		}
		if svc.ShutdownURL != "" {
			if err := validateHTTPURL(svc.ShutdownURL, svc.AllowNonLoopback); err != nil {
				return fmt.Errorf("service %q: invalid shutdownUrl: %w", svc.Name, err)
			}
		}
		if svc.ShutdownTokenFile != "" && strings.TrimSpace(svc.ShutdownTokenFile) == "" {
			return fmt.Errorf("service %q: shutdownTokenFile must not be whitespace", svc.Name)
		}
		if svc.ReloadURL != "" {
			if err := validateHTTPURL(svc.ReloadURL, svc.AllowNonLoopback); err != nil {
				return fmt.Errorf("service %q: invalid reloadUrl: %w", svc.Name, err)
			}
		}
		if svc.ReloadTokenFile != "" && strings.TrimSpace(svc.ReloadTokenFile) == "" {
			return fmt.Errorf("service %q: reloadTokenFile must not be whitespace", svc.Name)
		}
		for k, p := range svc.EnvFiles {
			if strings.TrimSpace(k) == "" {
				return fmt.Errorf("service %q: envFiles contains an empty env variable name", svc.Name)
			}
			if strings.TrimSpace(p) == "" {
				return fmt.Errorf("service %q: envFiles[%q] path must not be empty", svc.Name, k)
			}
		}
		if norm := inferServiceBindKey(svc); norm != "" {
			if prev, exists := usedAddrs[norm]; exists {
				return fmt.Errorf("service %q: inferred port %d conflicts with %s", svc.Name, InferServicePort(svc), prev)
			}
			usedAddrs[norm] = fmt.Sprintf("service %q", svc.Name)
		}
	}

	seenProxies := map[string]struct{}{}
	for i, p := range cfg.Proxies {
		if !validNameRe.MatchString(p.Name) {
			return fmt.Errorf("proxies[%d].name %q is invalid (use 1-64 chars of [a-zA-Z0-9._-], starting with alphanumeric)", i, p.Name)
		}
		if _, dup := seenProxies[p.Name]; dup {
			return fmt.Errorf("duplicate proxy name %q", p.Name)
		}
		seenProxies[p.Name] = struct{}{}

		if err := validateLoopbackAddr(fmt.Sprintf("proxies[%q].listenAddr", p.Name), p.ListenAddr); err != nil {
			return err
		}
		if norm := normalizeBindKey(p.ListenAddr); norm != "" {
			if prev, exists := usedAddrs[norm]; exists {
				return fmt.Errorf("proxy %q: listenAddr %q conflicts with %s", p.Name, p.ListenAddr, prev)
			}
			usedAddrs[norm] = fmt.Sprintf("proxy %q", p.Name)
		}
		if err := validateHTTPURL(p.TargetURL, p.AllowNonLoopback); err != nil {
			return fmt.Errorf("proxy %q: invalid targetUrl: %w", p.Name, err)
		}
		if strings.TrimSpace(p.UpstreamService) != "" {
			if _, ok := svcByName[p.UpstreamService]; !ok {
				return fmt.Errorf("proxy %q: upstreamService %q does not match any configured service", p.Name, p.UpstreamService)
			}
		}
		if err := validateRouteAuthConfig(fmt.Sprintf("proxy %q", p.Name), p.TokenFile, p.Tokens); err != nil {
			return err
		}
		if p.UpstreamTokenFile != "" && strings.TrimSpace(p.UpstreamTokenFile) == "" {
			return fmt.Errorf("proxy %q: upstreamTokenFile must not be empty", p.Name)
		}
		if p.HealthPath != "" && !strings.HasPrefix(p.HealthPath, "/") {
			return fmt.Errorf("proxy %q: healthPath %q must start with '/'", p.Name, p.HealthPath)
		}
		if p.ClientPath != "" && !strings.HasPrefix(strings.TrimSpace(p.ClientPath), "/") {
			return fmt.Errorf("proxy %q: clientPath %q must start with '/'", p.Name, p.ClientPath)
		}
	}

	if cfg.Gateway != nil {
		gwAddr := strings.TrimSpace(cfg.Gateway.ListenAddr)
		if gwAddr == "" && len(cfg.Gateway.Routes) > 0 {
			gwAddr = DefaultGatewayAddr
		}
		if gwAddr != "" {
			if err := validateLoopbackAddr("gateway.listenAddr", gwAddr); err != nil {
				return err
			}
			if norm := normalizeBindKey(gwAddr); norm != "" {
				if prev, exists := usedAddrs[norm]; exists {
					return fmt.Errorf("gateway: listenAddr %q conflicts with %s", gwAddr, prev)
				}
				usedAddrs[norm] = "gateway"
			}
		}
		seenSlugs := map[string]struct{}{}
		for i, r := range cfg.Gateway.Routes {
			if !validNameRe.MatchString(r.Slug) {
				return fmt.Errorf("gateway.routes[%d].slug %q is invalid (use 1-64 chars of [a-zA-Z0-9._-], starting with alphanumeric)", i, r.Slug)
			}
			if _, dup := seenSlugs[r.Slug]; dup {
				return fmt.Errorf("gateway: duplicate route slug %q", r.Slug)
			}
			seenSlugs[r.Slug] = struct{}{}

			var linkedSvc ServiceConfig
			hasLinkedSvc := false
			if strings.TrimSpace(r.Service) != "" {
				svc, ok := svcByName[r.Service]
				if !ok {
					return fmt.Errorf("gateway route %q: service %q does not match any configured service", r.Slug, r.Service)
				}
				linkedSvc = svc
				hasLinkedSvc = true
			}

			if hasLinkedSvc && linkedSvc.EffectiveMode() == "stdio-bridge" {
				if strings.TrimSpace(r.TargetURL) != "" {
					return fmt.Errorf("gateway route %q: targetUrl must be omitted when routing to stdio-bridge service %q", r.Slug, r.Service)
				}
			} else {
				effectiveTarget := strings.TrimSpace(r.TargetURL)
				inferredTarget := ""
				if hasLinkedSvc {
					inferredTarget = InferServiceBaseURL(linkedSvc)
				}
				if effectiveTarget == "" {
					effectiveTarget = inferredTarget
				}
				if effectiveTarget == "" {
					return fmt.Errorf("gateway route %q: targetUrl is required", r.Slug)
				}
				inheritedNonLoop := hasLinkedSvc && linkedSvc.AllowNonLoopback && inferredTarget != "" &&
					strings.EqualFold(strings.TrimRight(effectiveTarget, "/"), strings.TrimRight(inferredTarget, "/"))
				allowNonLoop := r.AllowNonLoopback || inheritedNonLoop
				if err := validateHTTPURL(effectiveTarget, allowNonLoop); err != nil {
					return fmt.Errorf("gateway route %q: invalid targetUrl: %w", r.Slug, err)
				}
			}

			if err := validateRouteAuthConfig(fmt.Sprintf("gateway route %q", r.Slug), r.TokenFile, r.Tokens); err != nil {
				return err
			}
			if r.UpstreamTokenFile != "" && strings.TrimSpace(r.UpstreamTokenFile) == "" {
				return fmt.Errorf("gateway route %q: upstreamTokenFile must not be empty", r.Slug)
			}
			if r.HealthPath != "" && !strings.HasPrefix(r.HealthPath, "/") {
				return fmt.Errorf("gateway route %q: healthPath %q must start with '/'", r.Slug, r.HealthPath)
			}
			if r.ClientPath != "" && !strings.HasPrefix(strings.TrimSpace(r.ClientPath), "/") {
				return fmt.Errorf("gateway route %q: clientPath %q must start with '/'", r.Slug, r.ClientPath)
			}
		}
	}

	return nil
}

func validateRouteAuthConfig(label, tokenFile string, tokens []RouteTokenConfig) error {
	if tokenFile != "" && strings.TrimSpace(tokenFile) == "" {
		return fmt.Errorf("%s: tokenFile must not be whitespace", label)
	}
	if strings.TrimSpace(tokenFile) == "" && len(tokens) == 0 {
		return fmt.Errorf("%s: tokenFile or tokens is required", label)
	}
	for i, tc := range tokens {
		if strings.TrimSpace(tc.TokenFile) == "" {
			return fmt.Errorf("%s: tokens[%d].tokenFile is required", label, i)
		}
		if tc.Name != "" && !validNameRe.MatchString(tc.Name) {
			return fmt.Errorf("%s: tokens[%d].name %q is invalid", label, i, tc.Name)
		}
	}
	return nil
}

// ValidateSecretFiles verifies that every secret file referenced by cfg exists
// on disk and can be decoded via ReadSecretFile (including optional .next grace files if present).
func ValidateSecretFiles(cfg *Config) error {
	if cfg == nil {
		return errors.New("config is nil")
	}
	if strings.TrimSpace(cfg.ControlTokenFile) != "" {
		if err := validateTokenFileWithOptionalNext("controlTokenFile", cfg.ControlTokenFile); err != nil {
			return err
		}
	}
	for _, svc := range cfg.Services {
		for k, path := range svc.EnvFiles {
			if _, err := ReadSecretFile(path); err != nil {
				return fmt.Errorf("service %q envFiles[%q]: %w", svc.Name, k, err)
			}
		}
		if strings.TrimSpace(svc.ShutdownTokenFile) != "" {
			if _, err := ReadSecretFile(svc.ShutdownTokenFile); err != nil {
				return fmt.Errorf("service %q shutdownTokenFile: %w", svc.Name, err)
			}
		}
		if strings.TrimSpace(svc.ReloadTokenFile) != "" {
			if _, err := ReadSecretFile(svc.ReloadTokenFile); err != nil {
				return fmt.Errorf("service %q reloadTokenFile: %w", svc.Name, err)
			}
		}
	}
	for _, p := range cfg.Proxies {
		if strings.TrimSpace(p.TokenFile) != "" {
			if err := validateTokenFileWithOptionalNext(fmt.Sprintf("proxy %q tokenFile", p.Name), p.TokenFile); err != nil {
				return err
			}
		}
		for i, tc := range p.Tokens {
			if err := validateTokenFileWithOptionalNext(fmt.Sprintf("proxy %q tokens[%d].tokenFile", p.Name, i), tc.TokenFile); err != nil {
				return err
			}
		}
		if strings.TrimSpace(p.UpstreamTokenFile) != "" {
			if _, err := ReadSecretFile(p.UpstreamTokenFile); err != nil {
				return fmt.Errorf("proxy %q upstreamTokenFile: %w", p.Name, err)
			}
		}
	}
	if cfg.Gateway != nil {
		for _, r := range cfg.Gateway.Routes {
			if strings.TrimSpace(r.TokenFile) != "" {
				if err := validateTokenFileWithOptionalNext(fmt.Sprintf("gateway route %q tokenFile", r.Slug), r.TokenFile); err != nil {
					return err
				}
			}
			for i, tc := range r.Tokens {
				if err := validateTokenFileWithOptionalNext(fmt.Sprintf("gateway route %q tokens[%d].tokenFile", r.Slug, i), tc.TokenFile); err != nil {
					return err
				}
			}
			if strings.TrimSpace(r.UpstreamTokenFile) != "" {
				if _, err := ReadSecretFile(r.UpstreamTokenFile); err != nil {
					return fmt.Errorf("gateway route %q upstreamTokenFile: %w", r.Slug, err)
				}
			}
		}
	}
	return nil
}

func validateTokenFileWithOptionalNext(label, path string) error {
	if _, err := ReadSecretFile(path); err != nil {
		return fmt.Errorf("%s: %w", label, err)
	}
	nextPath := filepath.Clean(strings.TrimSpace(path)) + ".next"
	if _, err := os.Stat(nextPath); err == nil {
		if _, err := ReadSecretFile(nextPath); err != nil {
			return fmt.Errorf("%s (.next): %w", label, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%s (.next): %w", label, err)
	}
	return nil
}

// CollectAllSecrets reads all available secret values referenced by cfg (including optional .next
// grace files and envFiles) for feeding Redactor.SetSecrets. Missing or unreadable files are skipped.
func CollectAllSecrets(cfg *Config) []string {
	if cfg == nil {
		return nil
	}
	seen := make(map[string]struct{})
	var out []string
	addPath := func(path string, checkNext bool) {
		p := strings.TrimSpace(path)
		if p == "" {
			return
		}
		if s, err := ReadSecretFile(p); err == nil && s != "" {
			if _, dup := seen[s]; !dup {
				seen[s] = struct{}{}
				out = append(out, s)
			}
		}
		if checkNext {
			nextPath := filepath.Clean(p) + ".next"
			if s, err := ReadSecretFile(nextPath); err == nil && s != "" {
				if _, dup := seen[s]; !dup {
					seen[s] = struct{}{}
					out = append(out, s)
				}
			}
		}
	}

	addPath(cfg.ControlTokenFile, true)
	for _, svc := range cfg.Services {
		for _, p := range svc.EnvFiles {
			addPath(p, false)
		}
		addPath(svc.ShutdownTokenFile, false)
		addPath(svc.ReloadTokenFile, false)
	}
	for _, p := range cfg.Proxies {
		addPath(p.TokenFile, true)
		for _, tc := range p.Tokens {
			addPath(tc.TokenFile, true)
		}
		addPath(p.UpstreamTokenFile, false)
	}
	if cfg.Gateway != nil {
		for _, r := range cfg.Gateway.Routes {
			addPath(r.TokenFile, true)
			for _, tc := range r.Tokens {
				addPath(tc.TokenFile, true)
			}
			addPath(r.UpstreamTokenFile, false)
		}
	}
	return out
}

func validateLoopbackAddr(field, addr string) error {
	host, portStr, err := net.SplitHostPort(strings.TrimSpace(addr))
	if err != nil {
		return fmt.Errorf("%s %q must be host:port: %w", field, addr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 0 || port > 65535 {
		return fmt.Errorf("%s %q has invalid port %q", field, addr, portStr)
	}
	if !IsLoopbackHost(host) {
		return fmt.Errorf("%s %q must bind a loopback address (127.0.0.1, localhost, or ::1)", field, addr)
	}
	return nil
}

// normalizeBindKey canonicalizes localhost and IPv4 loopback (127.0.0.1) to "loopback:<port>",
// IPv6 loopback (::1) to "[::1]:<port>", and returns "" for ephemeral port 0.
func normalizeBindKey(addr string) string {
	host, portStr, err := net.SplitHostPort(strings.TrimSpace(addr))
	if err != nil || portStr == "0" || portStr == "" {
		return ""
	}
	h := strings.ToLower(strings.Trim(strings.TrimSpace(host), "[]"))
	if h == "localhost" {
		return "loopback:" + portStr
	}
	if ip := net.ParseIP(h); ip != nil {
		if ip4 := ip.To4(); ip4 != nil {
			if ip4.IsLoopback() {
				return "loopback:" + portStr
			}
			return ip4.String() + ":" + portStr
		}
		if ip.IsLoopback() {
			return "[::1]:" + portStr
		}
		return "[" + ip.String() + "]:" + portStr
	}
	return h + ":" + portStr
}

func validateHTTPURL(raw string, allowNonLoopback bool) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("scheme must be http or https (got %q)", u.Scheme)
	}
	if u.Host == "" {
		return errors.New("host must not be empty")
	}
	if !allowNonLoopback && !IsLoopbackHost(u.Hostname()) {
		return fmt.Errorf("host %q must be a loopback address (set allowNonLoopback: true to override)", u.Hostname())
	}
	return nil
}

func validateHealthURL(raw string, allowNonLoopback bool) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return err
	}
	switch u.Scheme {
	case "http", "https":
		if u.Host == "" {
			return errors.New("host must not be empty")
		}
		if !allowNonLoopback && !IsLoopbackHost(u.Hostname()) {
			return fmt.Errorf("host %q must be a loopback address (set allowNonLoopback: true to override)", u.Hostname())
		}
		return nil
	case "tcp":
		host, portStr, err := net.SplitHostPort(u.Host)
		if err != nil || host == "" {
			return fmt.Errorf("tcp healthUrl must specify host:port (got %q)", u.Host)
		}
		port, err := strconv.Atoi(portStr)
		if err != nil || port < 1 || port > 65535 {
			return fmt.Errorf("tcp healthUrl has invalid port %q", portStr)
		}
		if !allowNonLoopback && !IsLoopbackHost(host) {
			return fmt.Errorf("host %q must be a loopback address (set allowNonLoopback: true to override)", host)
		}
		return nil
	default:
		return fmt.Errorf("scheme must be http, https, or tcp (got %q)", u.Scheme)
	}
}

// IsLoopbackHost reports whether host is localhost or a loopback IP literal.
func IsLoopbackHost(host string) bool {
	h := strings.TrimSpace(strings.Trim(host, "[]"))
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// DefaultAllowedHosts returns loopback Host header values for a listen address.
func DefaultAllowedHosts(listenAddr string) []string {
	seen := map[string]struct{}{}
	var out []string
	add := func(h string) {
		h = strings.ToLower(strings.TrimSpace(h))
		if h == "" {
			return
		}
		if _, ok := seen[h]; !ok {
			seen[h] = struct{}{}
			out = append(out, h)
		}
	}
	add(listenAddr)
	if _, port, err := net.SplitHostPort(strings.TrimSpace(listenAddr)); err == nil && port != "" && port != "0" {
		add("127.0.0.1:" + port)
		add("localhost:" + port)
		add("[::1]:" + port)
	}
	return out
}

// ReadSecretFile reads a secret token from disk (capped at 8 KiB), stripping UTF-8 BOM and
// surrounding whitespace. UTF-16 BOMs, NUL bytes, control chars, and internal whitespace are rejected.
func ReadSecretFile(path string) (string, error) {
	cleaned := filepath.Clean(strings.TrimSpace(path))
	if cleaned == "" || cleaned == "." {
		return "", errors.New("secret file path is empty")
	}
	f, err := os.Open(cleaned)
	if err != nil {
		return "", fmt.Errorf("read secret file %q: %w", cleaned, err)
	}
	defer f.Close()

	raw, err := io.ReadAll(io.LimitReader(f, maxSecretFileBytes+1))
	if err != nil {
		return "", fmt.Errorf("read secret file %q: %w", cleaned, err)
	}
	if len(raw) > maxSecretFileBytes {
		return "", fmt.Errorf("secret file %q exceeds maximum allowed size (%d bytes)", cleaned, maxSecretFileBytes)
	}
	if bytes.HasPrefix(raw, []byte{0xFF, 0xFE}) || bytes.HasPrefix(raw, []byte{0xFE, 0xFF}) {
		return "", fmt.Errorf("secret file %q appears to be UTF-16 encoded (use UTF-8)", cleaned)
	}
	if bytes.IndexByte(raw, 0x00) >= 0 {
		return "", fmt.Errorf("secret file %q contains NUL byte (use plain UTF-8)", cleaned)
	}
	raw = bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF})
	val := strings.TrimSpace(string(raw))
	if val == "" {
		return "", fmt.Errorf("secret file %q is empty", cleaned)
	}
	if strings.ContainsAny(val, "\r\n") {
		return "", fmt.Errorf("secret file %q must contain a single-line secret", cleaned)
	}
	for i := 0; i < len(val); i++ {
		b := val[i]
		if b < 0x21 || b > 0x7E {
			return "", fmt.Errorf("secret file %q contains invalid control/whitespace/non-ASCII byte 0x%02X", cleaned, b)
		}
	}
	return val, nil
}

// InferServicePort determines the reported port for a service from Port, HealthURL, ShutdownURL, or ReloadURL.
func InferServicePort(svc ServiceConfig) int {
	if svc.Port > 0 {
		return svc.Port
	}
	for _, rawURL := range []string{svc.HealthURL, svc.ShutdownURL, svc.ReloadURL} {
		if rawURL == "" {
			continue
		}
		u, err := url.Parse(rawURL)
		if err != nil {
			continue
		}
		if p := urlPort(u); p > 0 {
			return p
		}
	}
	return 0
}

// BuildServiceEnv constructs the child process environment:
//  1. Starts from baseEnv.
//  2. If svc.ShouldStripProxyEnv(), removes HTTP_PROXY/HTTPS_PROXY/ALL_PROXY/NO_PROXY (case-insensitive)
//     and injects no_proxy & NO_PROXY = "localhost,127.0.0.1,::1".
//  3. Applies svc.Env overrides.
//  4. Reads svc.EnvFiles from disk and injects secrets without ever exposing them in config.json.
func BuildServiceEnv(baseEnv []string, svc ServiceConfig) ([]string, error) {
	env, _, err := buildServiceEnvWithSecrets(baseEnv, svc)
	return env, err
}

func buildServiceEnvWithSecrets(baseEnv []string, svc ServiceConfig) ([]string, []string, error) {
	stripProxy := svc.ShouldStripProxyEnv()

	isProxyKey := func(k string) bool {
		switch strings.ToUpper(k) {
		case "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY":
			return true
		default:
			return false
		}
	}

	// Track explicit overrides (case-insensitive) so we replace inherited entries cleanly.
	overrides := make(map[string]struct{}, len(svc.Env)+len(svc.EnvFiles))
	for k := range svc.Env {
		overrides[strings.ToUpper(k)] = struct{}{}
	}
	for k := range svc.EnvFiles {
		overrides[strings.ToUpper(k)] = struct{}{}
	}

	out := make([]string, 0, len(baseEnv)+len(svc.Env)+len(svc.EnvFiles)+2)
	for _, entry := range baseEnv {
		k, _, ok := strings.Cut(entry, "=")
		if !ok || k == "" {
			continue
		}
		if stripProxy && isProxyKey(k) {
			continue
		}
		if _, overridden := overrides[strings.ToUpper(k)]; overridden {
			continue
		}
		out = append(out, entry)
	}

	if stripProxy {
		if _, overridden := overrides["NO_PROXY"]; !overridden {
			out = append(out,
				"no_proxy=localhost,127.0.0.1,::1",
				"NO_PROXY=localhost,127.0.0.1,::1",
			)
		}
	}

	if len(svc.Env) > 0 {
		keys := make([]string, 0, len(svc.Env))
		for k := range svc.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if _, fromFile := svc.EnvFiles[k]; fromFile {
				continue
			}
			conflict := false
			for fk := range svc.EnvFiles {
				if strings.EqualFold(fk, k) {
					conflict = true
					break
				}
			}
			if conflict {
				continue
			}
			out = append(out, k+"="+svc.Env[k])
		}
	}

	var secrets []string
	if len(svc.EnvFiles) > 0 {
		keys := make([]string, 0, len(svc.EnvFiles))
		for k := range svc.EnvFiles {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		secrets = make([]string, 0, len(keys))
		for _, k := range keys {
			secret, err := ReadSecretFile(svc.EnvFiles[k])
			if err != nil {
				return nil, nil, fmt.Errorf("service %q envFiles[%q]: %w", svc.Name, k, err)
			}
			out = append(out, k+"="+secret)
			secrets = append(secrets, secret)
		}
	}

	return out, secrets, nil
}
