package hub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestHTTPGuard_VerifyBearerAndSecurityHeaders(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:18470/status", nil)
	req.Header.Set("Authorization", "Bearer secret-token-value")
	if !VerifyBearer(req, "secret-token-value") {
		t.Fatal("expected matching Bearer token to verify")
	}
	if VerifyBearer(req, "short") {
		t.Fatal("expected mismatched length token to fail")
	}
	if VerifyBearer(req, "") {
		t.Fatal("expected empty expected token to fail")
	}

	// Duplicate Authorization headers must fail.
	reqDup := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:18470/status", nil)
	reqDup.Header.Add("Authorization", "Bearer secret-token-value")
	reqDup.Header.Add("Authorization", "Bearer secret-token-value")
	if VerifyBearer(reqDup, "secret-token-value") {
		t.Fatal("expected duplicate Authorization headers to fail")
	}

	rec := httptest.NewRecorder()
	writeJSON(rec, http.StatusOK, map[string]bool{"ok": true})
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := rec.Header().Get("Referrer-Policy"); got != "no-referrer" {
		t.Errorf("Referrer-Policy = %q, want no-referrer", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
}

func TestHTTPGuard_CheckLoopbackGuardAndSecFetchSite(t *testing.T) {
	allowed := makeAllowedHostSet(DefaultAllowedHosts("127.0.0.1:18470"))

	cases := []struct {
		name           string
		method         string
		host           string
		origin         string
		secFetchSite   string
		allowDirectNav bool
		wantOK         bool
		wantCode       string
	}{
		{
			name:   "non-browser CLI request allowed",
			method: http.MethodPost,
			host:   "127.0.0.1:18470",
			wantOK: true,
		},
		{
			name:     "disallowed host rejected",
			method:   http.MethodGet,
			host:     "evil.example:18470",
			wantOK:   false,
			wantCode: "forbidden_host",
		},
		{
			name:     "origin header rejected",
			method:   http.MethodPost,
			host:     "localhost:18470",
			origin:   "https://evil.example",
			wantOK:   false,
			wantCode: "forbidden_origin",
		},
		{
			name:     "OPTIONS preflight rejected",
			method:   http.MethodOptions,
			host:     "127.0.0.1:18470",
			wantOK:   false,
			wantCode: "forbidden_preflight",
		},
		{
			name:         "cross-site fetch rejected",
			method:       http.MethodGet,
			host:         "127.0.0.1:18470",
			secFetchSite: "cross-site",
			wantOK:       false,
			wantCode:     "forbidden_fetch_site",
		},
		{
			name:         "same-site fetch rejected",
			method:       http.MethodGet,
			host:         "127.0.0.1:18470",
			secFetchSite: "same-site",
			wantOK:       false,
			wantCode:     "forbidden_fetch_site",
		},
		{
			name:         "same-origin fetch rejected",
			method:       http.MethodGet,
			host:         "127.0.0.1:18470",
			secFetchSite: "same-origin",
			wantOK:       false,
			wantCode:     "forbidden_fetch_site",
		},
		{
			name:           "Sec-Fetch-Site none rejected without allowDirectNav",
			method:         http.MethodGet,
			host:           "127.0.0.1:18470",
			secFetchSite:   "none",
			allowDirectNav: false,
			wantOK:         false,
			wantCode:       "forbidden_fetch_site",
		},
		{
			name:           "Sec-Fetch-Site none allowed when allowDirectNav is true",
			method:         http.MethodGet,
			host:           "127.0.0.1:18470",
			secFetchSite:   "none",
			allowDirectNav: true,
			wantOK:         true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, "http://127.0.0.1:18470/healthz", nil)
			req.Host = tc.host
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			if tc.secFetchSite != "" {
				req.Header.Set("Sec-Fetch-Site", tc.secFetchSite)
			}
			rec := httptest.NewRecorder()
			ok := CheckLoopbackGuard(rec, req, allowed, tc.allowDirectNav)
			if ok != tc.wantOK {
				t.Fatalf("CheckLoopbackGuard = %v, want %v (body=%s)", ok, tc.wantOK, rec.Body.String())
			}
			if !tc.wantOK {
				if rec.Code != http.StatusForbidden {
					t.Errorf("status = %d, want 403", rec.Code)
				}
				var payload map[string]string
				if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
					t.Fatalf("decode error JSON: %v", err)
				}
				if payload["error"] != tc.wantCode {
					t.Errorf("error code = %q, want %q", payload["error"], tc.wantCode)
				}
			}
		})
	}
}

func TestTokenStore_DualTokenGraceWindowAndRules(t *testing.T) {
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "proxy.token")
	nextPath := tokenPath + ".next"

	if err := os.WriteFile(tokenPath, []byte("primary-token-111111\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	store := NewTokenStore()
	if !store.Empty() {
		t.Fatal("expected new TokenStore to be empty")
	}

	// Single token file (no .next yet).
	loaded, err := store.LoadTokenFileWithGrace("admin", tokenPath, []string{"read_*"}, []string{"delete_*"}, true)
	if err != nil {
		t.Fatalf("LoadTokenFileWithGrace primary only: %v", err)
	}
	if len(loaded) != 1 || loaded[0] != "primary-token-111111" {
		t.Fatalf("loaded = %v, want [primary-token-111111]", loaded)
	}
	if store.Empty() {
		t.Fatal("expected TokenStore non-empty after load")
	}

	reqPrimary := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/mcp", nil)
	reqPrimary.Header.Set("Authorization", "Bearer primary-token-111111")
	rule, ok := store.Match(reqPrimary)
	if !ok || rule.Name != "admin" || !rule.ReadOnlyOnly || len(rule.AllowTools) != 1 || rule.AllowTools[0] != "read_*" {
		t.Fatalf("Match primary failed: ok=%v rule=%+v", ok, rule)
	}

	// Add .next grace token and reload with untrimmed/uncleaned path: BOTH primary and .next must authenticate!
	if err := os.WriteFile(nextPath, []byte("rotated-token-222222\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dirtyPath := "  " + filepath.Join(dir, ".", "proxy.token") + "  "
	loaded, err = store.LoadTokenFileWithGrace("admin", dirtyPath, []string{"read_*"}, nil, false)
	if err != nil {
		t.Fatalf("LoadTokenFileWithGrace with .next: %v", err)
	}
	if len(loaded) != 2 {
		t.Fatalf("expected 2 loaded secrets with .next, got %v", loaded)
	}

	reqNext := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/mcp", nil)
	reqNext.Header.Set("Authorization", "Bearer rotated-token-222222")
	reqBad := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/mcp", nil)
	reqBad.Header.Set("Authorization", "Bearer wrong-token-999999")

	if !store.Verify(reqPrimary) {
		t.Fatal("expected primary token to remain valid during grace window")
	}
	if !store.Verify(reqNext) {
		t.Fatal("expected .next token to be valid during grace window")
	}
	if store.Verify(reqBad) {
		t.Fatal("expected unknown token to be rejected")
	}

	// SetSingle replaces rules cleanly, and empty secret clears store.
	store.SetSingle("solo", "solo-token-333333", nil, nil, false)
	if store.Verify(reqPrimary) {
		t.Fatal("expected old primary token invalid after SetSingle")
	}
	reqSolo := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/mcp", nil)
	reqSolo.Header.Set("Authorization", "Bearer solo-token-333333")
	if !store.Verify(reqSolo) {
		t.Fatal("expected solo token valid after SetSingle")
	}
	store.SetSingle("solo", "", nil, nil, false)
	if !store.Empty() || store.Verify(reqSolo) {
		t.Fatal("expected empty SetSingle to clear TokenStore")
	}
}
