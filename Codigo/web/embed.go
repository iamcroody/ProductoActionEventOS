// Package webassets embeds the built Action EventOS single-page app so the
// whole UI ships inside the single .exe — no Node or web server to install.
// The Vite build outputs to ./dist, which is embedded here.
package webassets

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var distFS embed.FS

//go:embed cliente/index.html
var clienteFS embed.FS

// FS returns the embedded built management SPA rooted at dist/.
func FS() fs.FS {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		panic(err)
	}
	return sub
}

// ClienteFS returns the embedded standalone customer web rooted at cliente/.
// It is a single self-contained black page served at /cliente for QR self-order.
func ClienteFS() fs.FS {
	sub, err := fs.Sub(clienteFS, "cliente")
	if err != nil {
		panic(err)
	}
	return sub
}
