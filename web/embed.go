// Package web holds the server-rendered templates and static assets for the
// central portal, embedded so the binary is self-contained.
package web

import "embed"

//go:embed templates/*.html
var Templates embed.FS

//go:embed static
var Static embed.FS
