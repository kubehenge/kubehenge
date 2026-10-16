package ui

import (
	"embed"
	"io/fs"
)

// dist is the frontend as `make web` builds it from web/.
//
//go:embed all:dist
var dist embed.FS

// Frontend returns the built frontend.
func Frontend() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err) // dist is embedded, so it exists
	}
	return sub
}
