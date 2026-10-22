// Package migrations embeds the ordered SQLite migration files.
package migrations

import "embed"

// FS contains the migration scripts applied by internal/store/sqlite.
//
//go:embed *.sql
var FS embed.FS
