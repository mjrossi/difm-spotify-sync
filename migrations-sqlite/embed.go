// Package migrations bundles the SQLite migration files as an embedded
// filesystem so callers (cmd/difmsync, tests) can apply them without
// needing the on-disk files at runtime.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
