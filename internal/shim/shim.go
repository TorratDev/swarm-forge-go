// Package shim holds the legacy script names SwarmForge still honors on
// PATH -- shared between internal/cli (which dispatches on argv[0] when
// invoked under one of these names) and internal/orchestrator (which
// installs them as symlinks to the running binary) so the two stay in sync
// without either package importing the other.
package shim

// Names maps the legacy script basenames role prompts still invoke to the
// subcommand that now implements them.
var Names = map[string]string{
	"swarm_handoff.sh":     "handoff",
	"ready_for_next.sh":    "ready",
	"done_with_current.sh": "done",
}
