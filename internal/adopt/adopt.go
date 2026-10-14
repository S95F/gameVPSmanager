// Package adopt implements gvm adopt: the one-time migration of a live,
// legacy bash-managed game server into gvm management.
//
// The adopt lifecycle has four phases:
//
//   - discover: read-only reconnaissance; produces a DiscoverResult report.
//   - cutover:  stops the legacy unit, writes gvm configs, starts gvm unit.
//   - rollback: re-enables the legacy unit from the pre-written rollback script.
//   - finalize: removes legacy unit files after the 7-day review window.
//
// Security invariant: config.env is always PARSED, never sourced. Command
// substitution ($(...) and backticks), variable interpolation (${VAR}/$VAR),
// and all other shell metacharacters are returned as literal strings.
package adopt
