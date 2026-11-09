// Package web embeds the three pages (order, kitchen, board) and their
// shared script. No build step, no framework, no external assets.
package web

import (
	"embed"
	"io/fs"
)

//go:embed *.html app.js
var files embed.FS

// Pages returns the embedded files at the root of the FS.
func Pages() fs.FS { return files }
