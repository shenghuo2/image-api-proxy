package main

import (
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"strings"
)

func mountFrontend(api http.Handler, dir string, adminPath func() string) (http.Handler, error) {
	files := os.DirFS(dir)
	index, err := fs.Stat(files, "index.html")
	if err != nil {
		return nil, fmt.Errorf("%s must contain a regular index.html: %w", dir, err)
	}
	if !index.Mode().IsRegular() {
		return nil, fmt.Errorf("%s/index.html is not a regular file", dir)
	}
	static := http.FileServer(http.FS(files))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if (r.Method != http.MethodGet && r.Method != http.MethodHead) ||
			r.URL.EscapedPath() != r.URL.Path || isAPIRoute(r.URL.Path) {
			api.ServeHTTP(w, r)
			return
		}
		path := adminPath()
		if r.URL.Path == path {
			http.Redirect(w, r, path+"/", http.StatusPermanentRedirect)
			return
		}
		if !strings.HasPrefix(r.URL.Path, path+"/") {
			http.NotFound(w, r)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, path+"/")
		if name == "" {
			name = "index.html"
		}
		if fs.ValidPath(name) {
			if info, err := fs.Stat(files, name); err == nil && info.Mode().IsRegular() {
				w.Header().Set("Cache-Control", "no-store")
				w.Header().Set("X-Content-Type-Options", "nosniff")
				http.StripPrefix(path, static).ServeHTTP(w, r)
				return
			}
		}
		http.NotFound(w, r)
	}), nil
}

func isAPIRoute(path string) bool {
	for _, prefix := range []string{"/admin", "/ai", "/image", "/user", "/quota", "/healthz", "/jobs"} {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return true
		}
	}
	return false
}
