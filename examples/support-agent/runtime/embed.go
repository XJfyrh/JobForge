// Package supportdata embeds only the registered runtime policy corpus.
// Evaluation labels and case families are excluded from the service binary.
package supportdata

import "embed"

// Files contains the immutable registration consumed by the offline loader.
//
//go:embed policies/*.md policies/manifest.json
var Files embed.FS
