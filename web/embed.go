// Package web embeds the Grimoire front-end assets (templates + static).
package web

import "embed"

// Templates holds the HTML templates.
//
//go:embed templates/*.html
var Templates embed.FS

// Static holds the static front-end files (css, js).
//
//go:embed static/*
var Static embed.FS
