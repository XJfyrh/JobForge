// Package businessmigrations embeds only the independent business migrations.
package businessmigrations

import "embed"

// Files contains the versioned SQL; the control migrator does not import it.
//
//go:embed *.sql
var Files embed.FS
