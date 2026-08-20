package cli

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/TorratDev/swarm-forge-go/internal/orchestrator"
	"github.com/TorratDev/swarm-forge-go/internal/tmux"
)

// RunUp prepares a swarm (validates swarmforge.yaml, creates git
// worktrees, patches Claude Code's trust dialog, writes state.json),
// launches every configured role's agent process in its own tmux window,
// and attaches the operator's terminal to that session.
//
// Detaching (tmux's prefix+d) does not stop the swarm: the handoff daemon
// is an in-process goroutine, so this process must keep running for the
// swarm's whole lifetime, independent of whether anyone is attached to
// look at it -- only an explicit "swarmforge down", a SIGTERM, or the
// cleanup role's own tmux window exiting tears it down. "swarmforge
// attach" is the explicit way back in after a detach.
//
// This function fundamentally needs a real terminal (tmux attach-session
// reads/writes os.Stdin/os.Stdout directly for the interactive session),
// so unlike the other command handlers it isn't practical to drive through
// Env's io.Writer abstraction end-to-end -- Prepare/Launch (the part that
// can go wrong non-interactively) are covered by internal/orchestrator's
// own tests instead.
func RunUp(env Env) int {
	if !tmux.Available() {
		fmt.Fprintln(env.Stderr, "swarmforge up: tmux is required but not found on PATH (tmux >= 3.0)")
		return 1
	}

	result, err := orchestrator.Prepare(env.Cwd)
	if err != nil {
		fmt.Fprintln(env.Stderr, "swarmforge up:", err)
		return 1
	}

	run, err := orchestrator.Launch(env.Cwd, result.Project, result.State, orchestrator.StartDelay, nil)
	if err != nil {
		fmt.Fprintln(env.Stderr, "swarmforge up:", err)
		return 1
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		select {
		case <-sigCh:
			run.Shutdown()
		case <-run.ShutdownCh():
		}
	}()

	go func() {
		for role := range run.Exited() {
			if role == run.CleanupRole() {
				run.Shutdown()
				return
			}
		}
	}()

	attachErrCh := make(chan error, 1)
	go func() {
		cmd := run.AttachCmd()
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		attachErrCh <- cmd.Run()
	}()

	for {
		select {
		case <-attachErrCh:
			select {
			case <-run.ShutdownCh():
				return 0
			default:
				fmt.Fprintf(env.Stdout, "Detached. Swarm still running.\nReattach: swarmforge attach\n  or: tmux -S %s attach -t %s\nStop it:  swarmforge down\n",
					result.State.TmuxSocket, result.State.TmuxSession)
				<-run.ShutdownCh()
				return 0
			}
		case <-run.ShutdownCh():
			return 0
		}
	}
}
