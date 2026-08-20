package cli

import (
	"fmt"
	"os"

	"github.com/TorratDev/swarm-forge-go/internal/state"
	"github.com/TorratDev/swarm-forge-go/internal/tmux"
)

// RunAttach attaches the operator's terminal to a running swarm's tmux
// session -- the explicit way back in after detaching (tmux's prefix+d)
// from "swarmforge up".
func RunAttach(env Env, projectRoot string) int {
	st, err := state.Load(projectRoot)
	if err != nil {
		fmt.Fprintln(env.Stderr, "swarmforge attach: no swarm found:", err)
		return 1
	}
	if st.TmuxSocket == "" || st.TmuxSession == "" {
		fmt.Fprintln(env.Stderr, "swarmforge attach: state.json has no tmux session recorded; run \"swarmforge up\" first")
		return 1
	}

	cmd := tmux.New(st.TmuxSocket, st.TmuxSession).AttachCmd()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintln(env.Stderr, "swarmforge attach:", err)
		return 1
	}
	return 0
}
