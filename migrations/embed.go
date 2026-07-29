// Package migrations embeds the SQL migration files into the binary for
// automatic schema management on startup. The source-of-truth SQL files
// live in this directory and are also used by external tooling (sqlfluff,
// golang-migrate CLI).
package migrations

import "embed"

// FS contains all migration SQL files embedded at compile time.
// Files follow the naming convention: NNNN_description.{up,down}.sql.
//
//go:embed *.sql
var FS embed.FS
