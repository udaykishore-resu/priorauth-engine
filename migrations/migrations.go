// Package migrations embeds the numbered SQL migrations applied by the
// Postgres adapter at start-up (and by `make migrate`).
package migrations

import "embed"

// FS holds every *.sql file in lexical (= numeric) order.
//
//go:embed *.sql
var FS embed.FS
