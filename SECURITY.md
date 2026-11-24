# Security Policy

## Threat Model

`mcp-hub` is a localhost-first single-user workstation daemon. It supervises local MCP servers, exposes authenticated loopback proxies and a single-port gateway, and serves a local control API and embedded Web Dashboard.

The design defends against:

- **Cross-origin browser attacks**: DNS rebinding, CSRF, cross-site fetches, CORS preflights, and malicious tabs targeting localhost ports.
- **Accidental network exposure**: Binding to non-loopback interfaces or forwarding traffic to remote hosts without explicit opt-in.
- **Local multi-user secret exposure**: Other local accounts on Windows reading configuration files, token files, or logs.
- **Credential leakage in telemetry**: Tokens or environment secrets appearing in logs, `/status` error fields, CLI `--json` output, config snippets, or the Web Dashboard.
- **Orphaned child processes**: Managed servers or browser subtrees surviving after the supervisor exits or crashes.

Explicit non-goals:

- `mcp-hub` is not an internet-facing multi-tenant gateway or TLS terminator.
- Compromised processes already running under the same OS user (or Windows Administrators with raw disk/process privileges) are outside the OS user isolation boundary.

## Security Invariants

1. **Loopback-Only by Default**
   - `controlAddr`, proxy `listenAddr`, `gateway.listenAddr`, and all upstream/health/shutdown/reload URLs must resolve to loopback (`127.0.0.1`, `localhost`, or `::1`).
   - Non-loopback upstream URLs require explicit per-service, per-proxy, or per-route `allowNonLoopback: true`. Gateway routes with an explicit custom `targetUrl` must declare `allowNonLoopback: true` on the route itself and never inherit it implicitly from a linked service.
   - `mcp-hub-ctl audit` flags any `allowNonLoopback: true` entry.
   - `mcp-hub-ctl --addr` rejects non-loopback addresses and refuses to attach the config's control token when `--addr` differs from `controlAddr` unless `--token-file` is explicitly supplied.

2. **Loopback HTTP Guard (`CheckLoopbackGuard`)**
   - Every control, proxy, and gateway request runs `CheckLoopbackGuard` before authentication, in strict order:
     1. `Host` header allowlist (`127.0.0.1:<port>`, `localhost:<port>`, `[::1]:<port>`, plus configured `allowedHosts`).
     2. Reject any non-empty `Origin` header (`403 forbidden_origin`).
     3. Reject `OPTIONS` preflight requests (`403 forbidden_preflight`).
     4. Reject browser `Sec-Fetch-Site` headers (`cross-site`, `same-site`, `same-origin`, and any unrecognized value; `none` is permitted only for direct `GET`/`HEAD` requests to `/healthz`).

3. **Constant-Time Bearer Auth & Zero-Downtime Rotation**
   - The control API fails closed on startup unless `controlTokenFile` is configured (or `allowInsecureNoControlAuth: true` is explicitly set).
   - All Bearer tokens are stored as SHA-256 digests and verified with `crypto/subtle.ConstantTimeCompare` across all rules in the store without early exit.
   - Proxies and gateway routes enforce per-route token isolation: a token for `/s/a` cannot authenticate `/s/b`.
   - When `<tokenFile>.next` exists, both tokens are accepted during rotation. Once committed and reloaded, active SSE and MCP streams bound to revoked tokens or changed ACL rules are cancelled immediately.

4. **Per-Token Tool ACLs (Fail-Closed)**
   - Tokens can restrict access with `allowTools`, `denyTools` (glob patterns `*` and `?`), and `readOnlyOnly: true` (requiring MCP `readOnlyHint: true`).
   - `tools/list` filtering drops unparseable or non-object entries whenever `allowTools` or `readOnlyOnly` is active.
   - Denied `tools/call` requests return a JSON-RPC error response with `X-Content-Type-Options: nosniff` and `Referrer-Policy: no-referrer`.

5. **Embedded Web Dashboard Boundary (`/ui`)**
   - Mounted on the control listener without weakening any existing Bearer-authenticated API route.
   - Enforces the same loopback `Host` allowlist on every `/ui` and `/ui/*` request (`403` otherwise).
   - Authentication uses a single-use 256-bit bootstrap nonce (60-second TTL, max 16 active) minted by the System Tray or `POST /ui/session` (Bearer-authenticated). Visiting `/ui/?bootstrap=<nonce>` atomically consumes the nonce, sets an `HttpOnly; SameSite=Strict; Path=/ui` session cookie (`mcphub_ui`, 12-hour sliding TTL, max 8 concurrent sessions), and redirects `303 See Other` to `/ui/` so the nonce is stripped from the address bar and browser history.
   - Direct unlock via `POST /ui/api/login` verifies the control token in constant time and is rate-limited.
   - `GET /ui/api/*` requires a valid session cookie and `Sec-Fetch-Site` in `{"same-origin", "none"}`.
   - `POST /ui/api/*` requires a valid session cookie, exact `Origin` match against `http://<requestHost>`, `Sec-Fetch-Site: same-origin`, and a matching `X-McpHub-CSRF` header.
   - Responses carry strict headers: `Content-Security-Policy: default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'`, `X-Content-Type-Options: nosniff`, `Referrer-Policy: no-referrer`, and `Cache-Control: no-store`.

6. **Private NTFS Storage & Secret Redaction**
   - Secrets live only in external files, never inline in `config.json`. `ReadSecretFile` caps files at 8 KiB and rejects UTF-16 BOMs, NUL bytes, control characters, multi-line files, and non-ASCII bytes.
   - Secret files are written via `WritePrivateFileAtomic`, which creates a temporary file with an owner-only protected DACL (`D:P(A;;FA;;;<SID>)` for files, `D:P(A;OICI;FA;;;<SID>)` for directories), flushes to disk, and atomically replaces the target.
   - `install-local.ps1` and `mcp-hub-ctl audit --fix-acl` refuse to traverse or modify reparse points (symlinks, junctions, mount points).
   - All logs, `/status` diagnostics (`lastError`, `lastStderr`, `crashTail`), `/logs` output, CLI output, and Web Dashboard responses pass through `Redactor`, replacing known secret values and common Bearer/API-key patterns with `[REDACTED]`.

7. **Resource Limits & Process Isolation**
   - Request bodies are capped at 64 KiB on the control API and 32 MiB on proxy/gateway routes (`413`).
   - `stripProxyEnv` defaults to `true`, removing inherited `HTTP_PROXY`, `HTTPS_PROXY`, and `ALL_PROXY` from child services and setting `NO_PROXY=localhost,127.0.0.1,::1`.
   - On Windows, child services run in `KILL_ON_JOB_CLOSE` Job Objects with `CREATE_NO_WINDOW`.

## Supported Versions

Security fixes are provided for the latest `0.x` minor release (`0.3.x`).

## Reporting a Vulnerability

Please do not open a public GitHub issue for suspected security vulnerabilities.

Instead, report privately through **GitHub Security Advisories** ("Report a vulnerability" in the repository's **Security** tab). Include:

- `mcp-hub` version (`mcp-hub-ctl version --json`) and OS/architecture.
- Affected component (control API, gateway, proxy, stdio-bridge, Web UI, tray, CLI, or installer).
- Minimal sanitized configuration and step-by-step reproduction instructions.
- Never include live tokens, private keys, or personal data in your report.

We aim to acknowledge reports within 72 hours and coordinate a fix and advisory before public disclosure.
