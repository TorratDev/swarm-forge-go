package orchestrator

import (
	"os"
	"path/filepath"

	"github.com/TorratDev/swarm-forge-go/internal/shim"
)

// BinDir is where InstallShims places the legacy-script symlinks, and
// where Launch points every role's PATH so role prompts invoking
// "swarm_handoff.sh" etc. resolve here first.
func BinDir(projectRoot string) string {
	return filepath.Join(projectRoot, ".swarmforge", "bin")
}

// InstallShims (re)creates binDir/<legacy-name> symlinks pointing at the
// currently running swarmforge binary, for every name in shim.Names, so
// role prompts invoking "swarm_handoff.sh" etc. resolve via PATH to this
// binary, which dispatches on argv[0] (see internal/cli.DispatchShim)
// before falling through to normal subcommand parsing.
func InstallShims(binDir string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return err
	}
	for name := range shim.Names {
		link := filepath.Join(binDir, name)
		// Idempotent: drop whatever's already there -- a symlink from a
		// prior binary location, or nothing -- before relinking.
		if err := os.Remove(link); err != nil && !os.IsNotExist(err) {
			return err
		}
		if err := os.Symlink(exe, link); err != nil {
			return err
		}
	}
	return nil
}
