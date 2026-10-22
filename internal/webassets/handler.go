package webassets

import (
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// newAssetsHandler serves files from fsys and falls back to index.html for
// client-side routes, so the frontend can own its own routing.
func newAssetsHandler(fsys fs.FS) http.Handler {
	return &assetsHandler{fs: fsys}
}

type assetsHandler struct {
	fs fs.FS
}

func (h *assetsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	clean := path.Clean(r.URL.Path)
	name := strings.TrimPrefix(clean, "/")
	if name != "" && name != "." {
		if file, err := h.fs.Open(name); err == nil {
			_ = file.Close()
			h.serveFile(w, r, name, strings.HasPrefix(clean, "/assets/"))
			return
		}
	}
	if strings.HasPrefix(clean, "/assets/") || path.Ext(clean) != "" {
		http.NotFound(w, r)
		return
	}
	h.serveFile(w, r, "index.html", false)
}

func (h *assetsHandler) serveFile(w http.ResponseWriter, r *http.Request, name string, immutable bool) {
	data, err := fs.ReadFile(h.fs, name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if strings.HasSuffix(name, ".html") {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
	} else if strings.HasSuffix(name, ".js") {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	} else if strings.HasSuffix(name, ".css") {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	} else if strings.HasSuffix(name, ".svg") {
		w.Header().Set("Content-Type", "image/svg+xml")
	} else if strings.HasSuffix(name, ".json") {
		w.Header().Set("Content-Type", "application/json")
	}
	if immutable {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		// Unhashed public files (index.html, config.js, theme-init.js) must be
		// revalidated so a new build is picked up immediately.
		w.Header().Set("Cache-Control", "no-cache")
	}
	_, _ = w.Write(data)
}
