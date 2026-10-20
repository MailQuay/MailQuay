//go:build !full

package webassets

import "net/http"

// Handler is nil for API-only builds.
func Handler() http.Handler { return nil }
