// Package agenttmux holds the repo-root files the binary ships. go:embed cannot
// reach above a package's own directory, so files that must stay at the repo
// root (where people and tools look for them) are embedded here.
package agenttmux

import _ "embed"

// Skill is SKILL.md, printed by `agent-tmux skill`. A skill manager keeps the
// printed copy, so the installed skill always matches the installed binary.
//
//go:embed SKILL.md
var Skill []byte
