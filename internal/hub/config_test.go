package hub

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestBuildServiceEnv_StripProxyAndEnvFiles(t *testing.T) {
	dir := t.TempDir()
	secretPath := filepath.Join(dir, "api.token")
	// Include UTF-8 BOM and surrounding CRLF/spaces to test resilient loading.
	if err := os.WriteFile(secretPath, []byte("\xef\xbb\xbf  s3cr3t-token-value \r\n"), 0o600); err != nil {
		t.Fatalf("write secret file: %v", err)
	}

	baseEnv := []string{
		"PATH=C:\\Windows\\System32",
		"HTTP_PROXY=http://127.0.0.1:9999",
		"https_proxy=http://127.0.0.1:9999",
		"All_Proxy=socks5://127.0.0.1:9999",
		"NO_PROXY=example.com",
		"KEEP_ME=hello",
		"OVERRIDE_ME=old",
	}

	svc := ServiceConfig{
		Name:    "demo",
		Command: "node.exe",
		Env: map[string]string{
			"OVERRIDE_ME": "new",
			"MCP_TOKEN":   "should-be-overridden-by-envFiles",
		},
		EnvFiles: map[string]string{
			"MCP_TOKEN": secretPath,
		},
	}

	if !svc.ShouldStripProxyEnv() {
		t.Fatalf("expected default StripProxyEnv to be true")
	}

	got, err := BuildServiceEnv(baseEnv, svc)
	if err != nil {
		t.Fatalf("BuildServiceEnv error: %v", err)
	}

	for _, entry := range got {
		k, _, _ := strings.Cut(entry, "=")
		switch strings.ToUpper(k) {
		case "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY":
			t.Errorf("expected proxy env var %q to be stripped", entry)
		}
	}

	mustHave := []string{
		"PATH=C:\\Windows\\System32",
		"KEEP_ME=hello",
		"OVERRIDE_ME=new",
		"MCP_TOKEN=s3cr3t-token-value",
		"NO_PROXY=localhost,127.0.0.1,::1",
		"no_proxy=localhost,127.0.0.1,::1",
	}
	for _, want := range mustHave {
		if !slices.Contains(got, want) {
			t.Errorf("expected env to contain %q; got %v", want, got)
		}
	}
	if slices.Contains(got, "NO_PROXY=example.com") {
		t.Errorf("expected inherited NO_PROXY to be replaced")
	}
	if slices.Contains(got, "MCP_TOKEN=should-be-overridden-by-envFiles") {
		t.Errorf("expected envFiles to take precedence over env")
	}
}

func TestBuildServiceEnv_StripProxyFalse(t *testing.T) {
	strip := false
	baseEnv := []string{
		"HTTP_PROXY=http://127.0.0.1:9999",
		"https_proxy=http://127.0.0.1:9999",
		"ALL_PROXY=socks5://127.0.0.1:9999",
	}
	svc := ServiceConfig{
		Name:          "keep-proxy",
		Command:       "node.exe",
		StripProxyEnv: &strip,
	}
	got, err := BuildServiceEnv(baseEnv, svc)
	if err != nil {
		t.Fatalf("BuildServiceEnv error: %v", err)
	}
	for _, want := range baseEnv {
		if !slices.Contains(got, want) {
			t.Errorf("expected proxy var %q preserved when stripProxyEnv=false", want)
		}
	}
}

func TestBuildServiceEnv_EnvFileErrorsAndHardening(t *testing.T) {
	dir := t.TempDir()
	writeFile := func(name string, data []byte) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, data, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	cases := []struct {
		name string
		path string
	}{
		{"missing", filepath.Join(dir, "does-not-exist.token")},
		{"empty", writeFile("empty.token", []byte("   \r\n"))},
		{"multiline", writeFile("multi.token", []byte("line1\nline2"))},
		{"utf16-le-bom", writeFile("utf16le.token", []byte{0xFF, 0xFE, 't', 0x00, 'o', 0x00, 'k', 0x00})},
		{"utf16-be-bom", writeFile("utf16be.token", []byte{0xFE, 0xFF, 0x00, 't', 0x00, 'o', 0x00, 'k'})},
		{"embedded-nul", writeFile("nul.token", []byte("tok\x00en"))},
		{"internal-space", writeFile("space.token", []byte("tok en"))},
		{"internal-tab", writeFile("tab.token", []byte("tok\ten"))},
		{"control-char", writeFile("ctrl.token", []byte("tok\x07en"))},
		{"non-ascii", writeFile("utf8.token", []byte("tok-密码"))},
		{"oversized-gt-8kib", writeFile("big.token", []byte(strings.Repeat("a", maxSecretFileBytes+1)))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := BuildServiceEnv(nil, ServiceConfig{
				Name:     "svc",
				Command:  "cmd.exe",
				EnvFiles: map[string]string{"SECRET": tc.path},
			})
			if err == nil {
				t.Fatalf("expected error for %s secret file", tc.name)
			}
		})
	}
}

func TestParseConfig_DefaultsAndValidation(t *testing.T) {
	cfg, err := ParseConfig([]byte(`{
		"controlTokenFile": "C:/Users/example/AppData/Local/McpHub/secrets/control.token",
		"services": [
			{"name": "svc-1", "command": "node.exe", "healthUrl": "http://127.0.0.1:18486/healthz"},
			{"name": "svc-2", "command": "node.exe", "healthUrl": "tcp://127.0.0.1:18487"},
			{"name": "bridge-1", "mode": "stdio-bridge", "command": "node.exe"}
		],
		"proxies": [
			{"name": "px-1", "listenAddr": "127.0.0.1:18476", "targetUrl": "http://127.0.0.1:18486", "tokenFile": "C:/Users/example/AppData/Local/McpHub/secrets/px.token"}
		],
		"gateway": {
			"routes": [
				{"slug": "svc1", "service": "svc-1", "tokenFile": "C:/Users/example/AppData/Local/McpHub/secrets/gw1.token"},
				{"slug": "br1", "service": "bridge-1", "tokens": [{"name": "ro", "tokenFile": "C:/Users/example/AppData/Local/McpHub/secrets/gw2.token", "readOnlyOnly": true}]}
			]
		}
	}`))
	if err != nil {
		t.Fatalf("valid config failed: %v", err)
	}
	if cfg.ControlAddr != DefaultControlAddr {
		t.Errorf("controlAddr = %q, want %q", cfg.ControlAddr, DefaultControlAddr)
	}
	if cfg.LogDir != DefaultLogDir {
		t.Errorf("logDir = %q, want %q", cfg.LogDir, DefaultLogDir)
	}
	if cfg.LogMaxBytes != DefaultLogMaxBytes {
		t.Errorf("logMaxBytes = %d, want %d", cfg.LogMaxBytes, DefaultLogMaxBytes)
	}
	if !cfg.ShouldShowTray() {
		t.Errorf("ShouldShowTray = false, want default true")
	}
	if cfg.Proxies[0].UpstreamService != "svc-1" {
		t.Errorf("auto-inferred UpstreamService = %q, want svc-1", cfg.Proxies[0].UpstreamService)
	}
	if cfg.Gateway.ListenAddr != DefaultGatewayAddr {
		t.Errorf("gateway.listenAddr = %q, want %q", cfg.Gateway.ListenAddr, DefaultGatewayAddr)
	}
	if cfg.Gateway.Routes[0].TargetURL != "http://127.0.0.1:18486" {
		t.Errorf("auto-inferred gateway targetUrl = %q, want http://127.0.0.1:18486", cfg.Gateway.Routes[0].TargetURL)
	}
	if cfg.Services[2].EffectiveStartMode() != "lazy" {
		t.Errorf("stdio-bridge default startMode = %q, want lazy", cfg.Services[2].EffectiveStartMode())
	}
	if cfg.Services[2].EffectiveIdleSleep() != DefaultBridgeIdleTimeout {
		t.Errorf("stdio-bridge default idleSleep = %s, want %s", cfg.Services[2].EffectiveIdleSleep(), DefaultBridgeIdleTimeout)
	}
	noSleepBridge := ServiceConfig{Name: "b", Mode: "stdio-bridge", Command: "x", IdleSleepSec: -1}
	if noSleepBridge.EffectiveIdleSleep() != 0 {
		t.Errorf("stdio-bridge idleSleepSec=-1 = %s, want 0 (disabled)", noSleepBridge.EffectiveIdleSleep())
	}
	if p := InferServicePort(cfg.Services[0]); p != 18486 {
		t.Errorf("InferServicePort(http) = %d, want 18486", p)
	}
	if p := InferServicePort(cfg.Services[1]); p != 18487 {
		t.Errorf("InferServicePort(tcp) = %d, want 18487", p)
	}

	// Explicit allowInsecureNoControlAuth works when controlTokenFile is omitted.
	if _, err := ParseConfig([]byte(`{"allowInsecureNoControlAuth": true}`)); err != nil {
		t.Fatalf("expected allowInsecureNoControlAuth=true to pass: %v", err)
	}

	badConfigs := []struct {
		name string
		raw  string
	}{
		{"fail-closed missing controlTokenFile", `{}`},
		{"fail-closed whitespace controlTokenFile", `{"controlTokenFile": "   "}`},
		{"non-loopback controlAddr", `{"allowInsecureNoControlAuth":true,"controlAddr": "0.0.0.0:18470"}`},
		{"non-loopback proxy listenAddr", `{"allowInsecureNoControlAuth":true,"proxies": [{"name":"p","listenAddr":"0.0.0.0:18476","targetUrl":"http://127.0.0.1:18486","tokenFile":"a"}]}`},
		{"non-loopback proxy targetUrl without override", `{"allowInsecureNoControlAuth":true,"proxies": [{"name":"p","listenAddr":"127.0.0.1:18476","targetUrl":"http://10.0.0.5:18486","tokenFile":"a"}]}`},
		{"non-loopback service healthUrl without override", `{"allowInsecureNoControlAuth":true,"services": [{"name":"s","command":"x","healthUrl":"http://example.com:8080/healthz"}]}`},
		{"non-loopback service shutdownUrl without override", `{"allowInsecureNoControlAuth":true,"services": [{"name":"s","command":"x","shutdownUrl":"http://example.com:8080/shutdown"}]}`},
		{"non-loopback service reloadUrl without override", `{"allowInsecureNoControlAuth":true,"services": [{"name":"s","command":"x","reloadUrl":"http://example.com:8080/reload"}]}`},
		{"path traversal service name", `{"allowInsecureNoControlAuth":true,"services": [{"name":"../evil","command":"cmd.exe"}]}`},
		{"duplicate service name", `{"allowInsecureNoControlAuth":true,"services": [{"name":"a","command":"x"},{"name":"a","command":"y"}]}`},
		{"missing proxy tokenFile and tokens", `{"allowInsecureNoControlAuth":true,"proxies": [{"name":"p","listenAddr":"127.0.0.1:18476","targetUrl":"http://127.0.0.1:18486"}]}`},
		{"unknown proxy upstreamService", `{"allowInsecureNoControlAuth":true,"proxies": [{"name":"p","listenAddr":"127.0.0.1:18476","targetUrl":"http://127.0.0.1:18486","tokenFile":"t","upstreamService":"nope"}]}`},
		{"port conflict with controlAddr", `{"allowInsecureNoControlAuth":true,"controlAddr":"127.0.0.1:18470","proxies":[{"name":"p","listenAddr":"127.0.0.1:18470","targetUrl":"http://127.0.0.1:18486","tokenFile":"t"}]}`},
		{"localhost vs 127.0.0.1 collision", `{"allowInsecureNoControlAuth":true,"controlAddr":"127.0.0.1:18470","proxies":[{"name":"p","listenAddr":"localhost:18470","targetUrl":"http://127.0.0.1:18486","tokenFile":"t"}]}`},
		{"proxy collides with service port", `{"allowInsecureNoControlAuth":true,"services":[{"name":"s","command":"x","port":18486}],"proxies":[{"name":"p","listenAddr":"127.0.0.1:18486","targetUrl":"http://127.0.0.1:18487","tokenFile":"t"}]}`},
		{"two services collide on port", `{"allowInsecureNoControlAuth":true,"services":[{"name":"s1","command":"x","healthUrl":"http://127.0.0.1:18486/h"},{"name":"s2","command":"x","healthUrl":"http://localhost:18486/h"}]}`},
		{"gateway collides with proxy", `{"allowInsecureNoControlAuth":true,"proxies":[{"name":"p","listenAddr":"127.0.0.1:18471","targetUrl":"http://127.0.0.1:18486","tokenFile":"t"}],"gateway":{"routes":[{"slug":"r","targetUrl":"http://127.0.0.1:18486","tokenFile":"t"}]}}`},
		{"gateway duplicate slug", `{"allowInsecureNoControlAuth":true,"gateway":{"routes":[{"slug":"dup","targetUrl":"http://127.0.0.1:18486","tokenFile":"t"},{"slug":"dup","targetUrl":"http://127.0.0.1:18487","tokenFile":"t"}]}}`},
		{"gateway route missing auth", `{"allowInsecureNoControlAuth":true,"gateway":{"routes":[{"slug":"r","targetUrl":"http://127.0.0.1:18486"}]}}`},
		{"gateway stdio-bridge with targetUrl", `{"allowInsecureNoControlAuth":true,"services":[{"name":"b","mode":"stdio-bridge","command":"x"}],"gateway":{"routes":[{"slug":"r","service":"b","targetUrl":"http://127.0.0.1:18486","tokenFile":"t"}]}}`},
		{"whitespace-only upstreamTokenFile", `{"allowInsecureNoControlAuth":true,"proxies":[{"name":"p","listenAddr":"127.0.0.1:18476","targetUrl":"http://127.0.0.1:18486","tokenFile":"t","upstreamTokenFile":"   "}]}`},
		{"invalid mode", `{"allowInsecureNoControlAuth":true,"services":[{"name":"s","command":"x","mode":"bogus"}]}`},
		{"invalid startMode", `{"allowInsecureNoControlAuth":true,"services":[{"name":"s","command":"x","startMode":"bogus"}]}`},
		{"unknown JSON field", `{"allowInsecureNoControlAuth":true,"bogusField": true}`},
		{"gateway custom non-loopback targetUrl does not inherit service allowNonLoopback", `{"allowInsecureNoControlAuth":true,"services":[{"name":"remote","command":"x","healthUrl":"https://10.0.0.5:8443/healthz","allowNonLoopback":true}],"gateway":{"routes":[{"slug":"r","service":"remote","targetUrl":"https://10.0.0.99:9000","tokenFile":"t"}]}}`},
		{"gateway custom non-loopback targetUrl with portless remote service requires route allowNonLoopback", `{"allowInsecureNoControlAuth":true,"services":[{"name":"remote","command":"x","allowNonLoopback":true}],"gateway":{"routes":[{"slug":"r","service":"remote","targetUrl":"https://10.0.0.5:8443","tokenFile":"t"}]}}`},
	}
	for _, tc := range badConfigs {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseConfig([]byte(tc.raw)); err == nil {
				t.Fatalf("expected validation error for %s", tc.name)
			}
		})
	}

	// Non-loopback allowed when allowNonLoopback: true (including explicit route override and inferred service base URL).
	if _, err := ParseConfig([]byte(`{
		"allowInsecureNoControlAuth": true,
		"services": [{"name":"remote","command":"x","healthUrl":"https://10.0.0.5:8443/healthz","allowNonLoopback":true}],
		"proxies": [{"name":"p","listenAddr":"127.0.0.1:18476","targetUrl":"https://10.0.0.5:8443","tokenFile":"t","allowNonLoopback":true}],
		"gateway": {"routes":[
			{"slug":"r-inferred","service":"remote","tokenFile":"t"},
			{"slug":"r-custom","service":"remote","targetUrl":"https://10.0.0.99:9000","tokenFile":"t","allowNonLoopback":true}
		]}
	}`)); err != nil {
		t.Fatalf("expected allowNonLoopback=true to succeed: %v", err)
	}
}

func TestNormalizeBindKey(t *testing.T) {
	if normalizeBindKey("127.0.0.1:18470") != normalizeBindKey("localhost:18470") {
		t.Fatalf("expected 127.0.0.1 and localhost to normalize to same key: %q vs %q",
			normalizeBindKey("127.0.0.1:18470"), normalizeBindKey("localhost:18470"))
	}
	if normalizeBindKey("127.0.0.1:18470") == normalizeBindKey("[::1]:18470") {
		t.Fatalf("expected IPv4 loopback and [::1] to have distinct keys")
	}
	if normalizeBindKey("127.0.0.1:0") != "" {
		t.Fatalf("expected port 0 to normalize to empty string")
	}
}

func TestValidateSecretFilesAndCollectAllSecrets(t *testing.T) {
	dir := t.TempDir()
	writeTok := func(name, val string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(val+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	ctrlTok := writeTok("control.token", "control-secret-1111")
	writeTok("control.token.next", "control-next-2222")
	proxyTok := writeTok("proxy.token", "proxy-secret-3333")
	upstreamTok := writeTok("upstream.token", "upstream-secret-4444")
	reloadTok := writeTok("reload.token", "reload-secret-5555")
	gwTok := writeTok("gw.token", "gateway-secret-6666")
	envTok := writeTok("env.token", "envfile-secret-7777")
	utf16Tok := filepath.Join(dir, "utf16.token")
	if err := os.WriteFile(utf16Tok, []byte{0xFF, 0xFE, 'u', 0x00, 'p', 0x00}, 0o600); err != nil {
		t.Fatal(err)
	}

	validCfg := &Config{
		ControlAddr:      "127.0.0.1:18470",
		ControlTokenFile: ctrlTok,
		LogDir:           dir,
		Services: []ServiceConfig{
			{
				Name:            "svc",
				Command:         "node.exe",
				Port:            18488,
				EnvFiles:        map[string]string{"API_KEY": envTok},
				ReloadURL:       "http://127.0.0.1:18488/admin/reload",
				ReloadTokenFile: reloadTok,
			},
		},
		Proxies: []ProxyConfig{
			{
				Name:              "docs-proxy",
				ListenAddr:        "127.0.0.1:18478",
				TargetURL:         "http://127.0.0.1:18488",
				TokenFile:         proxyTok,
				UpstreamTokenFile: upstreamTok,
			},
		},
		Gateway: &GatewayConfig{
			ListenAddr: "127.0.0.1:18471",
			Routes: []GatewayRouteConfig{
				{
					Slug:      "gw-svc",
					Service:   "svc",
					TargetURL: "http://127.0.0.1:18488",
					Tokens:    []RouteTokenConfig{{Name: "main", TokenFile: gwTok}},
				},
			},
		},
	}
	if err := ValidateConfig(validCfg); err != nil {
		t.Fatalf("ValidateConfig failed: %v", err)
	}
	if err := ValidateSecretFiles(validCfg); err != nil {
		t.Fatalf("ValidateSecretFiles failed on valid files: %v", err)
	}

	collected := CollectAllSecrets(validCfg)
	for _, want := range []string{
		"control-secret-1111", "control-next-2222", "proxy-secret-3333",
		"upstream-secret-4444", "reload-secret-5555", "gateway-secret-6666", "envfile-secret-7777",
	} {
		if !slices.Contains(collected, want) {
			t.Errorf("CollectAllSecrets missing %q (got %v)", want, collected)
		}
	}

	// Missing reloadTokenFile fails validation.
	badReload := *validCfg
	badReload.Services = []ServiceConfig{{
		Name:            "svc",
		Command:         "node.exe",
		Port:            18488,
		ReloadURL:       "http://127.0.0.1:18488/admin/reload",
		ReloadTokenFile: filepath.Join(dir, "missing.token"),
	}}
	if err := ValidateSecretFiles(&badReload); err == nil {
		t.Fatalf("expected error for missing reloadTokenFile")
	}

	// Bad UTF-16 upstreamTokenFile fails validation.
	utf16Cfg := *validCfg
	utf16Cfg.Proxies = []ProxyConfig{{
		Name:              "docs-proxy",
		ListenAddr:        "127.0.0.1:18478",
		TargetURL:         "http://127.0.0.1:18488",
		TokenFile:         proxyTok,
		UpstreamTokenFile: utf16Tok,
	}}
	if err := ValidateSecretFiles(&utf16Cfg); err == nil {
		t.Fatalf("expected error for UTF-16 upstreamTokenFile")
	}

	// Bad .next grace file fails validation.
	badNextPath := writeTok("grace.token", "valid-primary-tok")
	if err := os.WriteFile(badNextPath+".next", []byte("bad token with space"), 0o600); err != nil {
		t.Fatal(err)
	}
	badNextCfg := *validCfg
	badNextCfg.ControlTokenFile = badNextPath
	if err := ValidateSecretFiles(&badNextCfg); err == nil {
		t.Fatalf("expected error for malformed .next grace token file")
	}
}

func TestResolvePortableDefaultPaths(t *testing.T) {
	if DefaultMutexName != `Local\McpHubSupervisor` {
		t.Fatalf("DefaultMutexName = %q, want %q", DefaultMutexName, `Local\McpHubSupervisor`)
	}
	if Version != "0.3.0" {
		t.Fatalf("Version = %q, want 0.3.0", Version)
	}

	t.Run("env var overrides take precedence", func(t *testing.T) {
		cfgDir := t.TempDir()
		customCfg := filepath.Join(cfgDir, "custom.json")
		customLogs := filepath.Join(cfgDir, "custom-logs")
		t.Setenv("MCP_HUB_CONFIG", customCfg)
		t.Setenv("MCP_HUB_LOG_DIR", customLogs)

		if got := ResolveDefaultConfigPath(); got != filepath.Clean(customCfg) {
			t.Fatalf("ResolveDefaultConfigPath() = %q, want %q", got, filepath.Clean(customCfg))
		}
		if got := ResolveDefaultLogDir(); got != filepath.Clean(customLogs) {
			t.Fatalf("ResolveDefaultLogDir() = %q, want %q", got, filepath.Clean(customLogs))
		}
	})

	t.Run("installation.json adjacent to executable is honored", func(t *testing.T) {
		t.Setenv("MCP_HUB_CONFIG", "")
		t.Setenv("MCP_HUB_LOG_DIR", "")
		installDir := t.TempDir()
		fakeExe := filepath.Join(installDir, "mcp-hub.exe")
		wantCfg := filepath.Join(t.TempDir(), "data", "config.json")
		receipt := append([]byte{0xEF, 0xBB, 0xBF}, []byte(`{"config_path": "`+filepath.ToSlash(wantCfg)+`"}`)...)
		if err := os.WriteFile(filepath.Join(installDir, "installation.json"), receipt, 0o600); err != nil {
			t.Fatal(err)
		}
		got := resolveDefaultConfigPath("windows", func() (string, error) { return fakeExe, nil })
		if got != filepath.Clean(wantCfg) {
			t.Fatalf("resolveDefaultConfigPath with receipt = %q, want %q", got, filepath.Clean(wantCfg))
		}
	})

	t.Run("platform defaults when env and receipt absent", func(t *testing.T) {
		t.Setenv("MCP_HUB_CONFIG", "")
		t.Setenv("MCP_HUB_LOG_DIR", "")
		noExe := func() (string, error) { return filepath.Join(t.TempDir(), "mcp-hub"), nil }

		winBase := t.TempDir()
		t.Setenv("LOCALAPPDATA", winBase)
		gotWin := resolveDefaultConfigPath("windows", noExe)
		wantWin := filepath.Join(winBase, "McpHub", "config.json")
		if gotWin != wantWin {
			t.Fatalf("windows default = %q, want %q", gotWin, wantWin)
		}

		xdgBase := t.TempDir()
		t.Setenv("XDG_CONFIG_HOME", xdgBase)
		gotUnix := resolveDefaultConfigPath("linux", noExe)
		wantUnix := filepath.Join(xdgBase, "mcp-hub", "config.json")
		if gotUnix != wantUnix {
			t.Fatalf("linux default = %q, want %q", gotUnix, wantUnix)
		}
	})
}

func TestConfigExampleAndLiveSchemaBackwardsCompat(t *testing.T) {
	cfg, err := LoadConfig(filepath.Join("..", "..", "config.example.json"))
	if err != nil {
		t.Fatalf("LoadConfig(config.example.json): %v", err)
	}
	if len(cfg.Services) == 0 {
		t.Fatalf("expected at least 1 example service, got 0")
	}

	// Verify exact v0.1.0 schema structure loads with zero breaking changes.
	legacyJSON := `{
		"controlAddr": "127.0.0.1:18470",
		"controlTokenFile": "C:/Users/example/AppData/Local/McpHub/secrets/control.token",
		"logDir": "C:/Users/example/AppData/Local/McpHub/logs",
		"services": [
			{"name":"search-mcp","command":"node.exe","healthUrl":"http://127.0.0.1:18473/healthz","healthIntervalSec":15,"stripProxyEnv":true,"shutdownUrl":"http://127.0.0.1:18473/admin/shutdown","shutdownTokenFile":"C:/Users/example/AppData/Local/McpHub/secrets/search.token"},
			{"name":"docs-mcp","command":"node.exe","healthUrl":"http://127.0.0.1:18484/health","healthIntervalSec":15,"stripProxyEnv":true},
			{"name":"notes-mcp","command":"node.exe","healthUrl":"http://127.0.0.1:18475/healthz","healthIntervalSec":15,"stripProxyEnv":true},
			{"name":"browser-mcp","command":"node.exe","healthUrl":"tcp://127.0.0.1:18486","healthIntervalSec":15,"stripProxyEnv":true},
			{"name":"tasks-mcp","command":"node.exe","healthUrl":"http://127.0.0.1:18477/healthz","healthIntervalSec":15,"stripProxyEnv":true}
		],
		"proxies": [
			{"name":"docs-auth-proxy","listenAddr":"127.0.0.1:18474","targetUrl":"http://127.0.0.1:18484","tokenFile":"C:/Users/example/AppData/Local/McpHub/secrets/docs.token","upstreamTokenFile":"C:/Users/example/AppData/Local/McpHub/secrets/docs-up.token","healthPath":"/health"},
			{"name":"browser-auth-proxy","listenAddr":"127.0.0.1:18476","targetUrl":"http://127.0.0.1:18486","tokenFile":"C:/Users/example/AppData/Local/McpHub/secrets/browser.token"}
		]
	}`
	legacyCfg, err := ParseConfig([]byte(legacyJSON))
	if err != nil {
		t.Fatalf("legacy v0.1.0 config failed to parse: %v", err)
	}
	if legacyCfg.Proxies[0].UpstreamService != "docs-mcp" {
		t.Errorf("docs proxy inferred UpstreamService = %q, want docs-mcp", legacyCfg.Proxies[0].UpstreamService)
	}
	if legacyCfg.Proxies[1].UpstreamService != "browser-mcp" {
		t.Errorf("browser proxy inferred UpstreamService = %q, want browser-mcp", legacyCfg.Proxies[1].UpstreamService)
	}
	if legacyCfg.Services[0].EffectiveStartTimeout() != 15*time.Second {
		t.Errorf("default start timeout = %s, want 15s", legacyCfg.Services[0].EffectiveStartTimeout())
	}
}
