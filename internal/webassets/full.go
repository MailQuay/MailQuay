//go:build full

package webassets

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed dist
var embedded embed.FS

// Handler serves the embedded single-page frontend.
func Handler() http.Handler {
	sub, err := fs.Sub(embedded, "dist")
	if err != nil {
		return nil
	}
	return newAssetsHandler(sub)
}
