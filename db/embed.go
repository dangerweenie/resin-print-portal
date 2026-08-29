// Package db embeds the SQL migration files so they ship inside the compiled
// binary and the `portal migrate` subcommand needs nothing on disk.
package db

import "embed"

//go:embed migrations/*.sql
var Migrations embed.FS
