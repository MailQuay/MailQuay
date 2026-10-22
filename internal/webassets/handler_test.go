package webassets

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func testAssets() fs.FS {
	return fstest.MapFS{
		"index.html":        {Data: []byte(`<!doctype html><div id="root"></div>`)},
		"assets/app-abc.js": {Data: []byte(`console.log('app')`)},
		"config.js":         {Data: []byte(`window.__MAILQUAY_CONFIG__ = {};`)},
		"theme-init.js":     {Data: []byte(`(() => {})();`)},
	}
}

func serve(fsys fs.FS, method, target string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	newAssetsHandler(fsys).ServeHTTP(recorder, httptest.NewRequest(method, target, nil))
	return recorder
}

func TestClientRoutesFallBackToIndex(t *testing.T) {
	for _, target := range []string{"/", "/settings/general", "/accounts/a/mailboxes/b"} {
		recorder := serve(testAssets(), http.MethodGet, target)
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200", target, recorder.Code)
		}
		if got := recorder.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
			t.Fatalf("%s: content type = %q", target, got)
		}
		if got := recorder.Header().Get("Cache-Control"); got != "no-cache" {
			t.Fatalf("%s: cache control = %q", target, got)
		}
		if !strings.Contains(recorder.Body.String(), `id="root"`) {
			t.Fatalf("%s: body does not contain the app mount point", target)
		}
	}
}

func TestPublicFilesAreRevalidated(t *testing.T) {
	for _, target := range []string{"/config.js", "/theme-init.js"} {
		recorder := serve(testAssets(), http.MethodGet, target)
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200", target, recorder.Code)
		}
		if got := recorder.Header().Get("Cache-Control"); got != "no-cache" {
			t.Fatalf("%s: cache control = %q", target, got)
		}
	}
}

func TestHashedAssetsAreImmutable(t *testing.T) {
	recorder := serve(testAssets(), http.MethodGet, "/assets/app-abc.js")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if got := recorder.Header().Get("Content-Type"); got != "text/javascript; charset=utf-8" {
		t.Fatalf("content type = %q", got)
	}
	if got := recorder.Header().Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
		t.Fatalf("cache control = %q", got)
	}
}

func TestMissingFilesDoNotFallBack(t *testing.T) {
	for _, target := range []string{"/assets/missing.js", "/missing.js", "/favicon.ico"} {
		recorder := serve(testAssets(), http.MethodGet, target)
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("%s: status = %d, want 404", target, recorder.Code)
		}
	}
}

func TestWritesAreRejected(t *testing.T) {
	recorder := serve(testAssets(), http.MethodPost, "/")
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", recorder.Code)
	}
}
