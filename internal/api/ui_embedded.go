//go:build !dev

package api

import "embed"

//go:embed all:ui/dist
var uiFS embed.FS
