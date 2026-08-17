package store

import "embed"

// Migrations contains the forward-only database history shipped with the binary.
// The pre-release greenfield baseline remains editable until baseline_frozen is true.
//
//go:embed migrations/*.sql
var Migrations embed.FS
