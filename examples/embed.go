// Package examples embeds the example seeds, so the MCP server can offer
// them to seed authors without the repository at hand.
package examples

import "embed"

// FS holds the example seeds (*.yaml).
//
//go:embed *.yaml
var FS embed.FS
