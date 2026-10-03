// Package web embeds the dashboard's static assets into the binary.
package web

import "embed"

// Files contains index.html, style.css, app.js and icons, served at "/".
//
//go:embed index.html style.css app.js favicon.svg
var Files embed.FS
