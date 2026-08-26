// Package migrations embeds the numbered SQL files so the binary can migrate
// itself with no files on disk beside it.
//
// This exists because go:embed can only reach files at or below the directory
// of the package that declares it. Keeping the SQL at the repository root, as a
// package containing nothing but this directive, means the layout stays obvious
// to a human reading the tree while the files still travel inside the binary —
// which matters for the scratch-based container image in phase 4, where there is
// no filesystem to read them from.
package migrations

import "embed"

// FS holds every .sql file in this directory.
//
// embed.FS is read-only and safe for concurrent use, so exporting the value
// directly rather than behind an accessor is fine — there is nothing a caller
// can do to corrupt it.
//
//go:embed *.sql
var FS embed.FS
