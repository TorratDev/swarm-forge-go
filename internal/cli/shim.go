package cli

import (
	"path/filepath"

	"github.com/TorratDev/swarm-forge-go/internal/shim"
)

// DispatchShim checks whether args[0]'s basename is one of the legacy
// script names installed as symlinks to this binary (see
// orchestrator.InstallShims) and, if so, runs the matching command
// directly -- before cobra's normal flag/subcommand parsing ever runs.
// This lets existing role prompts keep invoking "ready_for_next.sh" etc.
// unmodified.
func DispatchShim(args []string, env Env) (code int, handled bool) {
	if len(args) == 0 {
		return 0, false
	}
	name, ok := shim.Names[filepath.Base(args[0])]
	if !ok {
		return 0, false
	}
	rest := args[1:]
	switch name {
	case "handoff":
		return RunHandoff(env, rest), true
	case "ready":
		return RunReady(env), true
	case "done":
		return RunDone(env), true
	}
	return 0, false
}
