package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
		"admin/keys":        "must not replace admin API",
		"ai/generate-image": "must not replace image API",
		"quota":             "must not replace quota API",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	api, err := proxy.NewManaged(proxy.ManagedConfig{
		AdminKey:  "admin-0123456789abcdef0123456789abcdef",
		StatePath: filepath.Join(t.TempDir(), "keys.json"),
	})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := mountFrontend(api, dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		method, path string
		status       int
		contains     string
	}{
		{http.MethodGet, "/", http.StatusOK, "management console"},
		{http.MethodHead, "/", http.StatusOK, ""},
		{http.MethodGet, "/assets/app.js", http.StatusOK, "console.log"},
		{http.MethodGet, "/assets/missing.js", http.StatusNotFound, ""},
		{http.MethodGet, "/admin/keys", http.StatusUnauthorized, ""},
		{http.MethodGet, "/ai/generate-image", http.StatusUnauthorized, ""},
		{http.MethodGet, "/quota", http.StatusUnauthorized, ""},
		{http.MethodGet, "/healthz", http.StatusOK, "ok"},
		{http.MethodGet, "/missing", http.StatusUnauthorized, ""},
		{http.MethodPost, "/", http.StatusUnauthorized, ""},
		{http.MethodPost, "/assets/app.js", http.StatusUnauthorized, ""},
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
}

func TestMountFrontendRequiresIndex(t *testing.T) {
	if _, err := mountFrontend(http.NotFoundHandler(), t.TempDir()); err == nil {
		t.Fatal("expected missing frontend index to fail")
	}
}
