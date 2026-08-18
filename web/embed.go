// Package web embeds the single page UI into the binary, so the container image
// is one static file with no sidecar and no runtime asset fetching.
//
// web/ui holds hand-written source, not build output. There is no bundler and
// nothing generated: edit the files and rebuild the binary.
package web

import "embed"

//go:embed ui
var Assets embed.FS
