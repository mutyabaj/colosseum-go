//go:build dev

package api

import "embed"

// uiFS is empty in dev mode — the server will show a placeholder message for UI routes.
var uiFS embed.FS
