// Package attention embeds the PWA so the server ships as a single binary.
package attention

import "embed"

//go:embed web
var Web embed.FS
