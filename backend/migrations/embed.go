// Package migrations embeds the versioned SQL schema migrations.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
