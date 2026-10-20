package migrations

import "embed"

// FS contains the gooseloaded PostgreSQL migrations.
//
//go:embed postgres/*.sql
var FS embed.FS
