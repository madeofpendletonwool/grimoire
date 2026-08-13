// Package web embeds the Grimoire front-end assets (templates + static).
package web

import "embed"

// Templates holds the HTML templates.
//
//go:embed templates/*.html
var Templates embed.FS

// Static holds the static front-end files: css, js, the pixel-art sprites and
// scene layers under assets/, and the self-hosted webfonts under fonts/.
//
// `all:` rather than a bare glob so nothing is skipped for having a name Go's
// embed would otherwise ignore.
//
//go:embed all:static
var Static embed.FS
