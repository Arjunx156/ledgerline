// Package migrations embeds the SQL migrations so the binary can migrate a
// database without the migrate CLI installed.
package migrations

import "embed"

// FS holds every NNNN_name.{up,down}.sql file.
//
//go:embed *.sql
var FS embed.FS
