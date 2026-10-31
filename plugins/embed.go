// Package plugins embeds the eipagent standard plugins. Each plugin is an
// interpreted Go file (//go:build ignore, package plugin) with an Apply
// function; see any file here for the shape.
package plugins

import "embed"

//go:embed *.go
var FS embed.FS
