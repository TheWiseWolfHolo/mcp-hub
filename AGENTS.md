# mcp-hub Contributor & Agent Guide

`mcp-hub` is a zero-dependency Go standard-library local MCP daemon supervisor, STDIO-to-Streamable-HTTP bridge, authenticated reverse proxy, and single-port loopback gateway.

## Core Principles

1. **Zero third-party Go dependencies**: Keep `go.mod` free of `require` directives. Use the Go standard library, native Windows DLL syscalls for Windows-only features, and embedded static assets (`embed.FS`, vanilla HTML/CSS/JS, no CDN or npm build step).
2. **Never expose raw secrets**: Never print, log, commit, or embed raw token values, `envFiles` contents, or `.next` files in stdout, logs, tests, screenshots, or documentation. Refer to tokens only by file path and short `sha256:` fingerprint.
3. **Never touch a live installation during development**: Do not run the daemon long-term from the source tree, do not manually copy binaries into `%LOCALAPPDATA%\Programs\McpHub`, and never read or mutate the developer's live `%LOCALAPPDATA%\McpHub` directory in tests. Use `t.TempDir()`, temporary `--config` files, and `httptest`.
4. **Preserve cross-platform builds**: Windows-specific code belongs in `*_windows.go` with matching stubs in `*_other.go`. Both Windows and Linux (`GOOS=linux go vet ./...`) must pass cleanly.

## Repository Map

| Path | Responsibility |
| --- | --- |
| `cmd/mcp-hub/` | Windowless supervisor binary (`main.go`) and embedded Windows resources (`rsrc_windows_*.syso`) |
| `cmd/mcp-hub-ctl/` | Console CLI (`main.go`, `probe_targets.go`), exit codes, `--json` error envelopes, and `.syso` resources |
| `internal/hub/version.go` | Version constant (`0.3.0`), ldflags-injectable `GitCommit`, portable default path resolution |
| `internal/hub/config.go` | Config schema, default inference, validation, `ReadSecretFile`, `CollectAllSecrets` |
| `internal/hub/supervisor.go` | Lifecycle orchestrator, hot reload, catalog, tray snapshot, events, `hub.Client` |
| `internal/hub/service.go` / `stdio_bridge.go` | HTTP child supervision and STDIO-to-Streamable-HTTP bridge (lazy start, idle sleep, backoff) |
| `internal/hub/proxy.go` / `gateway.go` | Authenticated reverse proxies and `/s/<slug>` single-port gateway |
| `internal/hub/httpguard.go` / `mcp_inspect.go` | Loopback guard, `TokenStore`, stream lifetime revocation, MCP tool ACL filtering and metrics |
| `internal/hub/control.go` | Loopback HTTP control API, SSE logs/events, optional `/ui` mount hook |
| `internal/hub/redact.go` / `ringlog.go` / `rotlog.go` | Secret redaction, in-memory ring buffer, rotating log files |
| `internal/hub/winproc_*.go` | Windows Job Objects, single-instance mutex, private NTFS ACL writer and auditor |
| `internal/hub/winlifecycle_*.go` | Shared tray snapshot types (`winlifecycle_types.go`), Windows tray/power lifecycle, non-Windows stub |
| `internal/hub/trayui/` | Pure-Go vector icons, Tri-Node Hub icon generator, Light/Dark tokens, Control Popover & Quick Menu renderer |
| `internal/hub/webui/` | Embedded local Web Dashboard (`/ui`), nonce/cookie/CSRF auth, live SSE logs, tool analytics & MCP explorer |
| `tools/genwinres/` | `//go:build ignore` pure-Go COFF `.syso` generator (multi-size PNG ICO, PerMonitorV2 manifest, `VS_VERSION_INFO`) |
| `install-local.ps1` | Windows PowerShell 5.1+ compatible local installer/upgrader with rollback and ACL verification |

## Security Invariants (Do Not Weaken)

- All listeners and upstreams default to loopback (`127.0.0.1`, `localhost`, `::1`); non-loopback upstreams require explicit per-item `allowNonLoopback: true` (never silently inherited by custom gateway `targetUrl` routes).
- Control server fails closed when `controlTokenFile` is omitted unless `allowInsecureNoControlAuth: true` is explicitly set.
- `CheckLoopbackGuard` runs in strict order: `Host` allowlist -> non-empty `Origin` -> `OPTIONS` -> `Sec-Fetch-Site` -> `403`. Bearer tokens use SHA-256 + `subtle.ConstantTimeCompare` across all rules.
- Embedded Web Dashboard (`/ui`) enforces loopback `Host`, single-use 256-bit bootstrap nonces redirected via `303 See Other`, `HttpOnly; SameSite=Strict` session cookies, exact `Origin` + `Sec-Fetch-Site: same-origin` + `X-McpHub-CSRF` on all `POST /ui/api/*`, and strict CSP (`frame-ancestors 'none'`).
- Secret files are written only via `WritePrivateFileAtomic` with owner-only protected DACLs (`D:P(A;;FA;;;<SID>)` for files, `D:P(A;OICI;FA;;;<SID>)` for directories).
- All logs, `/status` diagnostics, `/logs`, CLI outputs, and Web UI responses pass through `Redactor`.
- Keep `/healthz`, `/status`, and `/catalog` JSON schemas backward-compatible (additive fields with `omitempty` only). `install-local.ps1` depends on `/healthz` returning `ok` and `services`.

## CLI Exit Codes

`0` ok, `1` runtime/unhealthy/probe failure, `2` usage/invalid config, `3` hub unreachable/timeout, `4` auth/guard (`401`/`403`), `5` not found (`404`), `6` audit violation. With `--json`, errors write `{"ok":false,"error":{"code","message","hint"}}` to stdout.

## Verification Checklist

Run from the repository root (set `$env:GOTMPDIR = $env:TEMP` on Windows if needed):

```powershell
# Go static analysis (Windows + Linux)
go vet ./...
$env:GOOS = 'linux'; go vet ./...; Remove-Item Env:GOOS

# Full race-enabled unit test suite (includes config.example.json validation)
go test -race -count=1 ./...

# Build both binaries
go build ./cmd/mcp-hub ./cmd/mcp-hub-ctl
```

When editing `install-local.ps1`:

- Keep 100% Windows PowerShell 5.1 compatibility (no `&&`, no ternary `?:`, two-argument `Join-Path` only, SHA-256 via `.NET` API).
- Parse with `[System.Management.Automation.Language.Parser]::ParseFile(...)` in both `pwsh` and `powershell.exe -NoProfile` and confirm zero syntax errors.

When changing config schema or embedded Windows icons:

- Ensure `config.example.json` passes `hub.LoadConfig` (`go test -run TestConfigExample ./internal/hub`).
- Regenerate `.syso` resources with `go run ./tools/genwinres` and commit the updated `cmd/*/rsrc_windows_*.syso` files.
- Keep `internal/hub/version.go` aligned with release tags and docs.
