// Package agent provides the dicode task developer skill — a markdown document
// that gives any AI agent the context needed to develop dicode tasks correctly.
//
// The skill is embedded into the binary at compile time and distributed via:
//
//	dicode agent skill show                  # print to stdout
//	dicode agent skill install               # write to ~/.dicode/skill.md
//	dicode agent skill install --claude-code # write to ~/.claude/skills/
package agent

import _ "embed"

//go:embed skill.md
var Skill string
