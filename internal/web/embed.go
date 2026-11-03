package web

import (
	"embed"
	"io/fs"
)

//go:embed static
var staticFS embed.FS

// Static is the built-in UI.
func Static() fs.FS {
	sub, _ := fs.Sub(staticFS, "static")
	return sub
}
