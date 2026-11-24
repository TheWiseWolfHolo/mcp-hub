// Command mcp-hub-ctl is the console control CLI for the mcp-hub supervisor.
//
// Exit codes: 0 ok, 1 runtime/unhealthy, 2 usage/invalid config, 3 hub unreachable/timeout,
// 4 auth (401/403), 5 not found (404), 6 audit violation.
//
// Raw secret values are never printed: tokens are referenced by file path and short SHA-256 fingerprint only.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/TheWiseWolfHolo/mcp-hub/internal/hub"
)

const (
	exitOK          = 0
	exitRuntime     = 1
	exitUsage       = 2
	exitUnreachable = 3
	exitAuth        = 4
	exitNotFound    = 5
	exitAudit       = 6
)

const (
	defaultTimeout       = 20 * time.Second
	defaultWatchInterval = 2 * time.Second
	defaultLogLines      = 50
	maxOnlineLogLines    = 500
	maxOfflineLogLines   = 5000
	minTokenChars        = 32
	probeProtocolVersion = "2025-06-18"
)

var nameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)

// Test hooks for external process launches (never mutate live host processes during unit tests).
var (
	findHubBinaryFn = findHubBinary
	launchHubFn     = launchHubDetached
	openExternalFn  = openExternal
)

// cliError carries a stable exit code plus a machine-readable error code for --json output.
type cliError struct {
	exit    int
	code    string
	message string
	hint    string
	extra   map[string]any
	silent  bool // output already written; only the exit code matters
}

func (e *cliError) Error() string { return e.message }

func usageErr(msg, hint string) *cliError {
	return &cliError{exit: exitUsage, code: "usage", message: msg, hint: hint}
}

type cliOptions struct {
	configPath     string
	configExplicit bool
	addr           string
	addrSet        bool
	tokenFile      string
	tokenFileSet   bool
	asJSON         bool
	timeout        time.Duration
	timeoutSet     bool

	lines         int
	follow        bool
	offline       bool
	grep          string
	all           bool
	hub           bool
	wait          bool
	watch         bool
	watchInterval time.Duration
	probeTools    bool
	mcp           bool
	wake          bool
	schema        bool
	noOpen        bool
	dir           string
	client        string
	fixACL        bool
	force         bool
	stage         bool
	commit        bool
	noReload      bool

	local map[string]bool // command-specific flags that were set
}

type app struct {
	out          io.Writer
	errOut       io.Writer
	style        termStyle
	errStyle     termStyle
	base         context.Context
	opts         cliOptions
	streamJSON   bool
	controlToken string

	cfg       *hub.Config
	cfgErr    error
	cfgLoaded bool
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	base, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	a := &app{out: stdout, errOut: stderr, base: base}
	opts, pos, err := parseArgs(args)
	a.opts = opts
	a.style = newTermStyle(stdout, opts.asJSON)
	a.errStyle = newTermStyle(stderr, opts.asJSON)
	if err != nil {
		return a.fail(err)
	}
	if len(pos) == 0 {
		if opts.asJSON {
			return a.fail(usageErr("missing command", "run `mcp-hub-ctl help` for the command list"))
		}
		printUsage(stderr)
		return exitUsage
	}
	cmd := canonicalCommand(pos[0])
	a.streamJSON = (cmd == "status" && opts.watch) || (cmd == "logs" && opts.follow)
	if err := checkCommandFlags(cmd, opts); err != nil {
		return a.fail(err)
	}
	if opts.addrSet {
		if _, err := normalizeControlAddr(opts.addr); err != nil {
			return a.fail(err)
		}
	}
	if err := a.dispatch(cmd, pos[1:]); err != nil {
		return a.fail(err)
	}
	return exitOK
}

func (a *app) dispatch(cmd string, rest []string) error {
	switch cmd {
	case "help":
		if a.opts.asJSON {
			var buf bytes.Buffer
			printUsage(&buf)
			a.writeJSON(map[string]any{"ok": true, "usage": buf.String()})
			return nil
		}
		printUsage(a.out)
		return nil
	case "version":
		return a.cmdVersion(rest)
	case "validate":
		return a.cmdValidate(rest)
	case "health":
		return a.cmdHealth(rest)
	case "status":
		return a.cmdStatus(rest)
	case "stats":
		return a.cmdStats(rest)
	case "tools":
		return a.cmdTools(rest)
	case "logs":
		return a.cmdLogs(rest)
	case "start":
		return a.cmdStart(rest)
	case "stop":
		return a.cmdStop(rest)
	case "sleep":
		return a.cmdSleep(rest)
	case "wake":
		return a.cmdWake(rest)
	case "shutdown":
		return a.cmdShutdown(rest)
	case "restart":
		return a.cmdServiceAction("restart", rest)
	case "reload":
		return a.cmdReload(rest)
	case "catalog":
		return a.cmdCatalog(rest)
	case "probe":
		return a.cmdProbe(rest)
	case "snippet":
		return a.cmdSnippet(rest)
	case "ui":
		return a.cmdUI(rest)
	case "open-logs":
		return a.cmdOpenLogs(rest)
	case "open-config":
		return a.cmdOpenConfig(rest)
	case "init":
		return a.cmdInit(rest)
	case "audit":
		return a.cmdAudit(rest)
	case "init-token":
		return a.cmdInitToken(rest)
	case "rotate-token":
		return a.cmdRotateToken(rest)
	default:
		return usageErr(fmt.Sprintf("unknown command %q", cmd), "run `mcp-hub-ctl help` for the command list")
	}
}

// ---------------------------------------------------------------------------
// Argument parsing
// ---------------------------------------------------------------------------

var shortValueFlagNames = map[string]string{
	"c": "config",
	"n": "lines",
}

var longValueFlagNames = map[string]string{
	"config":     "config",
	"addr":       "addr",
	"token-file": "token-file",
	"timeout":    "timeout",
	"lines":      "lines",
	"client":     "client",
	"grep":       "grep",
	"dir":        "dir",
}

var shortBoolFlagNames = map[string]string{
	"f": "follow",
}

var longBoolFlagNames = map[string]string{
	"json":        "json",
	"follow":      "follow",
	"offline":     "offline",
	"all":         "all",
	"hub":         "hub",
	"wait":        "wait",
	"probe-tools": "probe-tools",
	"mcp":         "mcp",
	"wake":        "wake",
	"schema":      "schema",
	"no-open":     "no-open",
	"fix-acl":     "fix-acl",
	"force":       "force",
	"stage":       "stage",
	"commit":      "commit",
	"no-reload":   "no-reload",
}

var globalFlags = map[string]bool{"config": true, "addr": true, "token-file": true, "json": true, "timeout": true}

var commandFlags = map[string][]string{
	"help":         nil,
	"version":      nil,
	"validate":     nil,
	"health":       nil,
	"status":       {"watch"},
	"stats":        nil,
	"tools":        {"wake", "schema"},
	"logs":         {"lines", "follow", "offline", "grep"},
	"start":        {"all", "hub", "wait"},
	"stop":         {"all", "hub", "wait"},
	"sleep":        nil,
	"wake":         nil,
	"shutdown":     {"wait"},
	"restart":      {"all", "wait"},
	"reload":       nil,
	"catalog":      {"probe-tools", "wake"},
	"probe":        {"mcp", "wake"},
	"snippet":      {"client"},
	"ui":           {"no-open"},
	"open-logs":    nil,
	"open-config":  nil,
	"init":         {"dir", "force"},
	"audit":        {"fix-acl"},
	"init-token":   {"force"},
	"rotate-token": {"stage", "commit", "no-reload", "force"},
}

func canonicalCommand(c string) string {
	switch c {
	case "healthz":
		return "health"
	case "doctor":
		return "probe"
	case "metrics":
		return "stats"
	case "-h", "--help":
		return "help"
	}
	return c
}

// looksLikeFlag reports whether s is formatted like a CLI flag (e.g. "-f", "--json"),
// while allowing negative numeric values like "-5" or "-1s" to reach value parsers for clearer errors.
func looksLikeFlag(s string) bool {
	if len(s) < 2 || s[0] != '-' {
		return false
	}
	return s[1] == '-' || s[1] < '0' || s[1] > '9'
}

func parseFlagToken(a string) (name, val string, hasEq, isLong, ok bool) {
	if strings.HasPrefix(a, "---") {
		return "", "", false, false, false
	}
	if strings.HasPrefix(a, "--") {
		rest := a[2:]
		name, val, hasEq = strings.Cut(rest, "=")
		if len(name) <= 1 || strings.HasPrefix(name, "-") {
			return "", "", false, true, false
		}
		return name, val, hasEq, true, true
	}
	if strings.HasPrefix(a, "-") && len(a) > 1 {
		rest := a[1:]
		name, val, hasEq = strings.Cut(rest, "=")
		if len(name) != 1 || name == "-" {
			return "", "", false, false, false
		}
		return name, val, hasEq, false, true
	}
	return "", "", false, false, false
}

func parseArgs(args []string) (cliOptions, []string, error) {
	opts := cliOptions{
		configPath:    hub.ResolveDefaultConfigPath(),
		timeout:       defaultTimeout,
		lines:         defaultLogLines,
		watchInterval: defaultWatchInterval,
		client:        "all",
		local:         map[string]bool{},
	}
	var pos []string
	var parseErr error
	recordError := func(err error) {
		if parseErr == nil {
			parseErr = err
		}
	}
	var earlyCommand string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			pos = append(pos, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			pos = append(pos, a)
			continue
		}
		name, val, hasEq, isLong, ok := parseFlagToken(a)
		if !ok {
			recordError(usageErr(fmt.Sprintf("unknown flag %q", a), "run `mcp-hub-ctl help` for the flag list"))
			continue
		}

		if (!isLong && name == "h") || (isLong && name == "help") {
			if hasEq {
				recordError(usageErr(fmt.Sprintf("flag %s does not accept a value", a), ""))
				continue
			}
			if earlyCommand == "" {
				earlyCommand = "help"
			}
			continue
		}
		if (!isLong && name == "v") || (isLong && name == "version") {
			if hasEq {
				recordError(usageErr(fmt.Sprintf("flag %s does not accept a value", a), ""))
				continue
			}
			if earlyCommand == "" {
				earlyCommand = "version"
			}
			continue
		}
		if isLong && name == "watch" {
			opts.watch = true
			opts.local["watch"] = true
			if hasEq {
				d, err := time.ParseDuration(val)
				if err != nil || d <= 0 {
					recordError(usageErr(fmt.Sprintf("invalid --watch interval %q", val), "use e.g. --watch 2s"))
					continue
				}
				opts.watchInterval = d
			} else if i+1 < len(args) {
				if d, err := time.ParseDuration(args[i+1]); err == nil {
					i++
					if d <= 0 {
						recordError(usageErr(fmt.Sprintf("invalid --watch interval %q", args[i]), "use e.g. --watch 2s"))
						continue
					}
					opts.watchInterval = d
				}
			}
			continue
		}

		var canon string
		var isVal, isBool bool
		if isLong {
			canon, isVal = longValueFlagNames[name]
			if !isVal {
				canon, isBool = longBoolFlagNames[name]
			}
		} else {
			canon, isVal = shortValueFlagNames[name]
			if !isVal {
				canon, isBool = shortBoolFlagNames[name]
			}
		}

		if isVal {
			if !hasEq {
				if i+1 >= len(args) || looksLikeFlag(args[i+1]) {
					recordError(usageErr(fmt.Sprintf("flag %s requires a value", a), ""))
					continue
				}
				i++
				val = args[i]
			}
			switch canon {
			case "config":
				opts.configPath = val
				opts.configExplicit = true
			case "addr":
				opts.addr = val
				opts.addrSet = true
			case "token-file":
				opts.tokenFile = val
				opts.tokenFileSet = true
			case "timeout":
				d, err := time.ParseDuration(val)
				if err != nil || d <= 0 {
					recordError(usageErr(fmt.Sprintf("invalid --timeout %q", val), "use a Go duration such as 20s or 1m"))
					continue
				}
				opts.timeout = d
				opts.timeoutSet = true
			case "lines":
				n, err := strconv.Atoi(val)
				if err != nil || n < 1 {
					recordError(usageErr(fmt.Sprintf("invalid line count %q", val), "use a positive integer, e.g. -n 100"))
					continue
				}
				opts.lines = n
				opts.local["lines"] = true
			case "client":
				opts.client = strings.ToLower(strings.TrimSpace(val))
				opts.local["client"] = true
			case "grep":
				opts.grep = val
				opts.local["grep"] = true
			case "dir":
				opts.dir = val
				opts.local["dir"] = true
			}
			continue
		}

		if isBool {
			v := true
			if hasEq {
				b, err := strconv.ParseBool(val)
				if err != nil {
					recordError(usageErr(fmt.Sprintf("invalid boolean value for %s", a), ""))
					continue
				}
				v = b
			}
			switch canon {
			case "json":
				opts.asJSON = v
			case "follow":
				opts.follow = v
			case "offline":
				opts.offline = v
			case "all":
				opts.all = v
			case "hub":
				opts.hub = v
			case "wait":
				opts.wait = v
			case "probe-tools":
				opts.probeTools = v
			case "mcp":
				opts.mcp = v
			case "wake":
				opts.wake = v
			case "schema":
				opts.schema = v
			case "no-open":
				opts.noOpen = v
			case "fix-acl":
				opts.fixACL = v
			case "force":
				opts.force = v
			case "stage":
				opts.stage = v
			case "commit":
				opts.commit = v
			case "no-reload":
				opts.noReload = v
			}
			if !globalFlags[canon] {
				opts.local[canon] = true
			}
			continue
		}
		recordError(usageErr(fmt.Sprintf("unknown flag %q", a), "run `mcp-hub-ctl help` for the flag list"))
	}
	if earlyCommand != "" {
		pos = []string{earlyCommand}
		opts.local = nil
	}
	return opts, pos, parseErr
}

func checkCommandFlags(cmd string, opts cliOptions) error {
	allowed, known := commandFlags[cmd]
	if !known {
		return nil
	}
	set := map[string]bool{}
	for _, f := range allowed {
		set[f] = true
	}
	var bad []string
	for f := range opts.local {
		if !set[f] {
			bad = append(bad, "--"+f)
		}
	}
	if len(bad) > 0 {
		sort.Strings(bad)
		return usageErr(fmt.Sprintf("flag %s is not valid for %q", strings.Join(bad, ", "), cmd), "run `mcp-hub-ctl help` for per-command flags")
	}
	return nil
}

// normalizeControlAddr validates an --addr override and returns host:port. Non-loopback hosts are rejected.
func normalizeControlAddr(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if strings.HasPrefix(strings.ToLower(s), "http://") {
		s = s[len("http://"):]
	} else if strings.Contains(s, "://") {
		return "", usageErr(fmt.Sprintf("--addr %q: only plain http host:port is supported", raw), "use e.g. --addr 127.0.0.1:18470")
	}
	s = strings.TrimRight(s, "/")
	host, port, err := net.SplitHostPort(s)
	if err != nil {
		return "", usageErr(fmt.Sprintf("--addr %q must be host:port", raw), "use e.g. --addr 127.0.0.1:18470")
	}
	p, err := strconv.Atoi(port)
	if err != nil || p < 1 || p > 65535 {
		return "", usageErr(fmt.Sprintf("--addr %q has an invalid port", raw), "")
	}
	if !hub.IsLoopbackHost(host) {
		return "", &cliError{exit: exitUsage, code: "non_loopback_addr",
			message: fmt.Sprintf("--addr %q is not a loopback address; mcp-hub-ctl only talks to 127.0.0.1, ::1 or localhost", raw),
			hint:    "the control API never listens on non-loopback interfaces"}
	}
	return net.JoinHostPort(strings.Trim(host, "[]"), strconv.Itoa(p)), nil
}

func bindKey(addr string) string {
	host, port, err := net.SplitHostPort(strings.TrimSpace(addr))
	if err != nil {
		return strings.ToLower(strings.TrimSpace(addr))
	}
	h := strings.ToLower(strings.Trim(host, "[]"))
	if ip := net.ParseIP(h); ip != nil {
		h = ip.String()
	}
	if n, err := strconv.Atoi(port); err == nil {
		port = strconv.Itoa(n)
	}
	return net.JoinHostPort(h, port)
}

func sameBind(a, b string) bool {
	return strings.TrimSpace(b) != "" && bindKey(a) == bindKey(b)
}

// ---------------------------------------------------------------------------
// Config, connection and output helpers
// ---------------------------------------------------------------------------

func (a *app) config(required bool) (*hub.Config, error) {
	if !a.cfgLoaded {
		a.cfgLoaded = true
		a.cfg, a.cfgErr = hub.LoadConfig(a.opts.configPath)
	}
	if a.cfgErr != nil {
		if !required && !a.opts.configExplicit && errors.Is(a.cfgErr, os.ErrNotExist) {
			return nil, nil
		}
		return nil, &cliError{exit: exitUsage, code: "invalid_config", message: a.cfgErr.Error(),
			hint: "fix the config or pass --config <path>; run `mcp-hub-ctl validate` for details"}
	}
	return a.cfg, nil
}

// resolveControl selects the control address and Bearer token file. The config's controlTokenFile is only
// attached when talking to the config's own controlAddr (or when --token-file is explicit).
func (a *app) resolveControl(needAuth bool) (string, string, error) {
	o := a.opts
	var cfg *hub.Config
	if o.configExplicit || !o.addrSet || (needAuth && !o.tokenFileSet) {
		c, err := a.config(false)
		if err != nil {
			return "", "", err
		}
		cfg = c
	}
	addr := hub.DefaultControlAddr
	if cfg != nil && strings.TrimSpace(cfg.ControlAddr) != "" {
		addr = cfg.ControlAddr
	}
	attachCfgToken := cfg != nil
	if o.addrSet {
		norm, err := normalizeControlAddr(o.addr)
		if err != nil {
			return "", "", err
		}
		if cfg == nil || !sameBind(norm, cfg.ControlAddr) {
			attachCfgToken = false
		}
		addr = norm
	}
	tokenFile := ""
	if needAuth {
		if o.tokenFileSet {
			tokenFile = o.tokenFile
		} else if attachCfgToken {
			tokenFile = cfg.ControlTokenFile
		}
	}
	return addr, tokenFile, nil
}

func (a *app) newHubClient(addr, token string) *hub.Client {
	var timeout time.Duration
	if a.opts.timeoutSet {
		timeout = a.opts.timeout
	}
	return hub.NewClientWithTimeout(addr, token, timeout)
}

func (a *app) connect(needAuth bool) (*hub.Client, string, error) {
	addr, tokenFile, err := a.resolveControl(needAuth)
	if err != nil {
		return nil, "", err
	}
	addr, err = loopbackDialAddr(addr)
	if err != nil {
		return nil, "", err
	}
	token := ""
	if strings.TrimSpace(tokenFile) != "" {
		token, err = hub.ReadSecretFile(tokenFile)
		if err != nil {
			return nil, "", &cliError{exit: exitUsage, code: "token_file_unreadable", message: "read control token: " + err.Error(),
				hint: "check controlTokenFile in config or pass --token-file <path>"}
		}
		a.controlToken = token
	}
	return a.newHubClient(addr, token), addr, nil
}

func (a *app) ctx(def time.Duration) (context.Context, context.CancelFunc) {
	t := def
	if a.opts.timeoutSet {
		t = a.opts.timeout
	}
	return context.WithTimeout(a.base, t)
}

func (a *app) effectiveTimeout() time.Duration {
	if a.opts.timeoutSet {
		return a.opts.timeout
	}
	return defaultTimeout
}

func (a *app) plainStatusTimeout() time.Duration {
	if a.opts.timeoutSet {
		return a.opts.timeout
	}
	return 10 * time.Second
}

func (a *app) writeJSON(v any) {
	enc := json.NewEncoder(a.out)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func (a *app) writeCompactJSON(v any) {
	enc := json.NewEncoder(a.out)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func (a *app) writeRawJSON(raw []byte) {
	var buf bytes.Buffer
	if err := json.Indent(&buf, bytes.TrimSpace(raw), "", "  "); err != nil {
		a.writeJSON(map[string]any{"ok": true, "raw": strings.TrimSpace(string(raw))})
		return
	}
	buf.WriteByte('\n')
	_, _ = a.out.Write(buf.Bytes())
}

func (a *app) printf(format string, args ...any) {
	fmt.Fprintf(a.out, format, args...)
}

func (a *app) fail(err error) int {
	ce := a.classify(err)
	if ce.silent {
		return ce.exit
	}
	if a.opts.asJSON {
		errObj := map[string]any{"code": ce.code, "message": ce.message}
		if ce.hint != "" {
			errObj["hint"] = ce.hint
		}
		env := map[string]any{"ok": false, "error": errObj}
		for k, v := range ce.extra {
			if k != "ok" && k != "error" {
				env[k] = v
			}
		}
		if a.streamJSON {
			a.writeCompactJSON(env)
		} else {
			a.writeJSON(env)
		}
		return ce.exit
	}
	fmt.Fprintf(a.errOut, "%s %s\n", a.errStyle.crimsonBold("error:"), ce.message)
	if ce.hint != "" {
		fmt.Fprintf(a.errOut, "%s %s\n", a.errStyle.dim("hint:"), ce.hint)
	}
	return ce.exit
}

func scrubDiagnostic(s, token string) string {
	if token != "" {
		s = strings.NewReplacer(url.QueryEscape(token), hub.RedactedPlaceholder,
			url.PathEscape(token), hub.RedactedPlaceholder, token, hub.RedactedPlaceholder).Replace(s)
	}
	return hub.NewRedactor().Redact(s)
}

func (a *app) classify(err error) *cliError {
	ce := *classifyDiagnostic(err, a.controlToken)
	ce.message = scrubDiagnostic(ce.message, a.controlToken)
	ce.hint = scrubDiagnostic(ce.hint, a.controlToken)
	return &ce
}

var (
	httpStatusRe = regexp.MustCompile(`(?s)^(?:control API )?(.*?) returned HTTP (\d{3}): ?(.*)$`)
	errorCodeRe  = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	tomlBareKey  = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
)

// classify maps arbitrary errors to stable exit codes.
func classify(err error) *cliError {
	return classifyDiagnostic(err)
}

func classifyDiagnostic(err error, tokens ...string) *cliError {
	var ce *cliError
	if errors.As(err, &ce) {
		return ce
	}
	msg := err.Error()
	for _, token := range tokens {
		msg = scrubDiagnostic(msg, token)
	}
	msg = scrubDiagnostic(msg, "")
	if errors.Is(err, context.DeadlineExceeded) {
		return &cliError{exit: exitUnreachable, code: "timeout", message: msg, hint: "the hub did not answer within --timeout; check `mcp-hub-ctl health` and the hub log"}
	}
	if errors.Is(err, context.Canceled) {
		return &cliError{exit: exitRuntime, code: "interrupted", message: "interrupted"}
	}
	if m := httpStatusRe.FindStringSubmatch(msg); m != nil {
		status, _ := strconv.Atoi(m[2])
		detail, code := summarizeBody(m[3])
		for _, token := range tokens {
			detail = scrubDiagnostic(detail, token)
		}
		message := fmt.Sprintf("%s: HTTP %d", m[1], status)
		if detail != "" {
			message += " " + detail
		}
		switch {
		case status == http.StatusUnauthorized:
			return &cliError{exit: exitAuth, code: "unauthorized", message: message,
				hint: "check controlTokenFile / --token-file (with --addr you must pass --token-file explicitly); after rotate-token --commit run `mcp-hub-ctl reload`"}
		case status == http.StatusForbidden:
			return &cliError{exit: exitAuth, code: "forbidden", message: message, hint: "the loopback guard rejected the request (Host/Origin)"}
		case status == http.StatusNotFound:
			if code == "" {
				code = "not_found"
			}
			return &cliError{exit: exitNotFound, code: code, message: message, hint: "run `mcp-hub-ctl status` to list services"}
		case status == http.StatusBadRequest:
			if code == "" {
				code = "bad_request"
			}
			return &cliError{exit: exitUsage, code: code, message: message}
		case status == http.StatusNotImplemented:
			if code == "" {
				code = "not_implemented"
			}
			return &cliError{exit: exitRuntime, code: code, message: message}
		default:
			if code == "" {
				code = "hub_error"
			}
			return &cliError{exit: exitRuntime, code: code, message: message}
		}
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return &cliError{exit: exitUnreachable, code: "timeout", message: msg, hint: "the hub did not answer in time"}
	}
	var op *net.OpError
	if (errors.As(err, &op) && op.Op == "dial") || errors.Is(err, syscall.ECONNREFUSED) {
		return &cliError{exit: exitUnreachable, code: "hub_unreachable", message: msg,
			hint: "is mcp-hub running? start it with `mcp-hub-ctl start --hub --wait` or `mcp-hub run`"}
	}
	return &cliError{exit: exitRuntime, code: "runtime", message: msg}
}

func summarizeBody(body string) (detail, code string) {
	body = strings.TrimSpace(body)
	if body == "" {
		return "", ""
	}
	var parsed map[string]any
	if json.Unmarshal([]byte(body), &parsed) == nil {
		if e, ok := parsed["error"].(string); ok && e != "" {
			detail = e
			if errorCodeRe.MatchString(e) {
				code = e
			}
			if n, ok := parsed["name"].(string); ok && n != "" {
				detail += " (" + n + ")"
			}
			return detail, code
		}
	}
	if len(body) > 300 {
		body = body[:300] + "..."
	}
	return body, ""
}

func isUnreachable(err error) bool {
	return err != nil && classify(err).exit == exitUnreachable
}

func isHTTPStatus(err error, want int) bool {
	if err == nil {
		return false
	}
	m := httpStatusRe.FindStringSubmatch(err.Error())
	if m == nil {
		return false
	}
	code, _ := strconv.Atoi(m[2])
	return code == want
}

func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// ---------------------------------------------------------------------------
// version / validate
// ---------------------------------------------------------------------------

func (a *app) cmdVersion(rest []string) error {
	if len(rest) > 0 {
		return usageErr("version takes no arguments", "")
	}
	if a.opts.asJSON {
		out := map[string]any{"ok": true, "version": hub.Version}
		if hub.GitCommit != "" {
			out["gitCommit"] = hub.GitCommit
		}
		a.writeJSON(out)
		return nil
	}
	if hub.GitCommit != "" {
		a.printf("mcp-hub-ctl %s (%s)\n", hub.Version, hub.GitCommit)
		return nil
	}
	a.printf("mcp-hub-ctl %s\n", hub.Version)
	return nil
}

func (a *app) cmdValidate(rest []string) error {
	if len(rest) > 0 {
		return usageErr("validate takes no arguments (use --config <path>)", "")
	}
	cfg, err := hub.LoadConfig(a.opts.configPath)
	if err != nil {
		return &cliError{exit: exitUsage, code: "invalid_config", message: err.Error(), hint: "see config.example.json"}
	}
	var errs, warns []string
	startupTimeoutSec := 15
	if err := hub.ValidateSecretFiles(cfg); err != nil {
		errs = append(errs, err.Error())
	}
	for _, svc := range cfg.Services {
		e, w := checkServiceExecutables(svc)
		errs = append(errs, e...)
		warns = append(warns, w...)
		if svc.EffectiveStartMode() == "eager" {
			startupTimeoutSec = max(startupTimeoutSec, int(svc.EffectiveStartTimeout()/time.Second))
		}
	}
	gwRoutes := 0
	if cfg.Gateway != nil {
		gwRoutes = len(cfg.Gateway.Routes)
	}
	summary := map[string]any{
		"configPath":    a.opts.configPath,
		"controlAddr":   cfg.ControlAddr,
		"services":      len(cfg.Services),
		"proxies":       len(cfg.Proxies),
		"gatewayRoutes": gwRoutes,
		"warnings":      nonNil(warns),
		"errors":        nonNil(errs),
	}
	if len(errs) > 0 {
		if !a.opts.asJSON {
			for _, w := range warns {
				fmt.Fprintf(a.errOut, "%s %s\n", a.errStyle.amberBold("warning:"), w)
			}
			for _, e := range errs {
				fmt.Fprintf(a.errOut, "%s %s\n", a.errStyle.crimsonBold("error:"), e)
			}
			return &cliError{exit: exitUsage, silent: true}
		}
		return &cliError{exit: exitUsage, code: "invalid_config", message: fmt.Sprintf("%d validation error(s)", len(errs)),
			hint: "fix the listed errors", extra: summary}
	}
	if a.opts.asJSON {
		summary["ok"] = true
		summary["startupTimeoutSec"] = startupTimeoutSec
		a.writeJSON(summary)
		return nil
	}
	for _, w := range warns {
		fmt.Fprintf(a.errOut, "%s %s\n", a.errStyle.amberBold("warning:"), w)
	}
	a.printf("%s %s (control=%s, services=%d, proxies=%d, gatewayRoutes=%d, warnings=%d)\n",
		a.style.emeraldBold("OK:"), a.opts.configPath, cfg.ControlAddr, len(cfg.Services), len(cfg.Proxies), gwRoutes, len(warns))
	return nil
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

var scriptExts = map[string]bool{".js": true, ".mjs": true, ".cjs": true, ".py": true, ".ps1": true, ".exe": true, ".cmd": true, ".bat": true, ".jar": true}

func checkServiceExecutables(svc hub.ServiceConfig) (errs, warns []string) {
	cwd := strings.TrimSpace(svc.Cwd)
	if cwd != "" {
		if st, err := os.Stat(cwd); err != nil || !st.IsDir() {
			errs = append(errs, fmt.Sprintf("service %q: cwd %q does not exist or is not a directory", svc.Name, cwd))
		}
	}
	cmd := strings.TrimSpace(svc.Command)
	if filepath.IsAbs(cmd) || strings.ContainsAny(cmd, `/\`) {
		p := cmd
		if !filepath.IsAbs(p) && cwd != "" {
			p = filepath.Join(cwd, p)
		}
		if _, err := os.Stat(p); err != nil {
			errs = append(errs, fmt.Sprintf("service %q: command %q not found on disk", svc.Name, cmd))
		}
	} else if _, err := exec.LookPath(cmd); err != nil {
		warns = append(warns, fmt.Sprintf("service %q: command %q not found on this shell's PATH", svc.Name, cmd))
	}
	for _, arg := range svc.Args {
		if !filepath.IsAbs(arg) || !scriptExts[strings.ToLower(filepath.Ext(arg))] {
			continue
		}
		if _, err := os.Stat(arg); err != nil {
			warns = append(warns, fmt.Sprintf("service %q: argument path %q does not exist", svc.Name, arg))
		}
	}
	return errs, warns
}

// ---------------------------------------------------------------------------
// health / status / stats
// ---------------------------------------------------------------------------

func (a *app) cmdHealth(rest []string) error {
	if len(rest) > 0 {
		return usageErr("health takes no arguments", "")
	}
	client, _, err := a.connect(false)
	if err != nil {
		return err
	}
	ctx, cancel := a.ctx(defaultTimeout)
	defer cancel()
	res, raw, err := client.Healthz(ctx)
	if err != nil {
		return err
	}
	if a.opts.asJSON {
		a.writeRawJSON(raw)
	} else {
		a.printHealth(res)
	}
	if !res.OK {
		return &cliError{exit: exitRuntime, silent: true}
	}
	return nil
}

func (a *app) printHealth(res *hub.HealthzResponse) {
	a.printf("ok: %s\n", a.style.boolBadge(res.OK))
	if len(res.Services) > 0 {
		tbl := newColTable(a.style,
			tableCol{Header: "SERVICE", Align: alignLeft},
			tableCol{Header: "STATE", Align: alignLeft},
			tableCol{Header: "MODE", Align: alignLeft},
			tableCol{Header: "RUNNING", Align: alignLeft},
			tableCol{Header: "HEALTHY", Align: alignLeft},
			tableCol{Header: "PID", Align: alignRight},
			tableCol{Header: "PORT", Align: alignRight},
			tableCol{Header: "RESTARTS", Align: alignRight},
		)
		for _, n := range sortedKeys(res.Services) {
			s := res.Services[n]
			tbl.addRow(n, a.style.stateBadge(s.State), dash(s.Mode), a.style.boolBadge(s.Running), a.style.boolBadge(s.Healthy),
				intOrDash(s.PID), intOrDash(s.Port), strconv.Itoa(s.Restarts))
		}
		tbl.render(a.out)
	} else {
		a.printf("services: (none)\n")
	}
	if len(res.Proxies) > 0 {
		a.printf("\n")
		tbl := newColTable(a.style,
			tableCol{Header: "PROXY", Align: alignLeft},
			tableCol{Header: "UP", Align: alignLeft},
		)
		for _, n := range sortedKeys(res.Proxies) {
			tbl.addRow(n, a.style.boolBadge(res.Proxies[n]))
		}
		tbl.render(a.out)
	}
	if g := res.Gateway; g != nil {
		a.printf("\ngateway: running=%s listen=%s\n", a.style.boolBadge(g.Running), dash(g.ListenAddr))
		if len(g.Routes) > 0 {
			tbl := newColTable(a.style,
				tableCol{Header: "ROUTE", Align: alignLeft},
				tableCol{Header: "UP", Align: alignLeft},
			)
			for _, n := range sortedKeys(g.Routes) {
				tbl.addRow(n, a.style.boolBadge(g.Routes[n]))
			}
			tbl.render(a.out)
		}
	}
}

func (a *app) cmdStatus(rest []string) error {
	if len(rest) > 0 {
		return usageErr("status takes no arguments", "")
	}
	client, _, err := a.connect(true)
	if err != nil {
		return err
	}
	if !a.opts.watch {
		ctx, cancel := a.ctx(defaultTimeout)
		defer cancel()
		res, raw, err := client.Status(ctx)
		if err != nil {
			return err
		}
		if a.opts.asJSON {
			a.writeRawJSON(raw)
			return nil
		}
		a.printStatusTo(a.out, res)
		return nil
	}

	useVT := !a.opts.asJSON && a.style.canUseVTWatch()
	if useVT {
		_, _ = io.WriteString(a.out, "\x1b[?25l")
		defer func() {
			_, _ = io.WriteString(a.out, "\x1b[?25h")
		}()
	}

	ticker := time.NewTicker(a.opts.watchInterval)
	defer ticker.Stop()
	tick := 0
	for {
		// Always fetch status FIRST before touching the terminal so slow fetches never flash a blank screen.
		ctx, cancel := a.ctx(defaultTimeout)
		res, raw, err := client.Status(ctx)
		cancel()

		if err != nil {
			if a.base.Err() != nil {
				return nil
			}
			ce := a.classify(err)
			if ce.exit == exitAuth || ce.exit == exitUsage {
				return err
			}
			if a.opts.asJSON {
				a.writeCompactJSON(map[string]any{"ok": false, "error": map[string]any{"code": ce.code, "message": ce.message}})
			} else {
				var frame bytes.Buffer
				fmt.Fprintf(&frame, "%s\n\n", a.watchBanner())
				fmt.Fprintf(&frame, "%s %s\n", a.style.crimsonBold("error:"), ce.message)
				a.writeWatchFrame(&frame, useVT, tick)
			}
		} else if a.opts.asJSON {
			var v any
			if json.Unmarshal(raw, &v) == nil {
				a.writeCompactJSON(v)
			}
		} else {
			var frame bytes.Buffer
			fmt.Fprintf(&frame, "%s\n\n", a.watchBanner())
			a.printStatusTo(&frame, res)
			a.writeWatchFrame(&frame, useVT, tick)
		}
		tick++

		select {
		case <-a.base.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (a *app) watchBanner() string {
	text := fmt.Sprintf("Updated %s · every %s · Ctrl+C to exit", time.Now().Format("15:04:05"), a.opts.watchInterval)
	return a.style.dim(text)
}

func (a *app) writeWatchFrame(frame *bytes.Buffer, useVT bool, tick int) {
	if useVT {
		_, _ = a.out.Write(formatWatchFrame(frame.Bytes()))
		return
	}
	if tick > 0 {
		_, _ = io.WriteString(a.out, strings.Repeat("-", 60)+"\n")
	}
	_, _ = a.out.Write(frame.Bytes())
}

func (a *app) printStatusTo(w io.Writer, res *hub.StatusResponse) {
	tray := "off"
	if res.TrayEnabled {
		tray = "on"
	}
	okStr := a.style.boolBadge(res.OK)
	fmt.Fprintf(w, "%s  ok=%s  pid=%d  uptime=%s  tray=%s  hub=%.1f MB  tree=Priv %.1f MB / WS %.1f MB\n",
		a.style.bold("mcp-hub v"+res.Version), okStr, res.PID, humanUptime(res.UptimeSec), tray,
		res.HubPrivateMB, res.TreeTotalPrivateMB, res.TreeTotalWorkingSetMB)
	fmt.Fprintf(w, "%s %s\n", a.style.dim("control:"), dash(res.ControlAddr))
	if res.ConfigPath != "" {
		fmt.Fprintf(w, "%s  %s\n", a.style.dim("config:"), res.ConfigPath)
	}
	if res.LogDir != "" {
		fmt.Fprintf(w, "%s    %s\n", a.style.dim("logs:"), res.LogDir)
	}

	fmt.Fprintf(w, "\n%s\n", a.style.section("SERVICES"))
	if len(res.Services) > 0 {
		tbl := newColTable(a.style,
			tableCol{Header: "NAME", Align: alignLeft},
			tableCol{Header: "MODE", Align: alignLeft},
			tableCol{Header: "STATE", Align: alignLeft},
			tableCol{Header: "HEALTH", Align: alignLeft},
			tableCol{Header: "PID(P)", Align: alignRight},
			tableCol{Header: "RAM_MB", Align: alignRight},
			tableCol{Header: "REQS", Align: alignRight},
			tableCol{Header: "ERRS", Align: alignRight},
			tableCol{Header: "SSE", Align: alignRight},
			tableCol{Header: "SESS", Align: alignRight},
			tableCol{Header: "CRASH/WAKE", Align: alignRight},
			tableCol{Header: "LAST_ERR", Align: alignLeft},
		)
		for _, n := range sortedKeys(res.Services) {
			s := res.Services[n]
			pid := "-"
			if s.PID > 0 {
				pid = strconv.Itoa(s.PID)
				if s.ProcessCount > 0 {
					pid += fmt.Sprintf("(%d)", s.ProcessCount)
				}
			}
			ram := "-"
			if s.TreePrivateMB > 0 {
				ram = fmt.Sprintf("%.1f", s.TreePrivateMB)
			}
			reqs, errsStr, sse, sess := "-", "-", "-", "-"
			if s.Metrics != nil {
				reqs = strconv.FormatInt(s.Metrics.TotalRequests, 10)
				errsStr = strconv.FormatInt(s.Metrics.ErrorRequests, 10)
				if s.Metrics.ErrorRequests > 0 {
					errsStr = a.style.crimson(errsStr)
				}
				activeSSE := s.Metrics.ActiveSSE
				if s.ActiveSSE > activeSSE {
					activeSSE = s.ActiveSSE
				}
				sse = strconv.FormatInt(activeSSE, 10)
				sess = strconv.Itoa(s.Metrics.ActiveSessions)
			} else if s.ActiveSSE > 0 {
				sse = strconv.FormatInt(s.ActiveSSE, 10)
			}
			lastErr := s.LastError
			if lastErr == "" && !s.Healthy && s.LastHealthError != "" {
				lastErr = s.LastHealthError
			}
			lastErrCell := truncateDisplay(oneLine(lastErr), 44)
			if lastErrCell != "" {
				lastErrCell = a.style.crimson(lastErrCell)
			}
			tbl.addRow(
				n,
				dash(s.Mode),
				a.style.stateBadge(s.State),
				a.style.healthBadge(healthWord(s)),
				pid,
				ram,
				reqs,
				errsStr,
				sse,
				sess,
				a.style.crashWakeCell(s.CrashRestarts, s.WakeCount),
				lastErrCell,
			)
		}
		tbl.render(w)
	} else {
		fmt.Fprintf(w, "%s\n", a.style.dim("services: (none)"))
	}

	if len(res.Proxies) > 0 {
		fmt.Fprintf(w, "\n%s\n", a.style.section("PROXIES"))
		tbl := newColTable(a.style,
			tableCol{Header: "NAME", Align: alignLeft},
			tableCol{Header: "RUN", Align: alignLeft},
			tableCol{Header: "LISTEN", Align: alignLeft},
			tableCol{Header: "TARGET", Align: alignLeft},
			tableCol{Header: "REQS", Align: alignRight},
			tableCol{Header: "ERRS", Align: alignRight},
			tableCol{Header: "SSE", Align: alignRight},
			tableCol{Header: "SESS", Align: alignRight},
		)
		for _, n := range sortedKeys(res.Proxies) {
			p := res.Proxies[n]
			reqs, errsStr, sse, sess := metricColsFull(a.style, p.Metrics)
			tbl.addRow(n, a.style.boolBadge(p.Running), dash(p.ListenAddr), dash(p.TargetURL), reqs, errsStr, sse, sess)
		}
		tbl.render(w)
	}

	if g := res.Gateway; g != nil {
		fmt.Fprintf(w, "\n%s  running=%s  listen=%s\n", a.style.section("GATEWAY"), a.style.boolBadge(g.Running), dash(g.ListenAddr))
		if len(g.Routes) > 0 {
			tbl := newColTable(a.style,
				tableCol{Header: "SLUG", Align: alignLeft},
				tableCol{Header: "MODE", Align: alignLeft},
				tableCol{Header: "UPSTREAM", Align: alignLeft},
				tableCol{Header: "STATE", Align: alignLeft},
				tableCol{Header: "REQS", Align: alignRight},
				tableCol{Header: "ERRS", Align: alignRight},
				tableCol{Header: "SSE", Align: alignRight},
				tableCol{Header: "SESS", Align: alignRight},
			)
			for _, r := range g.Routes {
				up := r.Service
				if up == "" {
					up = r.TargetURL
				}
				reqs, errsStr, sse, sess := metricColsFull(a.style, r.Metrics)
				tbl.addRow(r.Slug, dash(r.Mode), dash(up), a.style.stateBadge(r.State), reqs, errsStr, sse, sess)
			}
			tbl.render(w)
		}
	}

	var problems []string
	for _, n := range sortedKeys(res.Services) {
		if needsAttention(res.Services[n]) {
			problems = append(problems, n)
		}
	}
	if len(problems) > 0 {
		fmt.Fprintf(w, "\n%s\n", a.style.crimsonBold("RECENT ERRORS"))
		for _, n := range problems {
			s := res.Services[n]
			fmt.Fprintf(w, "[%s] state=%s exit=%d", a.style.bold(n), a.style.stateBadge(s.State), s.LastExitCode)
			if s.DegradedAt != "" {
				fmt.Fprintf(w, " degradedAt=%s", a.style.dim(s.DegradedAt))
			}
			fmt.Fprintf(w, "\n")
			if s.LastError != "" {
				fmt.Fprintf(w, "  lastError: %s\n", a.style.crimson(oneLine(s.LastError)))
			}
			if s.LastHealthError != "" {
				fmt.Fprintf(w, "  lastHealthError: %s\n", a.style.amber(oneLine(s.LastHealthError)))
			}
			tail := s.CrashTail
			label := "crashTail"
			if len(tail) == 0 {
				tail = s.LastStderr
				label = "lastStderr"
			}
			if len(tail) > 10 {
				tail = tail[len(tail)-10:]
			}
			if len(tail) > 0 {
				fmt.Fprintf(w, "  %s:\n", a.style.dim(label))
				for _, ln := range tail {
					fmt.Fprintf(w, "    %s\n", colorizeLogText(a.style, ln))
				}
			}
		}
	}
}

func needsAttention(s hub.ServiceStatus) bool {
	switch s.State {
	case "degraded":
		return true
	case "sleeping", "stopped", "stopping":
		return false
	case "":
		return s.Running && !s.Healthy
	default:
		return !s.Healthy || s.LastHealthError != ""
	}
}

func healthWord(s hub.ServiceStatus) string {
	switch {
	case s.State == "sleeping" || s.State == "stopped" || s.State == "stopping":
		return "-"
	case s.State == "degraded":
		return "DEGRADED"
	case s.Healthy:
		return "ok"
	case s.State == "starting":
		return "starting"
	default:
		return "DOWN"
	}
}

func metricColsFull(st termStyle, m *hub.MCPMetricsSnapshot) (string, string, string, string) {
	if m == nil {
		return "-", "-", "-", "-"
	}
	errs := strconv.FormatInt(m.ErrorRequests, 10)
	if m.ErrorRequests > 0 {
		errs = st.crimson(errs)
	}
	return strconv.FormatInt(m.TotalRequests, 10), errs, strconv.FormatInt(m.ActiveSSE, 10), strconv.Itoa(m.ActiveSessions)
}

func humanUptime(sec int64) string {
	if sec <= 0 {
		return "0s"
	}
	d := sec / 86400
	h := (sec % 86400) / 3600
	m := (sec % 3600) / 60
	s := sec % 60
	switch {
	case d > 0:
		return fmt.Sprintf("%dd%dh%dm", d, h, m)
	case h > 0:
		return fmt.Sprintf("%dh%dm", h, m)
	case m > 0:
		return fmt.Sprintf("%dm%ds", m, s)
	default:
		return fmt.Sprintf("%ds", s)
	}
}

// ---------------------------------------------------------------------------
// stats / metrics
// ---------------------------------------------------------------------------

type toolStatRow struct {
	Service      string  `json:"service"`
	Tool         string  `json:"tool"`
	Calls        int64   `json:"calls"`
	Errors       int64   `json:"errors"`
	AvgMs        float64 `json:"avgMs"`
	LastCalledAt string  `json:"lastCalledAt,omitempty"`
}

func (a *app) cmdStats(rest []string) error {
	if len(rest) > 1 {
		return usageErr("usage: mcp-hub-ctl stats [service]", "")
	}
	filter := ""
	if len(rest) == 1 {
		filter = rest[0]
		if !nameRe.MatchString(filter) {
			return usageErr(fmt.Sprintf("invalid service name %q", filter), "")
		}
	}
	client, _, err := a.connect(true)
	if err != nil {
		return err
	}
	ctx, cancel := a.ctx(defaultTimeout)
	defer cancel()
	st, _, err := client.Status(ctx)
	if err != nil {
		return err
	}

	known := map[string]bool{}
	var rows []toolStatRow
	addMetrics := func(svcName string, m *hub.MCPMetricsSnapshot) {
		known[svcName] = true
		if filter != "" && svcName != filter {
			return
		}
		if m == nil || len(m.Tools) == 0 {
			return
		}
		for toolName, tm := range m.Tools {
			rows = append(rows, toolStatRow{
				Service:      svcName,
				Tool:         toolName,
				Calls:        tm.Calls,
				Errors:       tm.Errors,
				AvgMs:        tm.AvgMs,
				LastCalledAt: tm.LastCalledAt,
			})
		}
	}

	for _, name := range sortedKeys(st.Services) {
		addMetrics(name, st.Services[name].Metrics)
	}
	// Include standalone proxies/gateway routes that are not already represented by a managed service.
	for _, name := range sortedKeys(st.Proxies) {
		if !known[name] {
			p := st.Proxies[name]
			if p.Metrics != nil && len(p.Metrics.Tools) > 0 {
				addMetrics(name, p.Metrics)
			} else {
				known[name] = true
			}
		}
	}
	if st.Gateway != nil {
		for _, r := range st.Gateway.Routes {
			if r.Service == "" && !known[r.Slug] {
				if r.Metrics != nil && len(r.Metrics.Tools) > 0 {
					addMetrics(r.Slug, r.Metrics)
				} else {
					known[r.Slug] = true
				}
			}
		}
	}

	if filter != "" && !known[filter] {
		return &cliError{exit: exitNotFound, code: "service_not_found", message: fmt.Sprintf("no service named %q", filter),
			hint: "run `mcp-hub-ctl status` to list services"}
	}

	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Calls != rows[j].Calls {
			return rows[i].Calls > rows[j].Calls
		}
		if rows[i].Errors != rows[j].Errors {
			return rows[i].Errors > rows[j].Errors
		}
		if rows[i].Service != rows[j].Service {
			return rows[i].Service < rows[j].Service
		}
		return rows[i].Tool < rows[j].Tool
	})

	var totalCalls, totalErrors int64
	for _, r := range rows {
		totalCalls += r.Calls
		totalErrors += r.Errors
	}

	if a.opts.asJSON {
		if rows == nil {
			rows = []toolStatRow{}
		}
		out := map[string]any{
			"ok":          true,
			"totalCalls":  totalCalls,
			"totalErrors": totalErrors,
			"tools":       rows,
		}
		if filter != "" {
			out["service"] = filter
		}
		a.writeJSON(out)
		return nil
	}

	if len(rows) == 0 {
		if filter != "" {
			a.printf("%s\n", a.style.dim(fmt.Sprintf("no tool metrics recorded for %s", filter)))
		} else {
			a.printf("%s\n", a.style.dim("no tool metrics recorded"))
		}
		return nil
	}

	tbl := newColTable(a.style,
		tableCol{Header: "SERVICE", Align: alignLeft},
		tableCol{Header: "TOOL", Align: alignLeft},
		tableCol{Header: "CALLS", Align: alignRight},
		tableCol{Header: "ERRORS", Align: alignRight},
		tableCol{Header: "AVG_MS", Align: alignRight},
		tableCol{Header: "LAST_CALLED", Align: alignLeft},
	)
	for _, r := range rows {
		errCell := strconv.FormatInt(r.Errors, 10)
		if r.Errors > 0 {
			errCell = a.style.crimsonBold(errCell)
		}
		tbl.addRow(
			r.Service,
			a.style.cyan(r.Tool),
			strconv.FormatInt(r.Calls, 10),
			errCell,
			fmt.Sprintf("%.1f", r.AvgMs),
			a.style.dim(dash(r.LastCalledAt)),
		)
	}
	tbl.render(a.out)
	a.printf("\n%s\n", a.style.dim(fmt.Sprintf("total: %d tool(s), %d calls, %d errors", len(rows), totalCalls, totalErrors)))
	return nil
}

// ---------------------------------------------------------------------------
// logs
// ---------------------------------------------------------------------------

func (a *app) matchesGrep(text string) bool {
	if a.opts.grep == "" {
		return true
	}
	return strings.Contains(strings.ToLower(text), strings.ToLower(a.opts.grep))
}

func (a *app) cmdLogs(rest []string) error {
	if len(rest) != 1 {
		return usageErr("usage: mcp-hub-ctl logs <service|hub> [-n 50] [-f] [--offline] [--grep <text>]", "")
	}
	name := rest[0]
	if name != "hub" && !nameRe.MatchString(name) {
		return usageErr(fmt.Sprintf("invalid log name %q", name), "use a service name or `hub`")
	}
	if a.opts.offline {
		if a.opts.follow {
			return usageErr("--offline cannot be combined with -f", "use `logs <name> -f` (SSE stream from the hub, no file lock)")
		}
		return a.logsOffline(name)
	}
	n := a.opts.lines
	if n > maxOnlineLogLines {
		n = maxOnlineLogLines
	}
	client, _, err := a.connect(true)
	if err != nil {
		return err
	}
	if a.opts.follow {
		err := client.FollowLogs(a.base, name, n, func(ln hub.LogLine) error {
			if !a.matchesGrep(ln.Text) {
				return nil
			}
			if a.opts.asJSON {
				a.writeCompactJSON(ln)
			} else {
				a.printLogLine(ln)
			}
			return nil
		})
		if err != nil && a.base.Err() != nil {
			return nil
		}
		return err
	}
	ctx, cancel := a.ctx(defaultTimeout)
	defer cancel()
	res, raw, err := client.Logs(ctx, name, n)
	if err != nil {
		return err
	}
	totalReturned := len(res.Lines)
	filtered := res.Lines
	if a.opts.grep != "" {
		filtered = make([]hub.LogLine, 0, len(res.Lines))
		for _, ln := range res.Lines {
			if a.matchesGrep(ln.Text) {
				filtered = append(filtered, ln)
			}
		}
	}
	if a.opts.asJSON {
		if a.opts.grep == "" {
			a.writeRawJSON(raw)
			return nil
		}
		a.writeJSON(map[string]any{"ok": res.OK, "name": res.Name, "grep": a.opts.grep, "lines": filtered})
		return nil
	}
	for _, ln := range filtered {
		a.printLogLine(ln)
	}
	if a.opts.lines > totalReturned && (a.opts.lines > hub.DefaultRingLogCapacity || totalReturned >= hub.DefaultRingLogCapacity) {
		fmt.Fprintf(a.errOut, "%s\n", a.errStyle.dim(fmt.Sprintf(
			"hint: ring buffer returned %d line(s) (requested %d); pass --offline for full rotating log history",
			totalReturned, a.opts.lines)))
	}
	return nil
}

func (a *app) printLogLine(ln hub.LogLine) {
	_, _ = io.WriteString(a.out, formatLogLine(a.style, ln))
}

func (a *app) logsOffline(name string) error {
	cfg, err := a.config(true)
	if err != nil {
		return err
	}
	file := name + ".log"
	if name == "hub" {
		file = "mcp-hub.log"
	}
	path := filepath.Join(cfg.LogDir, file)
	n := a.opts.lines
	if n > maxOfflineLogLines {
		n = maxOfflineLogLines
	}
	lines, err := tailFile(path, n, 8<<20)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &cliError{exit: exitNotFound, code: "log_not_found", message: fmt.Sprintf("log file %s does not exist", path)}
		}
		return &cliError{exit: exitRuntime, code: "log_read_failed", message: err.Error()}
	}
	red := hub.NewRedactor()
	red.SetSecrets(hub.CollectAllSecrets(cfg))
	out := make([]hub.LogLine, 0, len(lines))
	for _, l := range lines {
		// Always redact BEFORE applying --grep so --grep cannot probe redacted secrets.
		redacted := red.Redact(l)
		if !a.matchesGrep(redacted) {
			continue
		}
		out = append(out, hub.LogLine{Stream: "file", Text: redacted})
	}
	if a.opts.asJSON {
		payload := map[string]any{"ok": true, "name": name, "source": "file", "path": path, "lines": out}
		if a.opts.grep != "" {
			payload["grep"] = a.opts.grep
		}
		a.writeJSON(payload)
		return nil
	}
	for _, l := range out {
		a.printf("%s\n", colorizeLogText(a.style, l.Text))
	}
	return nil
}

// tailFile returns the last n lines of path, reading at most maxBytes from the end in O(N) time.
func tailFile(path string, n int, maxBytes int64) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := st.Size()
	const chunk = 64 << 10
	var chunks [][]byte
	var totalBytes int64
	newlines := 0
	pos := size
	for pos > 0 && totalBytes < maxBytes {
		readLen := int64(chunk)
		if pos < readLen {
			readLen = pos
		}
		if rem := maxBytes - totalBytes; readLen > rem {
			readLen = rem
		}
		pos -= readLen
		tmp := make([]byte, readLen)
		if _, err := f.ReadAt(tmp, pos); err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		chunks = append(chunks, tmp)
		totalBytes += readLen
		newlines += bytes.Count(tmp, []byte{'\n'})
		if newlines > n {
			break
		}
	}
	buf := make([]byte, 0, totalBytes)
	for i := len(chunks) - 1; i >= 0; i-- {
		buf = append(buf, chunks[i]...)
	}
	text := strings.ReplaceAll(string(buf), "\r\n", "\n")
	text = strings.TrimRight(text, "\n")
	if text == "" {
		return []string{}, nil
	}
	all := strings.Split(text, "\n")
	if pos > 0 && len(all) > 0 {
		all = all[1:] // first line may be partial
	}
	if len(all) > n {
		all = all[len(all)-n:]
	}
	return all, nil
}

// ---------------------------------------------------------------------------
// start / stop / sleep / wake / restart / shutdown / reload
// ---------------------------------------------------------------------------

type actionResult struct {
	Name    string             `json:"name"`
	OK      bool               `json:"ok"`
	Service *hub.ServiceStatus `json:"service,omitempty"`
	Error   string             `json:"error,omitempty"`
	exit    int
}

func (a *app) cmdStart(rest []string) error {
	if a.opts.hub {
		if a.opts.all || len(rest) > 0 {
			return usageErr("--hub cannot be combined with a service name or --all", "")
		}
		return a.cmdStartHub()
	}
	if a.opts.wait {
		return usageErr("--wait only applies to `start --hub`", "")
	}
	return a.cmdServiceAction("start", rest)
}

func findHubBinary() (string, error) {
	want := hubBinaryName()
	var dirs []string
	if exe, err := os.Executable(); err == nil && exe != "" {
		dirs = append(dirs, filepath.Dir(exe))
		if resolved, err := filepath.EvalSymlinks(exe); err == nil && resolved != "" {
			rdir := filepath.Dir(resolved)
			if !samePath(rdir, filepath.Dir(exe)) {
				dirs = append(dirs, rdir)
			}
		}
	}
	for _, dir := range dirs {
		cand := filepath.Join(dir, want)
		if st, err := os.Stat(cand); err == nil && !st.IsDir() {
			return cand, nil
		}
	}
	if p, err := exec.LookPath(want); err == nil {
		return p, nil
	}
	return "", &cliError{
		exit:    exitNotFound,
		code:    "hub_binary_not_found",
		message: fmt.Sprintf("could not locate %s next to mcp-hub-ctl or on PATH", want),
		hint:    "ensure mcp-hub is installed alongside mcp-hub-ctl or on PATH",
	}
}

func (a *app) cmdStartHub() error {
	if _, err := a.config(true); err != nil {
		return err
	}
	absCfg, err := filepath.Abs(a.opts.configPath)
	if err != nil {
		absCfg = filepath.Clean(a.opts.configPath)
	}
	addr, _, err := a.resolveControl(false)
	if err != nil {
		return err
	}

	// Idempotent pre-check: if the hub already responds on /healthz, report it cleanly.
	if dialAddr, derr := loopbackDialAddr(addr); derr == nil {
		pctx, pcancel := context.WithTimeout(a.base, 500*time.Millisecond)
		hz, _, herr := a.newHubClient(dialAddr, "").Healthz(pctx)
		pcancel()
		if herr == nil && hz != nil {
			if a.opts.asJSON {
				a.writeJSON(map[string]any{
					"ok":             true,
					"alreadyRunning": true,
					"ready":          true,
					"healthy":        hz.OK,
					"controlAddr":    addr,
					"configPath":     absCfg,
				})
				return nil
			}
			a.printf("mcp-hub is already running (control=%s, healthy=%s)\n", addr, a.style.boolBadge(hz.OK))
			return nil
		}
	}

	hubExe, err := findHubBinaryFn()
	if err != nil {
		return err
	}
	pid, err := launchHubFn(hubExe, absCfg)
	if err != nil {
		return &cliError{exit: exitRuntime, code: "hub_launch_failed", message: fmt.Sprintf("launch %s: %v", hubExe, err)}
	}

	ready := false
	healthy := false
	if a.opts.wait {
		waitTimeout := 15 * time.Second
		if a.opts.timeoutSet {
			waitTimeout = a.opts.timeout
		}
		deadline := time.Now().Add(waitTimeout)
		for time.Now().Before(deadline) {
			if a.base.Err() != nil {
				return a.base.Err()
			}
			dialAddr, derr := loopbackDialAddr(addr)
			if derr == nil {
				rem := time.Until(deadline)
				if rem <= 0 {
					break
				}
				probeTimeout := time.Second
				if rem < probeTimeout {
					probeTimeout = rem
				}
				pctx, pcancel := context.WithTimeout(a.base, probeTimeout)
				hz, _, herr := a.newHubClient(dialAddr, "").Healthz(pctx)
				pcancel()
				if herr == nil && hz != nil {
					ready = true
					healthy = hz.OK
					break
				}
			}
			sleepDur := 200 * time.Millisecond
			if rem := time.Until(deadline); rem < sleepDur {
				sleepDur = rem
			}
			if err := sleepContext(a.base, sleepDur); err != nil {
				return err
			}
		}
		if !ready {
			return &cliError{
				exit:    exitUnreachable,
				code:    "start_timeout",
				message: fmt.Sprintf("mcp-hub (pid %d) did not answer /healthz within %s", pid, waitTimeout),
				hint:    "check the hub log with `mcp-hub-ctl logs hub --offline`",
			}
		}
	}

	if a.opts.asJSON {
		out := map[string]any{
			"ok":          true,
			"started":     true,
			"pid":         pid,
			"binary":      hubExe,
			"configPath":  absCfg,
			"controlAddr": addr,
		}
		if a.opts.wait {
			out["ready"] = ready
			out["healthy"] = healthy
		}
		a.writeJSON(out)
		return nil
	}
	if a.opts.wait {
		a.printf("%s mcp-hub (pid=%d, control=%s, healthy=%s)\n", a.style.emeraldBold("started"), pid, addr, a.style.boolBadge(healthy))
	} else {
		a.printf("%s mcp-hub (pid=%d, control=%s)\n", a.style.emeraldBold("started"), pid, addr)
	}
	return nil
}

func (a *app) cmdSleep(rest []string) error {
	if len(rest) != 1 {
		return usageErr("usage: mcp-hub-ctl sleep <service>", "")
	}
	name := rest[0]
	if !nameRe.MatchString(name) {
		return usageErr(fmt.Sprintf("invalid service name %q", name), "")
	}
	client, _, err := a.connect(true)
	if err != nil {
		return err
	}
	ctx, cancel := a.ctx(defaultTimeout)
	defer cancel()
	res, raw, err := client.SleepService(ctx, name)
	if err != nil {
		return err
	}
	if a.opts.asJSON {
		a.writeRawJSON(raw)
		return nil
	}
	a.printAction("sleep", name, &res.Service)
	return nil
}

func (a *app) cmdWake(rest []string) error {
	if len(rest) != 1 {
		return usageErr("usage: mcp-hub-ctl wake <service>", "")
	}
	name := rest[0]
	if !nameRe.MatchString(name) {
		return usageErr(fmt.Sprintf("invalid service name %q", name), "")
	}
	client, _, err := a.connect(true)
	if err != nil {
		return err
	}
	ctx, cancel := a.ctx(35 * time.Second)
	defer cancel()
	res, raw, err := client.WakeService(ctx, name)
	if err != nil {
		return err
	}
	if a.opts.asJSON {
		a.writeRawJSON(raw)
		return nil
	}
	a.printAction("wake", name, &res.Service)
	return nil
}

func (a *app) cmdServiceAction(action string, rest []string) error {
	usage := fmt.Sprintf("usage: mcp-hub-ctl %s <service|--all>", action)
	if action == "restart" {
		usage += " [--wait]"
	}
	if (a.opts.all && len(rest) > 0) || (!a.opts.all && len(rest) != 1) {
		return usageErr(usage, "")
	}
	client, _, err := a.connect(true)
	if err != nil {
		return err
	}
	timeout := defaultTimeout
	if action == "start" || (action == "restart" && a.opts.wait) {
		timeout = 35 * time.Second
	}
	do := func(name string) (*hub.ServiceStatus, []byte, error) {
		ctx, cancel := a.ctx(timeout)
		defer cancel()
		switch action {
		case "start":
			r, raw, err := client.StartService(ctx, name)
			if err != nil {
				return nil, raw, err
			}
			return &r.Service, raw, nil
		case "stop":
			r, raw, err := client.StopService(ctx, name)
			if err != nil {
				return nil, raw, err
			}
			return &r.Service, raw, nil
		default:
			r, raw, err := client.RestartWithWait(ctx, name, a.opts.wait)
			if err != nil {
				return nil, raw, err
			}
			return &r.Service, raw, nil
		}
	}

	if !a.opts.all {
		name := rest[0]
		if !nameRe.MatchString(name) {
			return usageErr(fmt.Sprintf("invalid service name %q", name), "")
		}
		st, raw, err := do(name)
		if err != nil {
			return err
		}
		if a.opts.asJSON {
			a.writeRawJSON(raw)
			return nil
		}
		a.printAction(action, name, st)
		return nil
	}

	// Prefer the v0.3.0 server-side parallel bulk endpoints when available; fall back on HTTP 501/404.
	if handled, err := a.tryBulkServiceAction(client, action); handled {
		return err
	}

	names, err := a.serviceNames(client)
	if err != nil {
		return err
	}
	results := make([]actionResult, 0, len(names))
	failed := 0
	for _, n := range names {
		st, _, err := do(n)
		r := actionResult{Name: n, OK: err == nil, Service: st}
		if err != nil {
			ce := a.classify(err)
			r.Error = ce.message
			r.exit = ce.exit
			failed++
			if ce.exit == exitAuth || ce.exit == exitUnreachable {
				return err
			}
		}
		results = append(results, r)
		if !a.opts.asJSON {
			if err != nil {
				a.printf("%s %s %s %s\n", action, n, a.style.crimsonBold("failed:"), r.Error)
			} else {
				a.printAction(action, n, st)
			}
		}
	}
	if failed > 0 {
		if a.opts.asJSON {
			return &cliError{exit: exitRuntime, code: "partial_failure", message: fmt.Sprintf("%s failed for %d of %d services", action, failed, len(names)),
				extra: map[string]any{"results": results}}
		}
		return &cliError{exit: exitRuntime, code: "partial_failure", message: fmt.Sprintf("%s failed for %d of %d services", action, failed, len(names))}
	}
	if a.opts.asJSON {
		a.writeJSON(map[string]any{"ok": true, "action": action, "results": results})
	}
	return nil
}

func (a *app) tryBulkServiceAction(client *hub.Client, action string) (bool, error) {
	bulkTimeout := 35 * time.Second
	if action == "start" || (action == "restart" && a.opts.wait) {
		bulkTimeout = 65 * time.Second
	}
	ctx, cancel := a.ctx(bulkTimeout)
	defer cancel()

	var results []actionResult
	switch action {
	case "start":
		res, _, err := client.StartAll(ctx)
		if err != nil {
			if isHTTPStatus(err, http.StatusNotImplemented) || isHTTPStatus(err, http.StatusNotFound) {
				return false, nil
			}
			return true, err
		}
		for _, n := range sortedKeys(res.Services) {
			it := res.Services[n]
			st := it.Service
			results = append(results, actionResult{Name: n, OK: it.OK, Service: &st, Error: it.Error})
		}
	case "stop":
		res, _, err := client.StopAll(ctx)
		if err != nil {
			if isHTTPStatus(err, http.StatusNotImplemented) || isHTTPStatus(err, http.StatusNotFound) {
				return false, nil
			}
			return true, err
		}
		for _, n := range sortedKeys(res.Services) {
			it := res.Services[n]
			st := it.Service
			results = append(results, actionResult{Name: n, OK: it.OK, Service: &st, Error: it.Error})
		}
	case "restart":
		res, _, err := client.RestartAll(ctx, a.opts.wait)
		if err != nil {
			if isHTTPStatus(err, http.StatusNotImplemented) || isHTTPStatus(err, http.StatusNotFound) {
				return false, nil
			}
			return true, err
		}
		for _, n := range sortedKeys(res.Services) {
			it := res.Services[n]
			st := it.Service
			results = append(results, actionResult{Name: n, OK: it.OK, Service: &st, Error: it.Error})
		}
	default:
		return false, nil
	}

	failed := 0
	for _, r := range results {
		if !r.OK {
			failed++
		}
		if !a.opts.asJSON {
			if !r.OK {
				a.printf("%s %s %s %s\n", action, r.Name, a.style.crimsonBold("failed:"), r.Error)
			} else {
				a.printAction(action, r.Name, r.Service)
			}
		}
	}
	if failed > 0 {
		if a.opts.asJSON {
			return true, &cliError{exit: exitRuntime, code: "partial_failure", message: fmt.Sprintf("%s failed for %d of %d services", action, failed, len(results)),
				extra: map[string]any{"results": results}}
		}
		return true, &cliError{exit: exitRuntime, code: "partial_failure", message: fmt.Sprintf("%s failed for %d of %d services", action, failed, len(results))}
	}
	if a.opts.asJSON {
		if results == nil {
			results = []actionResult{}
		}
		a.writeJSON(map[string]any{"ok": true, "action": action, "results": results})
	}
	return true, nil
}

func (a *app) printAction(action, name string, st *hub.ServiceStatus) {
	past := map[string]string{
		"start":   "started",
		"stop":    "stopped",
		"restart": "restarted",
		"sleep":   "slept",
		"wake":    "woke",
	}[action]
	if past == "" {
		past = action
	}
	if st == nil {
		a.printf("%s %s\n", past, name)
		return
	}
	a.printf("%s %s (state=%s, pid=%s, restarts=%d, healthy=%s)\n",
		past, name, a.style.stateBadge(st.State), intOrDash(st.PID), st.Restarts, a.style.boolBadge(st.Healthy))
}

func (a *app) serviceNames(client *hub.Client) ([]string, error) {
	ctx, cancel := a.ctx(defaultTimeout)
	defer cancel()
	st, _, err := client.Status(ctx)
	if err != nil {
		return nil, err
	}
	return sortedKeys(st.Services), nil
}

func (a *app) cmdStop(rest []string) error {
	if a.opts.hub {
		if a.opts.all || len(rest) > 0 {
			return usageErr("--hub cannot be combined with a service name or --all", "")
		}
		return a.cmdShutdown(nil)
	}
	if !a.opts.all && len(rest) == 0 {
		return usageErr("refusing bare `stop`: it no longer shuts down the hub",
			"use `stop <service>` for one service, `stop --all` for every service, or `shutdown` (alias `stop --hub`) to stop the hub supervisor")
	}
	if a.opts.wait {
		return usageErr("--wait only applies to `stop --hub` / `shutdown`", "")
	}
	return a.cmdServiceAction("stop", rest)
}

func (a *app) cmdShutdown(rest []string) error {
	if len(rest) > 0 {
		return usageErr("shutdown takes no arguments", "")
	}
	client, _, err := a.connect(true)
	if err != nil {
		return err
	}
	ctx, cancel := a.ctx(defaultTimeout)
	defer cancel()
	if _, err := client.Stop(ctx); err != nil {
		return err
	}
	stopped := false
	if a.opts.wait {
		waitTimeout := 10 * time.Second
		if a.opts.timeoutSet {
			waitTimeout = a.opts.timeout
		}
		deadline := time.Now().Add(waitTimeout)
		for time.Now().Before(deadline) {
			if a.base.Err() != nil {
				return a.base.Err()
			}
			rem := time.Until(deadline)
			if rem <= 0 {
				break
			}
			probeTimeout := time.Second
			if rem < probeTimeout {
				probeTimeout = rem
			}
			pctx, pcancel := context.WithTimeout(a.base, probeTimeout)
			_, _, herr := client.Healthz(pctx)
			pcancel()
			if herr != nil && isUnreachable(herr) {
				stopped = true
				break
			}
			if a.base.Err() != nil {
				return a.base.Err()
			}
			sleepDur := 250 * time.Millisecond
			if rem := time.Until(deadline); rem < sleepDur {
				sleepDur = rem
			}
			if err := sleepContext(a.base, sleepDur); err != nil {
				return err
			}
		}
		if !stopped {
			return &cliError{exit: exitRuntime, code: "shutdown_timeout",
				message: fmt.Sprintf("hub still answers /healthz %s after shutdown request", waitTimeout),
				hint:    "check the hub log with `mcp-hub-ctl logs hub --offline` or inspect the mcp-hub process"}
		}
	}
	if a.opts.asJSON {
		out := map[string]any{"ok": true, "stopping": true}
		if a.opts.wait {
			out["stopped"] = stopped
		}
		a.writeJSON(out)
		return nil
	}
	if stopped {
		a.printf("mcp-hub stopped\n")
	} else {
		a.printf("mcp-hub shutdown requested\n")
	}
	return nil
}

func (a *app) cmdReload(rest []string) error {
	if len(rest) > 1 {
		return usageErr("usage: mcp-hub-ctl reload [service]", "")
	}
	client, _, err := a.connect(true)
	if err != nil {
		return err
	}
	ctx, cancel := a.ctx(40 * time.Second)
	defer cancel()
	if len(rest) == 1 {
		name := rest[0]
		if !nameRe.MatchString(name) {
			return usageErr(fmt.Sprintf("invalid service name %q", name), "")
		}
		res, raw, err := client.ReloadService(ctx, name)
		if err != nil {
			return err
		}
		if a.opts.asJSON {
			a.writeRawJSON(raw)
			return nil
		}
		a.printf("reloaded %s\n", res.Name)
		if len(res.Result) > 0 {
			var buf bytes.Buffer
			if json.Compact(&buf, res.Result) == nil {
				a.printf("result: %s\n", truncate(buf.String(), 2000))
			}
		}
		return nil
	}
	res, raw, err := client.ReloadHub(ctx)
	if err != nil {
		return err
	}
	if a.opts.asJSON {
		a.writeRawJSON(raw)
		return nil
	}
	a.printReloadSummary(res)
	return nil
}

func (a *app) printReloadSummary(res *hub.ReloadHubResponse) {
	list := func(v []string) string {
		if len(v) == 0 {
			return "-"
		}
		return strings.Join(v, ", ")
	}
	gw := "unchanged"
	if res.ReloadedGateway {
		gw = "reloaded"
	}
	a.printf("hub config reloaded (ok=%s)\n", a.style.boolBadge(res.OK))
	a.printf("  added:     %s\n", list(res.AddedServices))
	a.printf("  updated:   %s\n", list(res.RestartedServices))
	a.printf("  removed:   %s\n", list(res.RemovedServices))
	a.printf("  unchanged: %s\n", list(res.UnchangedServices))
	a.printf("  proxies:   %s\n", list(res.ReloadedProxies))
	a.printf("  gateway:   %s\n", gw)
}

// ---------------------------------------------------------------------------
// ui / open-logs / open-config / init
// ---------------------------------------------------------------------------

func (a *app) cmdUI(rest []string) error {
	if len(rest) > 0 {
		return usageErr("usage: mcp-hub-ctl ui [--no-open]", "")
	}
	client, _, err := a.connect(true)
	if err != nil {
		return err
	}
	ctx, cancel := a.ctx(defaultTimeout)
	defer cancel()
	res, _, err := client.CreateDashboardSession(ctx)
	if err != nil {
		return err
	}
	if strings.TrimSpace(res.URL) == "" {
		return &cliError{exit: exitRuntime, code: "empty_dashboard_url", message: "hub returned an empty dashboard URL"}
	}
	if a.opts.asJSON {
		out := map[string]any{"ok": true, "url": res.URL}
		if res.ExpiresAt != "" {
			out["expiresAt"] = res.ExpiresAt
		}
		a.writeJSON(out)
		return nil
	}
	a.printf("%s\n", res.URL)
	if res.ExpiresAt != "" {
		a.printf("%s\n", a.style.dim("one-time bootstrap link expires at "+res.ExpiresAt))
	}
	if !a.opts.noOpen {
		if u, ok := safeOpenDashboardURL(res.URL); !ok {
			fmt.Fprintf(a.errOut, "%s refused to auto-open non-loopback dashboard URL\n", a.errStyle.amberBold("warning:"))
		} else if err := openExternalFn(u); err != nil {
			fmt.Fprintf(a.errOut, "%s could not open browser automatically: %v\n", a.errStyle.amberBold("warning:"), err)
		}
	}
	return nil
}

func safeOpenDashboardURL(raw string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "http" || u.User != nil || !hub.IsLoopbackHost(u.Hostname()) {
		return "", false
	}
	return u.String(), true
}

func (a *app) cmdOpenLogs(rest []string) error {
	if len(rest) > 1 {
		return usageErr("usage: mcp-hub-ctl open-logs [service|hub]", "")
	}
	cfg, err := a.config(false)
	if err != nil {
		return err
	}
	logDir := hub.ResolveDefaultLogDir()
	if cfg != nil && strings.TrimSpace(cfg.LogDir) != "" {
		logDir = cfg.LogDir
	}
	target := logDir
	if len(rest) == 1 {
		name := rest[0]
		if name != "hub" && !nameRe.MatchString(name) {
			return usageErr(fmt.Sprintf("invalid log name %q", name), "use a service name or `hub`")
		}
		file := name + ".log"
		if name == "hub" {
			file = "mcp-hub.log"
		}
		target = filepath.Join(logDir, file)
	}
	absTarget, err := filepath.Abs(target)
	if err != nil {
		absTarget = filepath.Clean(target)
	}
	st, err := os.Stat(absTarget)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &cliError{exit: exitNotFound, code: "log_not_found", message: fmt.Sprintf("%s does not exist", absTarget)}
		}
		return &cliError{exit: exitRuntime, code: "log_stat_failed", message: err.Error()}
	}
	if a.opts.asJSON {
		a.writeJSON(map[string]any{"ok": true, "path": absTarget, "isDir": st.IsDir()})
		return nil
	}
	if err := openExternalFn(absTarget); err != nil {
		return &cliError{exit: exitRuntime, code: "open_failed", message: fmt.Sprintf("open %s: %v", absTarget, err)}
	}
	a.printf("opened %s\n", absTarget)
	return nil
}

func (a *app) cmdOpenConfig(rest []string) error {
	if len(rest) > 0 {
		return usageErr("usage: mcp-hub-ctl open-config", "")
	}
	absCfg, err := filepath.Abs(a.opts.configPath)
	if err != nil {
		absCfg = filepath.Clean(a.opts.configPath)
	}
	if _, err := os.Stat(absCfg); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &cliError{exit: exitNotFound, code: "config_not_found",
				message: fmt.Sprintf("config file %s does not exist", absCfg),
				hint:    "run `mcp-hub-ctl init` to create a starter config"}
		}
		return &cliError{exit: exitRuntime, code: "config_stat_failed", message: err.Error()}
	}
	if a.opts.asJSON {
		a.writeJSON(map[string]any{"ok": true, "path": absCfg})
		return nil
	}
	if err := openExternalFn(absCfg); err != nil {
		return &cliError{exit: exitRuntime, code: "open_failed", message: fmt.Sprintf("open %s: %v", absCfg, err)}
	}
	a.printf("opened %s\n", absCfg)
	return nil
}

func (a *app) cmdInit(rest []string) error {
	if len(rest) > 0 {
		return usageErr("usage: mcp-hub-ctl init [--dir <dataDir>] [--force]", "")
	}
	var dataDir, cfgPath string
	switch {
	case a.opts.local["dir"]:
		dataDir = filepath.Clean(a.opts.dir)
		cfgPath = filepath.Join(dataDir, "config.json")
	case a.opts.configExplicit:
		cfgPath = filepath.Clean(a.opts.configPath)
		dataDir = filepath.Dir(cfgPath)
	default:
		cfgPath = filepath.Clean(hub.ResolveDefaultConfigPath())
		dataDir = filepath.Dir(cfgPath)
	}
	if abs, err := filepath.Abs(dataDir); err == nil {
		dataDir = abs
	}
	if abs, err := filepath.Abs(cfgPath); err == nil {
		cfgPath = abs
	}
	logsDir := filepath.Join(dataDir, "logs")
	secretsDir := filepath.Join(dataDir, "secrets")
	tokenPath := filepath.Join(secretsDir, "control.token")

	if !a.opts.force {
		if _, err := os.Stat(cfgPath); err == nil {
			return &cliError{exit: exitUsage, code: "file_exists", message: fmt.Sprintf("%s already exists", cfgPath),
				hint: "pass --force to overwrite the existing starter config and control token"}
		}
		if _, err := os.Stat(tokenPath); err == nil {
			return &cliError{exit: exitUsage, code: "file_exists", message: fmt.Sprintf("%s already exists", tokenPath),
				hint: "pass --force to regenerate the control token and config"}
		}
	}

	for _, dir := range []string{dataDir, logsDir, secretsDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return &cliError{exit: exitRuntime, code: "mkdir_failed", message: fmt.Sprintf("create %s: %v", dir, err)}
		}
		if err := hub.ResetFileACL(dir, true); err != nil {
			return &cliError{exit: exitRuntime, code: "acl_failed", message: fmt.Sprintf("harden ACL for %s: %v", dir, err)}
		}
	}

	unlock, err := hub.LockTokenFile(tokenPath)
	if err != nil {
		return &cliError{exit: exitRuntime, code: "token_lock_failed", message: err.Error()}
	}
	defer unlock()

	tok, err := newToken()
	if err != nil {
		return &cliError{exit: exitRuntime, code: "rand_failed", message: err.Error()}
	}
	if err := a.publishToken(tokenPath, tok); err != nil {
		if errors.Is(err, os.ErrExist) {
			return &cliError{exit: exitUsage, code: "file_exists", message: fmt.Sprintf("%s already exists", tokenPath),
				hint: "pass --force to overwrite"}
		}
		return &cliError{exit: exitRuntime, code: "write_failed", message: fmt.Sprintf("write control token: %v", err)}
	}
	fp := fingerprint(tok)

	trayTrue := true
	starter := hub.Config{
		ControlAddr:      hub.DefaultControlAddr,
		ControlTokenFile: filepath.ToSlash(tokenPath),
		LogDir:           filepath.ToSlash(logsDir),
		LogMaxBytes:      hub.DefaultLogMaxBytes,
		Tray:             &trayTrue,
		Gateway: &hub.GatewayConfig{
			ListenAddr: hub.DefaultGatewayAddr,
			Routes:     []hub.GatewayRouteConfig{},
		},
		Services: []hub.ServiceConfig{},
		Proxies:  []hub.ProxyConfig{},
	}
	var cfgBuf bytes.Buffer
	enc := json.NewEncoder(&cfgBuf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(starter); err != nil {
		return &cliError{exit: exitRuntime, code: "encode_failed", message: err.Error()}
	}
	writeCfg := hub.WritePrivateFileAtomicNew
	if a.opts.force {
		writeCfg = hub.WritePrivateFileAtomic
	}
	if err := writeCfg(cfgPath, cfgBuf.Bytes()); err != nil {
		if errors.Is(err, os.ErrExist) {
			return &cliError{exit: exitUsage, code: "file_exists", message: fmt.Sprintf("%s already exists", cfgPath),
				hint: "pass --force to overwrite"}
		}
		return &cliError{exit: exitRuntime, code: "write_failed", message: fmt.Sprintf("write config: %v", err)}
	}
	if _, err := hub.LoadConfig(cfgPath); err != nil {
		return &cliError{exit: exitRuntime, code: "invalid_config", message: "generated config failed validation: " + err.Error()}
	}

	if a.opts.asJSON {
		a.writeJSON(map[string]any{
			"ok":               true,
			"dataDir":          dataDir,
			"configPath":       cfgPath,
			"logDir":           logsDir,
			"secretsDir":       secretsDir,
			"controlTokenFile": tokenPath,
			"tokenFingerprint": fp,
		})
		return nil
	}
	a.printf("%s initialized portable mcp-hub data directory\n", a.style.emeraldBold("OK:"))
	a.printf("  dataDir:      %s\n", dataDir)
	a.printf("  config:       %s\n", cfgPath)
	a.printf("  logs:         %s\n", logsDir)
	a.printf("  secrets:      %s\n", secretsDir)
	a.printf("  controlToken: %s (%s)\n", tokenPath, a.style.dim(fp))
	a.printf("\n%s\n", a.style.dim("next steps:"))
	a.printf("  1. mcp-hub-ctl --config %s validate\n", cfgPath)
	a.printf("  2. mcp-hub-ctl --config %s start --hub --wait\n", cfgPath)
	a.printf("  3. mcp-hub-ctl --config %s ui\n", cfgPath)
	return nil
}

// ---------------------------------------------------------------------------
// catalog / tools / MCP probing
// ---------------------------------------------------------------------------

type catalogRow struct {
	hub.CatalogEntry
	ToolCount  *int     `json:"toolCount,omitempty"`
	ToolNames  []string `json:"toolNames,omitempty"`
	ProbeError string   `json:"probeError,omitempty"`
	ProbeSkip  string   `json:"probeSkipped,omitempty"`
}

func (a *app) cmdCatalog(rest []string) error {
	if len(rest) > 0 {
		return usageErr("catalog takes no arguments", "")
	}
	client, _, err := a.connect(true)
	if err != nil {
		return err
	}
	ctx, cancel := a.ctx(defaultTimeout)
	defer cancel()
	cat, raw, err := client.Catalog(ctx)
	if err != nil {
		return err
	}
	if !a.opts.probeTools {
		if a.opts.asJSON {
			a.writeRawJSON(raw)
			return nil
		}
		a.printCatalog(cat.Entries, nil)
		return nil
	}
	cfg, err := a.config(false)
	if err != nil {
		return err
	}
	rows := make([]catalogRow, 0, len(cat.Entries))
	for _, e := range cat.Entries {
		row := catalogRow{CatalogEntry: e}
		pr := a.probeEntry(cfg, e)
		switch {
		case pr.OK:
			row.ToolCount = &pr.ToolCount
			row.ToolNames = pr.ToolNames
		case pr.Skipped != "":
			row.ProbeSkip = pr.Skipped
		default:
			row.ProbeError = pr.Error
		}
		rows = append(rows, row)
	}
	if a.opts.asJSON {
		a.writeJSON(map[string]any{"schemaVersion": cat.SchemaVersion, "ok": cat.OK, "entries": rows})
		return nil
	}
	a.printCatalog(cat.Entries, rows)
	return nil
}

func (a *app) cmdTools(rest []string) error {
	if len(rest) != 1 {
		return usageErr("usage: mcp-hub-ctl tools <service> [--wake] [--schema]", "")
	}
	name := rest[0]
	if !nameRe.MatchString(name) {
		return usageErr(fmt.Sprintf("invalid service name %q", name), "")
	}
	client, _, err := a.connect(true)
	if err != nil {
		return err
	}
	ctx, cancel := a.ctx(defaultTimeout)
	cat, _, err := client.Catalog(ctx)
	cancel()
	if err != nil {
		return err
	}
	var entry *hub.CatalogEntry
	for i := range cat.Entries {
		if cat.Entries[i].Name == name {
			entry = &cat.Entries[i]
			break
		}
	}
	if entry == nil {
		return &cliError{exit: exitNotFound, code: "service_not_found", message: fmt.Sprintf("no catalog entry named %q", name),
			hint: "run `mcp-hub-ctl catalog` to list entries"}
	}
	if entry.State == "stopped" {
		return &cliError{exit: exitRuntime, code: "service_stopped", message: fmt.Sprintf("service %q is stopped", name),
			hint: fmt.Sprintf("start it first with `mcp-hub-ctl start %s`", name)}
	}
	if entry.State == "sleeping" && !a.opts.wake {
		return &cliError{exit: exitRuntime, code: "service_sleeping", message: fmt.Sprintf("service %q is sleeping", name),
			hint: fmt.Sprintf("re-run with `mcp-hub-ctl tools %s --wake` to wake and inspect tools", name)}
	}
	cfg, err := a.config(false)
	if err != nil {
		return err
	}
	pr := a.probeEntry(cfg, *entry)
	if pr.Skipped != "" {
		code := "probe_skipped"
		hint := ""
		if strings.Contains(pr.Skipped, "sleeping") {
			code = "service_sleeping"
			hint = fmt.Sprintf("re-run with `mcp-hub-ctl tools %s --wake`", name)
		} else if strings.Contains(pr.Skipped, "local config") {
			hint = "pass --config <path> matching the target hub"
		}
		return &cliError{exit: exitRuntime, code: code, message: fmt.Sprintf("cannot inspect %q: %s", name, pr.Skipped), hint: hint}
	}
	if !pr.OK {
		return &cliError{exit: exitRuntime, code: "mcp_probe_failed", message: fmt.Sprintf("inspect %q tools failed: %s", name, pr.Error)}
	}

	toolsOut := make([]mcpToolInfo, 0, len(pr.Tools))
	for _, t := range pr.Tools {
		cp := t
		if !a.opts.schema {
			cp.InputSchema = nil
		}
		toolsOut = append(toolsOut, cp)
	}

	if a.opts.asJSON {
		a.writeJSON(map[string]any{
			"ok":              true,
			"service":         entry.Name,
			"url":             pr.URL,
			"serverName":      pr.ServerName,
			"protocolVersion": pr.ProtocolVersion,
			"stateful":        pr.Stateful,
			"initMs":          pr.InitMs,
			"listMs":          pr.ListMs,
			"toolCount":       len(toolsOut),
			"tools":           toolsOut,
		})
		return nil
	}

	a.printf("%s  %d tool(s)", a.style.bold(entry.Name), len(toolsOut))
	if pr.ServerName != "" {
		a.printf("  server=%s", pr.ServerName)
	}
	if pr.ProtocolVersion != "" {
		a.printf("  proto=%s", pr.ProtocolVersion)
	}
	a.printf("  %s\n", a.style.dim(fmt.Sprintf("(init %dms, list %dms)", pr.InitMs, pr.ListMs)))

	if len(toolsOut) == 0 {
		a.printf("%s\n", a.style.dim("tools: (none)"))
		return nil
	}
	tbl := newColTable(a.style,
		tableCol{Header: "TOOL", Align: alignLeft},
		tableCol{Header: "HINT", Align: alignLeft},
		tableCol{Header: "DESCRIPTION", Align: alignLeft},
	)
	descCap := max(44, a.style.effectiveWidth()-44)
	for _, t := range toolsOut {
		tbl.addRow(a.style.cyan(t.Name), a.style.toolHintBadge(t.ReadOnly), truncateDisplay(oneLine(t.Description), descCap))
	}
	tbl.render(a.out)

	if a.opts.schema {
		for _, t := range toolsOut {
			if len(t.InputSchema) == 0 {
				continue
			}
			var pretty bytes.Buffer
			if json.Indent(&pretty, t.InputSchema, "  ", "  ") == nil {
				a.printf("\n%s %s\n  %s\n", a.style.bold(t.Name), a.style.dim("inputSchema:"), pretty.String())
			}
		}
	}
	return nil
}

func probeSkipReason(e hub.CatalogEntry, wake bool) string {
	switch {
	case e.ClientURL == "":
		return "no client URL"
	case e.State == "stopped":
		return "stopped (skipped)"
	case e.State == "sleeping" && !wake:
		return "sleeping (skipped; pass --wake)"
	}
	return ""
}

func (a *app) probeEntry(cfg *hub.Config, e hub.CatalogEntry) mcpProbeResult {
	if skip := probeSkipReason(e, a.opts.wake); skip != "" {
		return mcpProbeResult{Skipped: skip}
	}
	endpoint, ok := matchProbeEndpoint(configuredProbeEndpoints(cfg, e.Name), e.ClientURL)
	if !ok {
		return mcpProbeResult{Skipped: "endpoint is not in the local config"}
	}
	if endpoint.kind == "direct" && e.Capabilities.LazyWake && !a.opts.wake {
		return mcpProbeResult{Skipped: "direct lazy endpoint has no no-wake support (pass --wake)"}
	}
	token := ""
	if endpoint.tokenFile != "" {
		if e.Auth.Type != "bearer" || !samePath(endpoint.tokenFile, e.Auth.TokenFile) {
			return mcpProbeResult{Skipped: "catalog token file does not match the local config"}
		}
		var err error
		token, err = hub.ReadSecretFile(endpoint.tokenFile)
		if err != nil {
			return mcpProbeResult{Error: scrubDiagnostic("token file unreadable: "+err.Error(), a.controlToken)}
		}
	} else if e.Auth.Type == "bearer" {
		return mcpProbeResult{Skipped: "endpoint has no configured probe token"}
	}
	pctx, cancel := a.ctx(defaultTimeout)
	defer cancel()
	noWake := !a.opts.wake && endpoint.kind != "direct" && token != ""
	return probeMCP(pctx, endpoint.url, token, a.effectiveTimeout(), noWake)
}

func (a *app) printCatalog(entries []hub.CatalogEntry, rows []catalogRow) {
	cols := []tableCol{
		{Header: "NAME", Align: alignLeft},
		{Header: "MODE", Align: alignLeft},
		{Header: "CLIENT_URL", Align: alignLeft},
		{Header: "AUTH", Align: alignLeft},
		{Header: "STATE", Align: alignLeft},
		{Header: "CAPABILITIES", Align: alignLeft},
	}
	if rows != nil {
		cols = append(cols, tableCol{Header: "TOOLS", Align: alignLeft})
	}
	tbl := newColTable(a.style, cols...)
	for i, e := range entries {
		auth := "none"
		if e.Auth.Type == "bearer" {
			auth = "bearer"
			if e.Auth.TokenFile != "" {
				auth += "(" + filepath.Base(e.Auth.TokenFile) + ")"
			}
		}
		cells := []string{e.Name, dash(e.Mode), dash(e.ClientURL), auth, a.style.stateBadge(e.State), capsString(e.Capabilities)}
		if rows != nil {
			r := rows[i]
			switch {
			case r.ToolCount != nil:
				cells = append(cells, a.style.emerald(strconv.Itoa(*r.ToolCount)))
			case r.ProbeSkip != "":
				cells = append(cells, a.style.slate(r.ProbeSkip))
			default:
				cells = append(cells, a.style.crimson("error: "+truncateDisplay(oneLine(r.ProbeError), 60)))
			}
		}
		tbl.addRow(cells...)
	}
	tbl.render(a.out)
	if rows != nil {
		for _, r := range rows {
			if len(r.ToolNames) > 0 {
				a.printf("\n%s tools (%d): %s\n", a.style.bold(r.Name), len(r.ToolNames), strings.Join(r.ToolNames, ", "))
			}
		}
	}
}

func capsString(c hub.CatalogCapabilities) string {
	var parts []string
	if c.Reload {
		parts = append(parts, "reload")
	}
	if c.GracefulShutdown {
		parts = append(parts, "graceful")
	}
	if c.LazyWake {
		parts = append(parts, "lazy")
	}
	if c.StdioBridge {
		parts = append(parts, "stdio")
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, ",")
}

type mcpToolAnnotations struct {
	Title           string `json:"title,omitempty"`
	ReadOnlyHint    *bool  `json:"readOnlyHint,omitempty"`
	DestructiveHint *bool  `json:"destructiveHint,omitempty"`
	IdempotentHint  *bool  `json:"idempotentHint,omitempty"`
	OpenWorldHint   *bool  `json:"openWorldHint,omitempty"`
}

type mcpToolInfo struct {
	Name        string              `json:"name"`
	Title       string              `json:"title,omitempty"`
	Description string              `json:"description,omitempty"`
	ReadOnly    *bool               `json:"readOnlyHint,omitempty"`
	Annotations *mcpToolAnnotations `json:"annotations,omitempty"`
	InputSchema json.RawMessage     `json:"inputSchema,omitempty"`
}

type mcpProbeResult struct {
	URL             string        `json:"url"`
	OK              bool          `json:"ok"`
	ToolCount       int           `json:"toolCount"`
	ToolNames       []string      `json:"toolNames,omitempty"`
	Tools           []mcpToolInfo `json:"tools,omitempty"`
	ServerName      string        `json:"serverName,omitempty"`
	ProtocolVersion string        `json:"protocolVersion,omitempty"`
	Stateful        bool          `json:"stateful"`
	InitMs          int64         `json:"initMs"`
	ListMs          int64         `json:"listMs"`
	Error           string        `json:"error,omitempty"`
	Skipped         string        `json:"skipped,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcEnvelope struct {
	ID     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

type mcpClient struct {
	httpc   *http.Client
	url     string
	token   string
	session string
	proto   string
	noWake  bool
}

func (m *mcpClient) newRequest(ctx context.Context, method string, body []byte) (*http.Request, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, m.url, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json, text/event-stream")
	if m.token != "" {
		req.Header.Set("Authorization", "Bearer "+m.token)
	}
	if m.noWake {
		req.Header.Set("X-McpHub-No-Wake", "1")
	}
	if m.session != "" {
		req.Header.Set("Mcp-Session-Id", m.session)
	}
	if m.proto != "" {
		req.Header.Set("MCP-Protocol-Version", m.proto)
	}
	return req, nil
}

// post sends one JSON-RPC message. wantID == "" marks a notification (no response body expected).
func (m *mcpClient) post(ctx context.Context, payload map[string]any, wantID string) (*rpcEnvelope, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := m.newRequest(ctx, http.MethodPost, body)
	if err != nil {
		return nil, err
	}
	resp, err := m.httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		if m.noWake && resp.StatusCode == http.StatusConflict {
			var status struct{ Error string }
			if json.Unmarshal(snippet, &status) == nil && status.Error == "service_sleeping" {
				return nil, errProbeServiceSleeping
			}
		}
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if payload["method"] == "initialize" {
		m.session = strings.TrimSpace(resp.Header.Get("Mcp-Session-Id"))
	}
	limited := io.LimitReader(resp.Body, 16<<20)
	if wantID == "" {
		_, _ = io.Copy(io.Discard, limited)
		return nil, nil
	}
	if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		return readSSEResponse(limited, wantID)
	}
	raw, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	env, ok := findRPCResponse(raw, wantID)
	if !ok {
		return nil, fmt.Errorf("no JSON-RPC response with id %s", wantID)
	}
	return env, nil
}

func (m *mcpClient) close() error {
	if m.session == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := m.newRequest(ctx, http.MethodDelete, nil)
	if err != nil {
		return err
	}
	resp, err := m.httpc.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound || (resp.StatusCode >= 200 && resp.StatusCode < 300) {
		m.session = ""
		return nil
	}
	return fmt.Errorf("HTTP %d", resp.StatusCode)
}

func findRPCResponse(raw []byte, wantID string) (*rpcEnvelope, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil, false
	}
	var list []rpcEnvelope
	if raw[0] == '[' {
		if json.Unmarshal(raw, &list) != nil {
			return nil, false
		}
	} else {
		var one rpcEnvelope
		if json.Unmarshal(raw, &one) != nil {
			return nil, false
		}
		list = []rpcEnvelope{one}
	}
	for i := range list {
		if strings.Trim(strings.TrimSpace(string(list[i].ID)), `"`) == wantID {
			return &list[i], true
		}
	}
	return nil, false
}

func readSSEResponse(r io.Reader, wantID string) (*rpcEnvelope, error) {
	br := bufio.NewReaderSize(r, 64<<10)
	var data []string
	flush := func() *rpcEnvelope {
		if len(data) == 0 {
			return nil
		}
		joined := strings.Join(data, "\n")
		data = data[:0]
		if env, ok := findRPCResponse([]byte(joined), wantID); ok {
			return env
		}
		return nil
	}
	for {
		line, err := br.ReadString('\n')
		if len(line) > 0 {
			t := strings.TrimRight(line, "\r\n")
			if t == "" {
				if env := flush(); env != nil {
					return env, nil
				}
			} else if strings.HasPrefix(t, "data:") {
				data = append(data, strings.TrimPrefix(strings.TrimPrefix(t, "data:"), " "))
			}
		}
		if err != nil {
			if env := flush(); env != nil {
				return env, nil
			}
			if errors.Is(err, io.EOF) {
				return nil, fmt.Errorf("SSE stream ended without a response for id %s", wantID)
			}
			return nil, err
		}
	}
}

var errProbeServiceSleeping = errors.New("service sleeping")

// probeMCP runs initialize, notifications/initialized and paginated tools/list, then closes its session.
func probeMCP(ctx context.Context, rawURL, token string, timeout time.Duration, noWake bool) (res mcpProbeResult) {
	res.URL = rawURL
	m := &mcpClient{httpc: hub.NewDirectClient(timeout), url: rawURL, token: token, noWake: noWake}
	defer func() {
		if err := m.close(); err != nil {
			detail := scrubDiagnostic("close probe session: "+err.Error(), token)
			if res.Error != "" {
				detail = res.Error + ". " + detail
			}
			res.OK, res.Error, res.Skipped = false, detail, ""
		}
	}()
	fail := func(stage string, err error) mcpProbeResult {
		res.OK = false
		if errors.Is(err, errProbeServiceSleeping) {
			res.Skipped = "sleeping (skipped; pass --wake)"
		} else {
			res.Error = scrubDiagnostic(stage+": "+err.Error(), token)
		}
		return res
	}

	start := time.Now()
	initResp, err := m.post(ctx, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{
			"protocolVersion": probeProtocolVersion,
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "mcp-hub-ctl", "version": hub.Version},
		},
	}, "1")
	if err != nil {
		return fail("initialize", err)
	}
	if initResp.Error != nil {
		return fail("initialize", fmt.Errorf("JSON-RPC error %d: %s", initResp.Error.Code, initResp.Error.Message))
	}
	var initResult struct {
		ProtocolVersion string `json:"protocolVersion"`
		ServerInfo      struct {
			Name string `json:"name"`
		} `json:"serverInfo"`
	}
	_ = json.Unmarshal(initResp.Result, &initResult)
	res.InitMs = time.Since(start).Milliseconds()
	res.ProtocolVersion = initResult.ProtocolVersion
	res.ServerName = scrubDiagnostic(initResult.ServerInfo.Name, token)
	res.Stateful = m.session != ""
	m.proto = initResult.ProtocolVersion
	if m.proto == "" {
		m.proto = probeProtocolVersion
	}

	if _, err := m.post(ctx, map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"}, ""); err != nil {
		return fail("notifications/initialized", err)
	}

	listStart := time.Now()
	var tools []mcpToolInfo
	cursor := ""
	for page := 0; page < 50; page++ {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		id := page + 2
		r, err := m.post(ctx, map[string]any{"jsonrpc": "2.0", "id": id, "method": "tools/list", "params": params}, strconv.Itoa(id))
		if err != nil {
			return fail("tools/list", err)
		}
		if r.Error != nil {
			return fail("tools/list", fmt.Errorf("JSON-RPC error %d: %s", r.Error.Code, r.Error.Message))
		}
		var lr struct {
			Tools []struct {
				Name        string              `json:"name"`
				Title       string              `json:"title"`
				Description string              `json:"description"`
				Annotations *mcpToolAnnotations `json:"annotations"`
				InputSchema json.RawMessage     `json:"inputSchema"`
			} `json:"tools"`
			NextCursor string `json:"nextCursor"`
		}
		if err := json.Unmarshal(r.Result, &lr); err != nil {
			return fail("tools/list", fmt.Errorf("decode result: %v", err))
		}
		for _, t := range lr.Tools {
			info := mcpToolInfo{
				Name:        scrubDiagnostic(t.Name, token),
				Title:       scrubDiagnostic(t.Title, token),
				Description: scrubDiagnostic(t.Description, token),
			}
			if t.Annotations != nil {
				ann := *t.Annotations
				ann.Title = scrubDiagnostic(ann.Title, token)
				info.Annotations = &ann
				info.ReadOnly = ann.ReadOnlyHint
			}
			if len(bytes.TrimSpace(t.InputSchema)) > 0 {
				scrubbed := scrubDiagnostic(string(bytes.TrimSpace(t.InputSchema)), token)
				if json.Valid([]byte(scrubbed)) {
					info.InputSchema = json.RawMessage(scrubbed)
				}
			}
			tools = append(tools, info)
		}
		if lr.NextCursor == "" {
			break
		}
		cursor = lr.NextCursor
	}
	sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })
	names := make([]string, len(tools))
	for i, t := range tools {
		names[i] = t.Name
	}
	res.ListMs = time.Since(listStart).Milliseconds()
	res.Tools = tools
	res.ToolNames = names
	res.ToolCount = len(tools)
	res.OK = true
	return res
}

// ---------------------------------------------------------------------------
// probe / doctor
// ---------------------------------------------------------------------------

type probeCheck struct {
	Check     string `json:"check"`
	Target    string `json:"target"`
	Result    string `json:"result"` // pass | fail | warn | skip
	Detail    string `json:"detail,omitempty"`
	LatencyMs int64  `json:"latencyMs,omitempty"`
}

func (a *app) cmdProbe(rest []string) error {
	if len(rest) > 1 {
		return usageErr("usage: mcp-hub-ctl probe [service] [--mcp] [--wake]", "")
	}
	filter := ""
	if len(rest) == 1 {
		filter = rest[0]
		if !nameRe.MatchString(filter) {
			return usageErr(fmt.Sprintf("invalid service name %q", filter), "")
		}
	}
	client, addr, err := a.connect(true)
	if err != nil {
		return err
	}
	cfg, err := a.config(false)
	if err != nil {
		return err
	}
	httpc := hub.NewDirectClient(a.effectiveTimeout())
	plainTimeout := a.plainStatusTimeout()
	var checks []probeCheck
	add := func(c probeCheck) {
		c.Target = scrubDiagnostic(c.Target, a.controlToken)
		c.Detail = scrubDiagnostic(c.Detail, a.controlToken)
		checks = append(checks, c)
	}
	withChecks := func(err error) error {
		ce := a.classify(err)
		cp := *ce
		cp.extra = map[string]any{"checks": checks}
		if !a.opts.asJSON && len(checks) > 0 {
			a.printChecks(checks)
		}
		return &cp
	}

	// 1. Hub /healthz
	ctx, cancel := a.ctx(defaultTimeout)
	t0 := time.Now()
	hz, _, err := client.Healthz(ctx)
	cancel()
	if err != nil {
		add(probeCheck{Check: "hub-healthz", Target: addr, Result: "fail", Detail: a.classify(err).message})
		return withChecks(err)
	}
	hc := probeCheck{Check: "hub-healthz", Target: addr, Result: "pass", LatencyMs: time.Since(t0).Milliseconds()}
	if !hz.OK {
		hc.Result = "fail"
		var down []string
		for _, n := range sortedKeys(hz.Services) {
			s := hz.Services[n]
			if !s.Healthy && s.State != "sleeping" && s.State != "stopped" {
				down = append(down, n)
			}
		}
		hc.Detail = "ok=false; unhealthy: " + dash(strings.Join(down, ", "))
	}
	add(hc)

	// 2. Status + catalog (authenticated)
	ctx, cancel = a.ctx(defaultTimeout)
	st, _, err := client.Status(ctx)
	cancel()
	if err != nil {
		add(probeCheck{Check: "control-auth", Target: addr + "/status", Result: "fail", Detail: a.classify(err).message})
		return withChecks(err)
	}
	add(probeCheck{Check: "control-auth", Target: addr + "/status", Result: "pass", Detail: "Bearer accepted"})
	ctx, cancel = a.ctx(defaultTimeout)
	cat, _, err := client.Catalog(ctx)
	cancel()
	if err != nil {
		add(probeCheck{Check: "catalog", Target: addr + "/catalog", Result: "fail", Detail: a.classify(err).message})
		return withChecks(err)
	}

	if cfg == nil {
		add(probeCheck{
			Check:  "local-config",
			Target: a.opts.configPath,
			Result: "warn",
			Detail: "no local config loaded; proxy/gateway listener guards and endpoint probes skipped (pass --config <path>)",
		})
	}

	// Unauthenticated control route must be rejected.
	if code, err := plainStatus(a.base, httpc, plainTimeout, http.MethodGet, "http://"+addr+"/status", nil); err != nil {
		add(probeCheck{Check: "control-401", Target: addr + "/status", Result: "fail", Detail: err.Error()})
	} else if code == http.StatusUnauthorized {
		add(probeCheck{Check: "control-401", Target: addr + "/status", Result: "pass", Detail: "HTTP 401 without token"})
	} else {
		add(probeCheck{Check: "control-401", Target: addr + "/status", Result: "fail", Detail: fmt.Sprintf("HTTP %d without token (expected 401)", code)})
	}

	// 3. Host / Origin guard on every listener.
	type listener struct{ kind, addr string }
	listeners := []listener{{"control", addr}}
	if cfg != nil {
		for _, p := range cfg.Proxies {
			reported := st.Proxies[p.Name]
			if addr, matched := probeListenAddr(p.ListenAddr, reported.ListenAddr); reported.Running && matched {
				listeners = append(listeners, listener{"proxy:" + p.Name, addr})
			}
		}
		if cfg.Gateway != nil && st.Gateway != nil && st.Gateway.Running {
			if addr, matched := probeListenAddr(cfg.Gateway.ListenAddr, st.Gateway.ListenAddr); matched {
				listeners = append(listeners, listener{"gateway", addr})
			}
		}
	}
	for _, l := range listeners {
		u := "http://" + l.addr + "/healthz"
		for _, g := range []struct {
			name   string
			mutate func(*http.Request)
		}{
			{"host-guard", func(r *http.Request) { r.Host = "evil.example.com" }},
			{"origin-guard", func(r *http.Request) { r.Header.Set("Origin", "http://evil.example.com") }},
		} {
			code, err := plainStatus(a.base, httpc, plainTimeout, http.MethodGet, u, g.mutate)
			c := probeCheck{Check: g.name, Target: l.kind + " " + l.addr}
			switch {
			case err != nil:
				c.Result, c.Detail = "fail", err.Error()
			case code == http.StatusForbidden:
				c.Result, c.Detail = "pass", "HTTP 403"
			default:
				c.Result, c.Detail = "fail", fmt.Sprintf("HTTP %d (expected 403)", code)
			}
			add(c)
		}
	}

	// 4. 401 gate on protected MCP endpoints.
	entries := cat.Entries
	if filter != "" {
		var sel []hub.CatalogEntry
		for _, e := range entries {
			if e.Name == filter {
				sel = append(sel, e)
			}
		}
		if len(sel) == 0 {
			return withChecks(&cliError{exit: exitNotFound, code: "service_not_found", message: fmt.Sprintf("no catalog entry named %q", filter),
				hint: "run `mcp-hub-ctl catalog` to list entries"})
		}
		entries = sel
	}
	seenURL := map[string]bool{}
	endpointChecksRan := 0
	for _, e := range entries {
		urls := e.ClientURLs
		if len(urls) == 0 && e.ClientURL != "" {
			urls = []string{e.ClientURL}
		}
		endpoints := configuredProbeEndpoints(cfg, e.Name)
		for _, raw := range urls {
			c := probeCheck{Check: "auth-401", Target: e.Name + " " + raw}
			endpoint, matched := matchProbeEndpoint(endpoints, raw)
			if !matched {
				c.Result, c.Detail = "skip", "endpoint is not in the local config"
				add(c)
				continue
			}
			if seenURL[endpoint.url] {
				continue
			}
			seenURL[endpoint.url] = true
			kind := endpoint.kind
			if kind == "direct" {
				switch {
				case e.State == "sleeping" || e.State == "stopped":
					c.Detail = "direct upstream " + e.State
				case e.Capabilities.LazyWake && !a.opts.wake:
					c.Detail = "direct lazy endpoint has no no-wake support (pass --wake)"
				}
				if c.Detail != "" {
					c.Result = "skip"
					add(c)
					continue
				}
			}
			endpointChecksRan++
			body := []byte(`{"jsonrpc":"2.0","id":"mcp-hub-ctl-probe","method":"ping"}`)
			code, err := plainStatus(a.base, httpc, plainTimeout, http.MethodPost, endpoint.url, func(r *http.Request) {
				r.Body = io.NopCloser(bytes.NewReader(body))
				r.ContentLength = int64(len(body))
				r.Header.Set("Content-Type", "application/json")
				r.Header.Set("Accept", "application/json, text/event-stream")
			})
			switch {
			case err != nil:
				c.Result, c.Detail = "fail", err.Error()
			case code == http.StatusUnauthorized:
				c.Result, c.Detail = "pass", kind+": HTTP 401 without token"
			case endpoint.tokenFile != "":
				c.Result, c.Detail = "fail", fmt.Sprintf("%s: HTTP %d without token (expected 401)", kind, code)
			default:
				c.Result, c.Detail = "warn", fmt.Sprintf("direct loopback upstream answers HTTP %d without hub auth", code)
			}
			add(c)
		}
	}

	// 5. Authenticated MCP handshake.
	mcpChecksRan := 0
	if a.opts.mcp {
		for _, e := range entries {
			c := probeCheck{Check: "mcp-tools", Target: e.Name + " " + dash(e.ClientURL)}
			t := time.Now()
			pr := a.probeEntry(cfg, e)
			c.LatencyMs = time.Since(t).Milliseconds()
			switch {
			case pr.OK:
				mcpChecksRan++
				c.Result = "pass"
				c.Detail = fmt.Sprintf("%d tools (init %dms, list %dms, stateful=%v)", pr.ToolCount, pr.InitMs, pr.ListMs, pr.Stateful)
			case pr.Skipped != "":
				c.Result, c.Detail = "skip", pr.Skipped
			default:
				mcpChecksRan++
				c.Result, c.Detail = "fail", pr.Error
			}
			add(c)
		}
	}

	// Avoid a misleading clean pass when --addr was used without matching local config and zero endpoint checks ran.
	if cfg != nil && a.opts.addrSet && len(entries) > 0 && endpointChecksRan == 0 && mcpChecksRan == 0 {
		add(probeCheck{
			Check:  "local-config",
			Target: a.opts.configPath,
			Result: "warn",
			Detail: "zero endpoint checks ran against --addr (pass --config matching the target hub)",
		})
	}

	summary := map[string]int{"pass": 0, "fail": 0, "warn": 0, "skip": 0}
	for _, c := range checks {
		summary[c.Result]++
	}
	if !a.opts.asJSON {
		a.printChecks(checks)
		a.printf("\n%d passed, %d failed, %d warnings, %d skipped\n", summary["pass"], summary["fail"], summary["warn"], summary["skip"])
	}
	if summary["fail"] > 0 {
		if a.opts.asJSON {
			return &cliError{exit: exitRuntime, code: "probe_failed", message: fmt.Sprintf("%d check(s) failed", summary["fail"]),
				extra: map[string]any{"checks": checks, "summary": summary}}
		}
		return &cliError{exit: exitRuntime, silent: true}
	}
	if a.opts.asJSON {
		a.writeJSON(map[string]any{"ok": true, "checks": checks, "summary": summary})
	}
	return nil
}

func (a *app) printChecks(checks []probeCheck) {
	tbl := newColTable(a.style,
		tableCol{Header: "CHECK", Align: alignLeft},
		tableCol{Header: "TARGET", Align: alignLeft},
		tableCol{Header: "RESULT", Align: alignLeft},
		tableCol{Header: "DETAIL", Align: alignLeft},
	)
	targetCap := 40
	detailCap := max(44, a.style.effectiveWidth()-68)
	for _, c := range checks {
		detail := oneLine(c.Detail)
		if c.LatencyMs > 0 && !strings.Contains(detail, "ms") {
			detail = strings.TrimSpace(fmt.Sprintf("%s (%dms)", detail, c.LatencyMs))
		}
		tbl.addRow(c.Check, compactProbeTarget(c.Target, targetCap), a.style.resultBadge(c.Result), truncateDisplay(detail, detailCap))
	}
	tbl.render(a.out)
}

// plainStatus issues an unauthenticated request and returns only the status code.
func plainStatus(base context.Context, httpc *http.Client, timeout time.Duration, method, rawURL string, mutate func(*http.Request)) (int, error) {
	ctx, cancel := context.WithTimeout(base, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, rawURL, nil)
	if err != nil {
		return 0, err
	}
	if mutate != nil {
		mutate(req)
	}
	resp, err := httpc.Do(req)
	if err != nil {
		return 0, err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
	return resp.StatusCode, nil
}

// ---------------------------------------------------------------------------
// snippet
// ---------------------------------------------------------------------------

var snippetClients = []string{"agy", "claude", "codex", "cursor", "vscode", "windsurf", "gemini", "opencode", "devin"}

func (a *app) cmdSnippet(rest []string) error {
	if len(rest) != 1 {
		return usageErr("usage: mcp-hub-ctl snippet <service> [--client agy|claude|codex|cursor|vscode|windsurf|gemini|opencode|devin|all]", "")
	}
	name := rest[0]
	clients := snippetClients
	if a.opts.client != "all" {
		ok := false
		for _, c := range snippetClients {
			if c == a.opts.client {
				ok = true
			}
		}
		if !ok {
			return usageErr(fmt.Sprintf("unknown --client %q", a.opts.client), "use agy, claude, codex, cursor, vscode, windsurf, gemini, opencode, devin or all")
		}
		clients = []string{a.opts.client}
	}

	entry, source, err := a.lookupEntry(name)
	if err != nil {
		return err
	}
	if entry.ClientURL == "" {
		return &cliError{exit: exitNotFound, code: "no_client_url", message: fmt.Sprintf("%q has no client URL (no proxy, gateway route or HTTP port)", name)}
	}
	snips := map[string]string{}
	for _, c := range clients {
		snips[c] = buildSnippet(c, entry)
	}
	if a.opts.asJSON {
		auth := map[string]any{"type": entry.Auth.Type}
		if entry.Auth.TokenFile != "" {
			auth["tokenFile"] = filepath.ToSlash(entry.Auth.TokenFile)
		}
		a.writeJSON(map[string]any{"ok": true, "name": entry.Name, "url": entry.ClientURL, "source": source, "auth": auth, "snippets": snips})
		return nil
	}
	for i, c := range clients {
		if i > 0 {
			a.printf("\n")
		}
		a.printf("%s\n%s\n", a.style.bold("# "+c), snips[c])
	}
	return nil
}

// lookupEntry returns the live catalog entry, falling back to config inference when the hub is unreachable.
func (a *app) lookupEntry(name string) (hub.CatalogEntry, string, error) {
	client, _, err := a.connect(true)
	if err != nil {
		return hub.CatalogEntry{}, "", err
	}
	ctx, cancel := a.ctx(defaultTimeout)
	defer cancel()
	cat, _, err := client.Catalog(ctx)
	if err == nil {
		for _, e := range cat.Entries {
			if e.Name == name {
				return e, "catalog", nil
			}
		}
		return hub.CatalogEntry{}, "", &cliError{exit: exitNotFound, code: "service_not_found", message: fmt.Sprintf("no catalog entry named %q", name),
			hint: "run `mcp-hub-ctl catalog` to list entries"}
	}
	if !isUnreachable(err) {
		return hub.CatalogEntry{}, "", err
	}
	cfg, cerr := a.config(false)
	if cerr != nil || cfg == nil {
		return hub.CatalogEntry{}, "", err
	}
	e, ok := entryFromConfig(cfg, name)
	if !ok {
		return hub.CatalogEntry{}, "", &cliError{exit: exitNotFound, code: "service_not_found", message: fmt.Sprintf("hub unreachable and %q not found in config", name)}
	}
	return e, "config", nil
}

func firstTokenFile(primary string, tokens []hub.RouteTokenConfig) string {
	if strings.TrimSpace(primary) != "" {
		return strings.TrimSpace(primary)
	}
	for _, t := range tokens {
		if strings.TrimSpace(t.TokenFile) != "" {
			return strings.TrimSpace(t.TokenFile)
		}
	}
	return ""
}

func entryFromConfig(cfg *hub.Config, name string) (hub.CatalogEntry, bool) {
	var svc *hub.ServiceConfig
	for i := range cfg.Services {
		if cfg.Services[i].Name == name {
			svc = &cfg.Services[i]
		}
	}
	e := hub.CatalogEntry{Name: name, Mode: "http", Auth: hub.CatalogAuthInfo{Type: "none"}}
	if svc != nil {
		e.Mode = svc.EffectiveMode()
	}
	setAuth := func(tok string) {
		if tok != "" && e.Auth.Type == "none" {
			e.Auth = hub.CatalogAuthInfo{Type: "bearer", TokenFile: tok, Header: "Authorization"}
		}
	}
	found := svc != nil
	if cfg.Gateway != nil && strings.TrimSpace(cfg.Gateway.ListenAddr) != "" {
		for _, r := range cfg.Gateway.Routes {
			if r.Service != name && !(r.Service == "" && r.Slug == name) {
				continue
			}
			found = true
			u := fmt.Sprintf("http://%s/s/%s", cfg.Gateway.ListenAddr, r.Slug)
			if e.Mode != "stdio-bridge" {
				cp := strings.TrimSpace(r.ClientPath)
				if cp == "" && svc != nil {
					cp = svc.EffectiveClientPath()
				}
				if cp == "" {
					cp = "/mcp"
				}
				u += cp
			}
			e.ClientURLs = append(e.ClientURLs, u)
			if e.ViaGateway == "" {
				e.ViaGateway = r.Slug
			}
			setAuth(firstTokenFile(r.TokenFile, r.Tokens))
		}
	}
	for _, p := range cfg.Proxies {
		if p.UpstreamService != name && !(p.UpstreamService == "" && p.Name == name) {
			continue
		}
		found = true
		cp := strings.TrimSpace(p.ClientPath)
		if cp == "" && svc != nil {
			cp = svc.EffectiveClientPath()
		}
		if cp == "" {
			cp = "/mcp"
		}
		e.ClientURLs = append(e.ClientURLs, "http://"+p.ListenAddr+cp)
		if e.ViaProxy == "" {
			e.ViaProxy = p.Name
		}
		setAuth(firstTokenFile(p.TokenFile, p.Tokens))
	}
	if svc != nil && e.Mode == "http" {
		if base := hub.InferServiceBaseURL(*svc); base != "" {
			e.ClientURLs = append(e.ClientURLs, strings.TrimRight(base, "/")+svc.EffectiveClientPath())
		}
		setAuth(inferServiceTokenFile(*svc))
	}
	if len(e.ClientURLs) > 0 {
		e.ClientURL = e.ClientURLs[0]
	}
	return e, found
}

func tokenPlaceholder(e hub.CatalogEntry) string {
	return "Bearer <TOKEN_FROM: " + filepath.ToSlash(e.Auth.TokenFile) + ">"
}

func buildSnippet(client string, e hub.CatalogEntry) string {
	hasAuth := e.Auth.Type == "bearer" && e.Auth.TokenFile != ""
	headers := map[string]string{}
	if hasAuth {
		headers["Authorization"] = tokenPlaceholder(e)
	}
	server := map[string]any{}
	var root map[string]any
	switch client {
	case "codex":
		key := e.Name
		if !tomlBareKey.MatchString(key) {
			key = strconv.Quote(key)
		}
		var b strings.Builder
		fmt.Fprintf(&b, "[mcp_servers.%s]\n", key)
		fmt.Fprintf(&b, "url = %s\n", strconv.Quote(e.ClientURL))
		if hasAuth {
			fmt.Fprintf(&b, "http_headers = { Authorization = %s }\n", strconv.Quote(tokenPlaceholder(e)))
		}
		return strings.TrimRight(b.String(), "\n")
	case "agy", "windsurf":
		server["serverUrl"] = e.ClientURL
		root = map[string]any{"mcpServers": map[string]any{e.Name: server}}
	case "claude":
		server["type"] = "http"
		server["url"] = e.ClientURL
		root = map[string]any{"mcpServers": map[string]any{e.Name: server}}
	case "vscode":
		server["type"] = "http"
		server["url"] = e.ClientURL
		root = map[string]any{"servers": map[string]any{e.Name: server}}
	case "gemini":
		server["httpUrl"] = e.ClientURL
		root = map[string]any{"mcpServers": map[string]any{e.Name: server}}
	case "opencode":
		server["type"] = "remote"
		server["url"] = e.ClientURL
		server["oauth"] = false
		root = map[string]any{"mcp": map[string]any{e.Name: server}}
	default: // cursor, devin
		server["url"] = e.ClientURL
		root = map[string]any{"mcpServers": map[string]any{e.Name: server}}
	}
	if hasAuth {
		server["headers"] = headers
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	_ = enc.Encode(root)
	return strings.TrimRight(buf.String(), "\n")
}

// ---------------------------------------------------------------------------
// audit
// ---------------------------------------------------------------------------

type auditFinding struct {
	Check  string `json:"check"`
	Target string `json:"target"`
	Status string `json:"status"` // ok | violation | warn | fixed | skip
	Detail string `json:"detail,omitempty"`
}

type secretRef struct {
	label      string
	path       string
	owner      string
	bearer     bool // hub-issued client Bearer token (weak-secret check applies)
	policyFile bool // multi-token/structured policy file (ACL audited, skip single-line ReadSecretFile)
}

type aclTarget struct {
	label    string
	path     string
	isDir    bool
	optional bool // missing is a warning, not a violation
}

func isStructuredPolicyFile(path string) bool {
	return strings.HasSuffix(strings.ToLower(strings.TrimSpace(path)), ".json")
}

func collectSecretRefs(cfg *hub.Config) []secretRef {
	var refs []secretRef
	add := func(label, path, owner string, bearer, policyFile bool) {
		trimmed := strings.TrimSpace(path)
		if trimmed == "" {
			return
		}
		if !bearer && isStructuredPolicyFile(trimmed) {
			policyFile = true
		}
		refs = append(refs, secretRef{
			label:      label,
			path:       trimmed,
			owner:      owner,
			bearer:     bearer,
			policyFile: policyFile,
		})
	}
	add("controlTokenFile", cfg.ControlTokenFile, "control", true, false)
	for _, s := range cfg.Services {
		for _, k := range sortedKeys(s.EnvFiles) {
			add(fmt.Sprintf("service %s envFiles[%s]", s.Name, k), s.EnvFiles[k], s.Name, false, false)
		}
		add(fmt.Sprintf("service %s shutdownTokenFile", s.Name), s.ShutdownTokenFile, s.Name, false, false)
		add(fmt.Sprintf("service %s reloadTokenFile", s.Name), s.ReloadTokenFile, s.Name, false, false)
		for _, k := range sortedKeys(s.Env) {
			ku := strings.ToUpper(k)
			if strings.HasSuffix(ku, "_FILES") {
				isMulti := strings.HasSuffix(ku, "_TOKENS_FILES")
				for _, p := range strings.FieldsFunc(s.Env[k], func(r rune) bool { return r == ';' || r == ',' || r == '\n' }) {
					add(fmt.Sprintf("service %s env[%s]", s.Name, k), p, s.Name, false, isMulti)
				}
			} else if strings.HasSuffix(ku, "_FILE") {
				isMulti := strings.HasSuffix(ku, "_TOKENS_FILE")
				add(fmt.Sprintf("service %s env[%s]", s.Name, k), s.Env[k], s.Name, false, isMulti)
			}
		}
	}
	for _, p := range cfg.Proxies {
		owner := p.UpstreamService
		if owner == "" {
			owner = "proxy:" + p.Name
		}
		add(fmt.Sprintf("proxy %s tokenFile", p.Name), p.TokenFile, owner, true, false)
		for i, t := range p.Tokens {
			add(fmt.Sprintf("proxy %s tokens[%d]", p.Name, i), t.TokenFile, owner, true, false)
		}
		add(fmt.Sprintf("proxy %s upstreamTokenFile", p.Name), p.UpstreamTokenFile, owner, false, false)
	}
	if cfg.Gateway != nil {
		for _, r := range cfg.Gateway.Routes {
			owner := r.Service
			if owner == "" {
				owner = "route:" + r.Slug
			}
			add(fmt.Sprintf("gateway %s tokenFile", r.Slug), r.TokenFile, owner, true, false)
			for i, t := range r.Tokens {
				add(fmt.Sprintf("gateway %s tokens[%d]", r.Slug, i), t.TokenFile, owner, true, false)
			}
			add(fmt.Sprintf("gateway %s upstreamTokenFile", r.Slug), r.UpstreamTokenFile, owner, false, false)
		}
	}
	return refs
}

func pathKey(p string) string {
	c := filepath.Clean(p)
	if runtime.GOOS == "windows" {
		c = strings.ToLower(filepath.FromSlash(c))
	}
	return c
}

func samePath(a, b string) bool {
	if strings.TrimSpace(a) == "" || strings.TrimSpace(b) == "" {
		return false
	}
	ap, aErr := filepath.Abs(strings.TrimSpace(a))
	bp, bErr := filepath.Abs(strings.TrimSpace(b))
	return aErr == nil && bErr == nil && pathKey(ap) == pathKey(bp)
}

func canAutoFixDirACL(dirPath, configDataDir string) bool {
	defaultDataDir := filepath.Dir(hub.ResolveDefaultConfigPath())
	defaultLogDir := hub.ResolveDefaultLogDir()
	defaultSecretsDir := filepath.Join(defaultDataDir, "secrets")
	if samePath(dirPath, defaultDataDir) || samePath(dirPath, defaultLogDir) || samePath(dirPath, defaultSecretsDir) {
		return true
	}
	// Also permit fixing dedicated secrets/ or logs/ subdirectories inside the active config's dataDir.
	if configDataDir != "" && samePath(filepath.Dir(dirPath), configDataDir) {
		base := strings.ToLower(filepath.Base(dirPath))
		if base == "secrets" || base == "logs" {
			return true
		}
	}
	return false
}

var suspiciousEnvSuffixes = []string{"_TOKEN", "_KEY", "_SECRET", "_PASSWORD"}

func (a *app) cmdAudit(rest []string) error {
	if len(rest) > 0 {
		return usageErr("audit takes no arguments", "")
	}
	cfg, err := a.config(true)
	if err != nil {
		return err
	}
	configPath, _ := filepath.Abs(a.opts.configPath)
	dataDir := filepath.Dir(configPath)
	var findings []auditFinding
	add := func(f auditFinding) { findings = append(findings, f) }

	// ACL targets: dataDir, config.json, logDir, secrets/ parent dir (when present), and all secret files.
	targets := []aclTarget{
		{label: "dataDir", path: dataDir, isDir: true},
		{label: "config", path: configPath},
		{label: "logDir", path: cfg.LogDir, isDir: true, optional: true},
	}
	seen := map[string]bool{}
	for _, t := range targets {
		seen[pathKey(t.path)] = true
	}
	addTarget := func(t aclTarget) {
		k := pathKey(t.path)
		if !seen[k] {
			seen[k] = true
			targets = append(targets, t)
		}
	}

	// Audit <dataDir>/secrets directory when present.
	defaultSecrets := filepath.Join(dataDir, "secrets")
	if st, statErr := os.Stat(defaultSecrets); statErr == nil && st.IsDir() {
		addTarget(aclTarget{label: "secretsDir", path: defaultSecrets, isDir: true})
	}

	refs := collectSecretRefs(cfg)
	for _, r := range refs {
		// Also audit any parent "secrets" directory containing referenced secret files.
		parentDir := filepath.Dir(r.path)
		if strings.EqualFold(filepath.Base(parentDir), "secrets") {
			if st, statErr := os.Stat(parentDir); statErr == nil && st.IsDir() {
				addTarget(aclTarget{label: "secretsDir", path: parentDir, isDir: true})
			}
		}
		addTarget(aclTarget{label: r.label, path: r.path})
		if r.bearer {
			next := filepath.Clean(r.path) + ".next"
			if _, err := os.Stat(next); err == nil || !errors.Is(err, os.ErrNotExist) {
				label := r.label + " (.next)"
				refs = append(refs, secretRef{label: label, path: next, owner: r.owner, bearer: true})
				addTarget(aclTarget{label: label, path: next})
			}
		}
	}

	fixed := map[string]bool{}
	if a.opts.fixACL {
		for _, t := range targets {
			res := hub.AuditFileACL(t.path)
			if !res.Exists || res.Secure {
				continue
			}
			if t.isDir && !canAutoFixDirACL(t.path, dataDir) {
				add(auditFinding{Check: "fix-acl", Target: t.path, Status: "warn", Detail: "custom directory requires manual ACL repair"})
				continue
			}
			if err := hub.ResetFileACL(t.path, t.isDir); err != nil {
				add(auditFinding{Check: "fix-acl", Target: t.path, Status: "warn", Detail: err.Error()})
				continue
			}
			fixed[pathKey(t.path)] = true
		}
	}
	for _, t := range targets {
		res := hub.AuditFileACL(t.path)
		f := auditFinding{Check: "acl", Target: t.label + " " + t.path}
		switch {
		case !res.Exists:
			f.Status = "violation"
			if t.optional {
				f.Status = "warn"
			}
			f.Detail = strings.Join(res.Issues, "; ")
		case res.Secure && fixed[pathKey(t.path)]:
			f.Status, f.Detail = "fixed", "restricted access"
		case res.Secure:
			f.Status = "ok"
		default:
			f.Status, f.Detail = "violation", strings.Join(res.Issues, "; ")
		}
		add(f)
	}

	// Weak Bearer tokens + duplicate secret reuse across ALL secret files (bearer and non-bearer).
	type tokInfo struct {
		owners       map[string]bool
		hasBearer    bool
		hasNonBearer bool
		labels       []string
	}
	byHash := map[string]*tokInfo{}
	checkedWeakPath := map[string]bool{}
	for _, r := range refs {
		if r.policyFile {
			continue
		}
		val, err := hub.ReadSecretFile(r.path)
		if err != nil {
			if r.bearer || !errors.Is(err, os.ErrNotExist) {
				add(auditFinding{Check: "secret-read", Target: r.label, Status: "violation", Detail: err.Error()})
			}
			continue
		}
		if r.bearer && !checkedWeakPath[pathKey(r.path)] {
			checkedWeakPath[pathKey(r.path)] = true
			if len(val) < minTokenChars {
				add(auditFinding{Check: "weak-secret", Target: r.label + " " + r.path, Status: "violation",
					Detail: fmt.Sprintf("%d chars (< %d); regenerate with init-token/rotate-token", len(val), minTokenChars)})
			}
		}
		sum := sha256.Sum256([]byte(val))
		h := hex.EncodeToString(sum[:])
		ti := byHash[h]
		if ti == nil {
			ti = &tokInfo{owners: map[string]bool{}}
			byHash[h] = ti
		}
		ti.owners[r.owner] = true
		if r.bearer {
			ti.hasBearer = true
		} else {
			ti.hasNonBearer = true
		}
		ti.labels = append(ti.labels, r.label)
	}
	for _, h := range sortedKeys(byHash) {
		ti := byHash[h]
		if len(ti.owners) > 1 {
			add(auditFinding{Check: "duplicate-token", Target: strings.Join(sortedKeys(ti.owners), ", "), Status: "violation",
				Detail: "same secret value reused across: " + strings.Join(ti.labels, ", ")})
		} else if ti.hasBearer && ti.hasNonBearer {
			add(auditFinding{Check: "duplicate-token", Target: strings.Join(sortedKeys(ti.owners), ", "), Status: "violation",
				Detail: "client bearer token reuses internal/upstream secret: " + strings.Join(ti.labels, ", ")})
		}
	}

	// Inline secrets and insecure switches.
	for _, s := range cfg.Services {
		for _, k := range sortedKeys(s.Env) {
			ku := strings.ToUpper(k)
			if strings.HasSuffix(ku, "_FILE") || strings.HasSuffix(ku, "_FILES") || strings.TrimSpace(s.Env[k]) == "" {
				continue
			}
			for _, suf := range suspiciousEnvSuffixes {
				if strings.HasSuffix(ku, suf) {
					add(auditFinding{Check: "inline-secret", Target: fmt.Sprintf("service %s env[%s]", s.Name, k), Status: "violation",
						Detail: "secret-like value inline in config.json; move it to envFiles"})
					break
				}
			}
		}
		if s.AllowNonLoopback {
			add(auditFinding{Check: "allowNonLoopback", Target: "service " + s.Name, Status: "violation", Detail: "non-loopback upstream allowed"})
		}
	}
	if cfg.AllowInsecureNoControlAuth {
		add(auditFinding{Check: "allowInsecureNoControlAuth", Target: "config", Status: "violation", Detail: "control API accepts unauthenticated requests"})
	}
	for _, p := range cfg.Proxies {
		if p.AllowNonLoopback {
			add(auditFinding{Check: "allowNonLoopback", Target: "proxy " + p.Name, Status: "violation", Detail: "non-loopback target allowed"})
		}
	}
	if cfg.Gateway != nil {
		for _, r := range cfg.Gateway.Routes {
			if r.AllowNonLoopback {
				add(auditFinding{Check: "allowNonLoopback", Target: "gateway " + r.Slug, Status: "violation", Detail: "non-loopback target allowed"})
			}
		}
	}

	findings = append(findings, auditInstallation()...)

	violations := 0
	for _, f := range findings {
		if f.Status == "violation" {
			violations++
		}
	}
	if !a.opts.asJSON {
		a.printAuditFindings(findings, dataDir)
		a.printf("\n%d finding(s), %d violation(s)\n", len(findings), violations)
	}
	if violations > 0 {
		hint := "fix the listed issues"
		if !a.opts.fixACL {
			hint = "re-run with --fix-acl to repair NTFS ACLs; other violations need config changes"
		}
		if a.opts.asJSON {
			return &cliError{exit: exitAudit, code: "audit_violation", message: fmt.Sprintf("%d violation(s)", violations), hint: hint,
				extra: map[string]any{"findings": findings, "violations": violations}}
		}
		return &cliError{exit: exitAudit, silent: true}
	}
	if a.opts.asJSON {
		a.writeJSON(map[string]any{"ok": true, "violations": 0, "findings": findings})
	}
	return nil
}

func (a *app) printAuditFindings(findings []auditFinding, dataDir string) {
	tbl := newColTable(a.style,
		tableCol{Header: "CHECK", Align: alignLeft},
		tableCol{Header: "TARGET", Align: alignLeft},
		tableCol{Header: "RESULT", Align: alignLeft},
		tableCol{Header: "DETAIL", Align: alignLeft},
	)
	targetCap := 42
	detailCap := max(44, a.style.effectiveWidth()-72)
	for _, f := range findings {
		tbl.addRow(
			f.Check,
			compactAuditTarget(f.Target, dataDir, targetCap),
			a.style.resultBadge(f.Status),
			truncateDisplay(oneLine(f.Detail), detailCap),
		)
	}
	tbl.render(a.out)
}

func auditInstallation() []auditFinding {
	exe, err := os.Executable()
	if err != nil {
		return nil
	}
	return auditInstallationFrom(exe)
}

func isWithinDir(baseDir, targetPath string) bool {
	absBase, err1 := filepath.Abs(baseDir)
	absTarget, err2 := filepath.Abs(targetPath)
	if err1 != nil || err2 != nil {
		return false
	}
	if resolvedBase, err := filepath.EvalSymlinks(absBase); err == nil {
		absBase = resolvedBase
	}
	if resolvedTarget, err := filepath.EvalSymlinks(absTarget); err == nil {
		absTarget = resolvedTarget
	}
	rel, err := filepath.Rel(absBase, absTarget)
	if err != nil {
		return false
	}
	if rel == "." {
		return false
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return !filepath.IsAbs(rel)
}

func auditInstallationFrom(exe string) []auditFinding {
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	installDir := filepath.Dir(exe)
	receipt := filepath.Join(installDir, "installation.json")
	raw, err := os.ReadFile(receipt)
	if err != nil {
		return []auditFinding{{Check: "installation", Target: receipt, Status: "skip", Detail: "no installation.json next to executable"}}
	}
	raw = bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF})
	var r struct {
		Version  string `json:"version"`
		Binaries map[string]struct {
			Path   string `json:"path"`
			Sha256 string `json:"sha256"`
		} `json:"binaries"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return []auditFinding{{Check: "installation", Target: receipt, Status: "violation", Detail: "unparseable: " + err.Error()}}
	}
	if len(r.Binaries) == 0 {
		return []auditFinding{{Check: "installation", Target: receipt, Status: "violation", Detail: "installation.json has empty binaries map"}}
	}
	var out []auditFinding
	for _, name := range sortedKeys(r.Binaries) {
		b := r.Binaries[name]
		f := auditFinding{Check: "binary-sha256", Target: name + " " + b.Path}
		if strings.TrimSpace(b.Path) == "" || strings.TrimSpace(b.Sha256) == "" {
			f.Status, f.Detail = "violation", "missing path or sha256 in installation.json"
			out = append(out, f)
			continue
		}
		binPath := b.Path
		if !filepath.IsAbs(binPath) {
			binPath = filepath.Join(installDir, binPath)
		}
		if !isWithinDir(installDir, binPath) {
			f.Status, f.Detail = "violation", "binary path resolves outside install directory"
			out = append(out, f)
			continue
		}
		got, err := fileSHA256(binPath)
		switch {
		case err != nil:
			f.Status, f.Detail = "violation", err.Error()
		case !strings.EqualFold(got, b.Sha256):
			f.Status, f.Detail = "violation", "SHA-256 mismatch with installation.json (binary changed after install)"
		default:
			f.Status, f.Detail = "ok", "matches installation.json"
		}
		out = append(out, f)
	}
	return out
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ---------------------------------------------------------------------------
// init-token / rotate-token
// ---------------------------------------------------------------------------

func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func fingerprint(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return "sha256:" + hex.EncodeToString(sum[:])[:12] + "..."
}

func (a *app) cmdInitToken(rest []string) error {
	if len(rest) != 1 {
		return usageErr("usage: mcp-hub-ctl init-token <path> [--force]", "")
	}
	path := filepath.Clean(rest[0])
	unlock, err := hub.LockTokenFile(path)
	if err != nil {
		return &cliError{exit: exitRuntime, code: "token_lock_failed", message: err.Error()}
	}
	defer unlock()
	tok, err := newToken()
	if err != nil {
		return &cliError{exit: exitRuntime, code: "rand_failed", message: err.Error()}
	}
	if err := a.publishToken(path, tok); err != nil {
		if errors.Is(err, os.ErrExist) {
			return &cliError{exit: exitUsage, code: "file_exists", message: fmt.Sprintf("%s already exists", path),
				hint: "use rotate-token to replace a live token, or --force to overwrite"}
		}
		return &cliError{exit: exitRuntime, code: "write_failed", message: err.Error()}
	}
	fp := fingerprint(tok)
	if a.opts.asJSON {
		a.writeJSON(map[string]any{"ok": true, "path": path, "fingerprint": fp})
		return nil
	}
	a.printf("wrote %s (%s)\n", path, fp)
	return nil
}

func (a *app) publishToken(path, token string) error {
	if a.opts.force {
		return hub.WritePrivateFileAtomic(path, []byte(token))
	}
	return hub.WritePrivateFileAtomicNew(path, []byte(token))
}

func (a *app) cmdRotateToken(rest []string) error {
	if len(rest) != 1 || a.opts.stage == a.opts.commit {
		return usageErr("usage: mcp-hub-ctl rotate-token <path> --stage|--commit [--no-reload] [--force]", "")
	}
	if a.opts.commit && a.opts.force {
		return usageErr("--force is only valid with --stage, not --commit", "run `rotate-token <path> --commit` without --force")
	}
	path := filepath.Clean(rest[0])
	unlock, err := hub.LockTokenFile(path)
	if err != nil {
		return &cliError{exit: exitRuntime, code: "token_lock_failed", message: err.Error()}
	}
	defer unlock()
	reload, err := a.prepareRotationReload(path)
	if err != nil {
		return err
	}
	next := path + ".next"
	if a.opts.stage {
		return a.rotateStage(path, next, reload)
	}
	return a.rotateCommit(path, next, reload)
}

func (a *app) rotateStage(path, next string, reload *rotationReload) error {
	if _, err := os.Stat(path); err != nil {
		return &cliError{exit: exitUsage, code: "token_missing", message: fmt.Sprintf("%s does not exist", path), hint: "create it first with init-token"}
	}
	tok, err := newToken()
	if err != nil {
		return &cliError{exit: exitRuntime, code: "rand_failed", message: err.Error()}
	}
	if err := a.publishToken(next, tok); err != nil {
		if errors.Is(err, os.ErrExist) {
			return &cliError{exit: exitUsage, code: "next_exists", message: fmt.Sprintf("%s already staged", next),
				hint: "run --commit to promote it, or --stage --force to replace the staged token"}
		}
		return &cliError{exit: exitRuntime, code: "write_failed", message: err.Error()}
	}
	result := map[string]any{"ok": true, "action": "stage", "path": path, "next": next, "fingerprint": fingerprint(tok)}
	if reload != nil {
		if err := a.reloadRotation(reload); err != nil {
			ce := *a.classify(err)
			ce.hint = fmt.Sprintf("token staged at %s; run `mcp-hub-ctl reload` once the hub is up (or pass --no-reload)", next)
			ce.extra = map[string]any{"staged": next, "fingerprint": fingerprint(tok)}
			return &ce
		}
		result["reloaded"] = true
	}
	if a.opts.asJSON {
		a.writeJSON(result)
		return nil
	}
	a.printf("staged %s (%s)\n", next, fingerprint(tok))
	if !a.opts.noReload {
		a.printf("hub reloaded: old and staged tokens are both accepted (grace period)\n")
	}
	a.printf("next steps:\n")
	a.printf("  1. update clients to read the new token from %s\n", next)
	a.printf("  2. mcp-hub-ctl rotate-token %s --commit\n", path)
	return nil
}

func (a *app) rotateCommit(path, next string, reload *rotationReload) error {
	newTok, err := hub.ReadSecretFile(next)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &cliError{exit: exitUsage, code: "next_missing", message: fmt.Sprintf("%s does not exist", next), hint: "run --stage first"}
		}
		return &cliError{exit: exitUsage, code: "next_invalid", message: err.Error()}
	}
	oldTok := ""
	if reload != nil && reload.control {
		oldTok, err = hub.ReadSecretFile(path)
		if err != nil {
			return &cliError{exit: exitUsage, code: "token_file_unreadable", message: err.Error()}
		}
		// Catch stale control credentials before the old on-disk token is replaced.
		ctx, cancel := a.ctx(defaultTimeout)
		_, _, err = reload.client.Status(ctx)
		cancel()
		if err != nil {
			return err
		}
		// If staging was offline, load the grace token while the accepted
		// primary is still on disk. After promotion the new token can then
		// authenticate a retry even if the final reload fails.
		if oldTok != newTok {
			if err := a.ensureStagedControlToken(reload, newTok); err != nil {
				return err
			}
		}
		reload.client = a.newHubClient(reload.addr, newTok)
	}
	if err := hub.WritePrivateFileAtomic(path, []byte(newTok)); err != nil {
		return &cliError{exit: exitRuntime, code: "write_failed", message: err.Error()}
	}
	if err := os.Remove(next); err != nil && !errors.Is(err, os.ErrNotExist) {
		return &cliError{exit: exitRuntime, code: "remove_next_failed", message: err.Error(),
			hint: "token promoted but .next could not be removed; delete it manually before reloading"}
	}
	result := map[string]any{"ok": true, "action": "commit", "path": path, "fingerprint": fingerprint(newTok)}
	if reload != nil {
		if err := a.reloadRotation(reload); err != nil {
			ce := *classifyDiagnostic(err, a.controlToken, newTok)
			ce.hint = fmt.Sprintf("token committed to %s; verify which token the hub accepts before retrying reload", path)
			ce.extra = map[string]any{"committed": path, "fingerprint": fingerprint(newTok)}
			return &ce
		}
		result["reloaded"] = true
		if reload.control {
			if err := a.verifyRotation(reload.addr, oldTok, newTok); err != nil {
				return err
			}
			if oldTok != newTok {
				result["oldTokenRejected"] = true
			}
		}
	}
	if a.opts.asJSON {
		a.writeJSON(result)
		return nil
	}
	a.printf("committed %s (%s); %s removed\n", path, fingerprint(newTok), filepath.Base(next))
	if !a.opts.noReload {
		a.printf("hub reloaded: single token active\n")
		if result["oldTokenRejected"] == true {
			a.printf("verified: previous control token now rejected (401)\n")
		}
	}
	return nil
}

type rotationReload struct {
	client  *hub.Client
	addr    string
	control bool
}

func (a *app) prepareRotationReload(path string) (*rotationReload, error) {
	if a.opts.noReload {
		return nil, nil
	}
	cfg, err := a.config(false)
	if err != nil {
		return nil, err
	}
	addr, tokenFile, err := a.resolveControl(true)
	if err != nil {
		return nil, err
	}
	configControl := cfg != nil && samePath(path, cfg.ControlTokenFile)
	if !configControl && cfg != nil && strings.TrimSpace(cfg.ControlTokenFile) != "" {
		targetInfo, targetErr := os.Stat(path)
		controlInfo, controlErr := os.Stat(cfg.ControlTokenFile)
		configControl = targetErr == nil && controlErr == nil && os.SameFile(targetInfo, controlInfo)
	}
	if configControl && !sameBind(addr, cfg.ControlAddr) {
		return nil, usageErr("rotation target belongs to a different configured control address", "use its configured control address or --no-reload")
	}
	client, dialAddr, err := a.connect(true)
	if err != nil {
		return nil, err
	}
	return &rotationReload{client: client, addr: dialAddr, control: configControl || samePath(path, tokenFile)}, nil
}

func (a *app) reloadRotation(reload *rotationReload) error {
	ctx, cancel := a.ctx(40 * time.Second)
	defer cancel()
	res, _, err := reload.client.ReloadHub(ctx)
	if err != nil {
		return err
	}
	if !res.OK {
		return errors.New("hub did not confirm reload")
	}
	return nil
}

func (a *app) ensureStagedControlToken(reload *rotationReload, staged string) error {
	client := a.newHubClient(reload.addr, staged)
	ctx, cancel := a.ctx(defaultTimeout)
	_, _, err := client.Status(ctx)
	cancel()
	if err == nil {
		return nil
	}
	if classify(err).code == "unauthorized" {
		if err := a.reloadRotation(reload); err != nil {
			return classifyDiagnostic(err, a.controlToken, staged)
		}
		ctx, cancel = a.ctx(defaultTimeout)
		_, _, err = client.Status(ctx)
		cancel()
		if err == nil {
			return nil
		}
	}
	return &cliError{exit: classify(err).exit, code: "staged_token_not_verified", message: "hub did not accept the staged control token before commit"}
}

func (a *app) verifyRotation(addr, oldTok, newTok string) error {
	ctx, cancel := a.ctx(defaultTimeout)
	_, _, err := a.newHubClient(addr, newTok).Status(ctx)
	cancel()
	if err != nil {
		return &cliError{exit: exitRuntime, code: "new_token_not_verified", message: "token committed but hub did not accept the new control token"}
	}
	if oldTok == newTok {
		return nil
	}
	ctx, cancel = a.ctx(defaultTimeout)
	_, _, err = a.newHubClient(addr, oldTok).Status(ctx)
	cancel()
	if err != nil && classify(err).code == "unauthorized" {
		return nil
	}
	if err == nil {
		return &cliError{exit: exitRuntime, code: "old_token_still_active", message: "token committed but hub still accepts the previous control token"}
	}
	return &cliError{exit: exitRuntime, code: "old_token_not_rejected", message: "token committed but hub did not return HTTP 401 for the previous control token"}
}

// ---------------------------------------------------------------------------
// small helpers
// ---------------------------------------------------------------------------

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func dash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

func intOrDash(n int) string {
	if n <= 0 {
		return "-"
	}
	return strconv.Itoa(n)
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 3 {
		return string(r[:n])
	}
	return string(r[:n-3]) + "..."
}

func printUsage(w io.Writer) {
	ver := hub.Version
	if hub.GitCommit != "" {
		ver += " (" + hub.GitCommit + ")"
	}
	fmt.Fprintf(w, `mcp-hub-ctl %s - console CLI for mcp-hub

Usage:
  mcp-hub-ctl <command> [args] [flags]

Inspect:
  health | healthz                          GET /healthz (no auth); exit 1 when ok=false
  status [--watch [2s]]                     GET /status: services, proxies, gateway, errors
  stats [service]                           Per-tool call/error/latency metrics (alias: metrics)
  tools <service> [--wake] [--schema]       Inspect live MCP tools, readOnlyHint, and inputSchema
  logs <service|hub> [-n 50] [-f] [--grep <text>]
                                            Ring-buffer logs via the hub (-f streams SSE)
  logs <service|hub> --offline [--grep <text>]
                                            Tail <logDir>/<name>.log directly (redacted)
  catalog [--probe-tools] [--wake]          Client URLs, auth and capabilities (+ live tools/list)
  probe [service] [--mcp] [--wake]          End-to-end checks: healthz, 401/403 guards, MCP (alias: doctor)
  snippet <service> [--client <preset>]     Ready-to-paste client config (token as placeholder)
                                            Presets: agy, claude, codex, cursor, vscode, windsurf,
                                                     gemini, opencode, devin, all

Control:
  start <service|--all>                     POST /start/<name> (or /services/start-all)
  start --hub [--wait]                      Launch mcp-hub detached (--wait polls /healthz)
  stop <service|--all>                      POST /stop/<name> (bare "stop" is refused)
  stop --hub | shutdown [--wait]            POST /stop: stop the hub (--wait honors --timeout)
  sleep <service>                           POST /sleep/<name>: put idle-capable service to sleep
  wake <service>                            POST /wake/<name>: wake sleeping service on demand
  restart <service|--all> [--wait]          POST /restart/<name> (--wait blocks until healthy)
  reload [service]                          POST /reload (config + tokens) or /reload/<service>

Dashboard & Workspace:
  ui [--no-open]                            Mint one-time Web Dashboard URL and open browser
  open-logs [service|hub]                   Open log directory or specific log file (or print path with --json)
  open-config                               Open config.json in default editor (or print path with --json)

Config & Secrets:
  init [--dir <dataDir>] [--force]          Bootstrap portable dataDir, private ACLs, control.token & config.json
  validate                                  Parse config, check secret files and executables
  audit [--fix-acl]                         ACLs, weak/duplicate/inline secrets, insecure switches, binary hashes
  init-token <path> [--force]               Write a new 256-bit token (prints fingerprint only)
  rotate-token <path> --stage|--commit [--no-reload] [--force]
                                            Two-phase rotation via <path>.next with hub reload
  version                                   Print version (and git commit when built with ldflags)

Global flags (accepted anywhere):
  -c, --config <path>    Config file (default: %s)
  --addr <host:port>     Override control address (loopback only; config token not sent elsewhere)
  --token-file <path>    Override control Bearer token file
  --timeout <dur>        Request / wait timeout (default 20s)
  --json                 JSON on stdout, including errors (one object per line for watch/follow)

Exit codes: 0 ok, 1 runtime/unhealthy, 2 usage/invalid config, 3 hub unreachable/timeout,
            4 auth (401/403), 5 not found, 6 audit violation
`, ver, hub.ResolveDefaultConfigPath())
}
