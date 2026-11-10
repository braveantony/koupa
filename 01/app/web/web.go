// Package web embeds the three pages (order, kitchen, board), their shared
// script and stylesheet, and the drink photos. No build step, no framework,
// no external assets.
package web

import (
	"embed"
	"io/fs"
)

//go:embed *.html app.js app.css img/*.webp
var files embed.FS

// Pages returns the embedded files at the root of the FS.
func Pages() fs.FS { return files }
