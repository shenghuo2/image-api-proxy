package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/shenghuo2/novelai-api-proxy/internal/proxy"
)

func TestMountFrontend(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"assets", "admin", "ai"} {
		if err := os.Mkdir(filepath.Join(dir, name), 0755); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range map[string]string{
		"index.html":        "<html>management console</html>",
		"assets/app.js":     "console.log('loaded')",
		"favicon.svg":       "<svg></svg>",
		"admin/keys":        "must not replace admin API",
		"ai/generate-image": "must not replace image API",
		"quota":             "must not replace quota API",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	statePath := filepath.Join(t.TempDir(), "keys.json")
	api, err := proxy.NewManaged(proxy.ManagedConfig{
		AdminKey:  "admin-0123456789abcdef0123456789abcdef",
		StatePath: statePath,
	})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := mountFrontend(api, dir, api.AdminUIPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		method, path string
		status       int
		contains     string
	}{
		{http.MethodGet, "/", http.StatusNotFound, ""},
		{http.MethodGet, "/console", http.StatusPermanentRedirect, ""},
		{http.MethodGet, "/console/", http.StatusOK, "management console"},
		{http.MethodHead, "/console/", http.StatusOK, ""},
		{http.MethodGet, "/console/assets/app.js", http.StatusOK, "console.log"},
		{http.MethodGet, "/console/favicon.svg", http.StatusOK, "<svg"},
		{http.MethodGet, "/console/assets/missing.js", http.StatusNotFound, ""},
		{http.MethodGet, "/assets/app.js", http.StatusNotFound, ""},
		{http.MethodGet, "/admin/keys", http.StatusUnauthorized, ""},
		{http.MethodGet, "/ai/generate-image", http.StatusUnauthorized, ""},
		{http.MethodGet, "/quota", http.StatusUnauthorized, ""},
		{http.MethodGet, "/healthz", http.StatusOK, "ok"},
		{http.MethodGet, "/missing", http.StatusNotFound, ""},
		{http.MethodGet, "/console/missing", http.StatusNotFound, ""},
		{http.MethodPost, "/", http.StatusUnauthorized, ""},
		{http.MethodPost, "/console/assets/app.js", http.StatusUnauthorized, ""},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			result := httptest.NewRecorder()
			handler.ServeHTTP(result, httptest.NewRequest(tc.method, tc.path, nil))
			if result.Code != tc.status || !strings.Contains(result.Body.String(), tc.contains) {
				t.Fatalf("status=%d body=%q, want status=%d containing %q", result.Code, result.Body.String(), tc.status, tc.contains)
			}
		})
	}

	request := httptest.NewRequest(http.MethodGet, "/admin/keys", nil)
	request.Header.Set("Authorization", "Bearer admin-0123456789abcdef0123456789abcdef")
	result := httptest.NewRecorder()
	handler.ServeHTTP(result, request)
	if result.Code != http.StatusOK || strings.TrimSpace(result.Body.String()) != "[]" {
		t.Fatalf("authenticated API request: status=%d body=%q", result.Code, result.Body.String())
	}

	for _, path := range []string{"/", "/admin", "/admin/keys", "/ai", "/jobs", "/console//bad", "/console/%2e%2e/index.html", "/console/sub/../../index.html"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if strings.Contains(response.Body.String(), "management console") {
			t.Fatalf("frontend leaked at %q", path)
		}
	}

	change := httptest.NewRequest(http.MethodPut, "/admin/settings", strings.NewReader(`{"admin_ui_path":"/private/manage_123"}`))
	change.Header.Set("Authorization", "Bearer admin-0123456789abcdef0123456789abcdef")
	changed := httptest.NewRecorder()
	handler.ServeHTTP(changed, change)
	if changed.Code != http.StatusOK {
		t.Fatalf("change path: %d %s", changed.Code, changed.Body.String())
	}
	for path, want := range map[string]int{"/console/": http.StatusNotFound, "/private/manage_123/": http.StatusOK, "/private/manage_123/assets/app.js": http.StatusOK} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != want {
			t.Fatalf("%s: status=%d, want %d", path, response.Code, want)
		}
	}
	restarted, err := proxy.NewManaged(proxy.ManagedConfig{AdminKey: "admin-0123456789abcdef0123456789abcdef", StatePath: statePath})
	if err != nil {
		t.Fatalf("reload settings: %v", err)
	}
	if restarted.AdminUIPath() != "/private/manage_123" {
		t.Fatalf("path did not survive restart: %q", restarted.AdminUIPath())
	}
	for _, path := range []string{"/", "/admin", "/admin/hide", "/ai/hidden", "/jobs/test", "/trailing/", "/bad..path", "/space here", "/back\\slash", "/one//two"} {
		request := httptest.NewRequest(http.MethodPut, "/admin/settings", strings.NewReader(`{"admin_ui_path":`+strconv.Quote(path)+`}`))
		request.Header.Set("Authorization", "Bearer admin-0123456789abcdef0123456789abcdef")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("accepted invalid path %q: %d", path, response.Code)
		}
	}
}

func TestMountFrontendRequiresIndex(t *testing.T) {
	if _, err := mountFrontend(http.NotFoundHandler(), t.TempDir(), func() string { return "/console" }); err == nil {
		t.Fatal("expected missing frontend index to fail")
	}
}
