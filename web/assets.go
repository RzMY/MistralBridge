// Package web embeds the plugin console and its logo.
package web

import "embed"

// Files contains the console HTML, styles and scripts.
//
//go:embed index.html style.css json.js app.js
var Files embed.FS

// Logo contains the plugin icon used in registration and the console.
//
//go:embed logo.png
var Logo []byte
