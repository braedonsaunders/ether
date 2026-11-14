// Package schemas provides the canonical public JSON schemas.
package schemas

import _ "embed"

// PluginManifest is the EPP/1 plugin manifest JSON Schema.
//
//go:embed plugin-manifest.v1.json
var PluginManifest string

// PluginSources describes source queries, candidates and release hints.
//
//go:embed plugin-sources.v1.json
var PluginSources string
