package orchestrator

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/TorratDev/swarm-forge-go/internal/config"
	"github.com/TorratDev/swarm-forge-go/internal/daemon"
	"github.com/TorratDev/swarm-forge-go/internal/launch"
	"github.com/TorratDev/swarm-forge-go/internal/state"
	"github.com/TorratDev/swarm-forge-go/internal/tmux"
)

const (
	// PollInterval is how often the delivery daemon scans outboxes,
	// matching the original handoffd.bb's 1-second poll. exitPollLoop
	// reuses the same cadence for window-existence polling.
	PollInterval = 1 * time.Second
	// pollCheckInterval is how often the daemon loop checks for shutdown
	// while waiting out PollInterval, keeping shutdown responsive.
	pollCheckInterval = 100 * time.Millisecond

	// StartDelay staggers agent launches to avoid a thundering herd of
	// simultaneous API calls.
	StartDelay = 1500 * time.Millisecond

	pidFileName = "swarmforge.pid"

	// CloseTimeout bounds how long "swarmforge down" waits for the "up"
	// process to exit after SIGTERM before escalating to SIGKILL.
	CloseTimeout = 5 * time.Second

	// wakeMessage is the literal text sent into a recipient's tmux window
	// to wake it up -- deliberately generic (it never names the delivered
	// file), matching swarmforge/handoff-protocol.md's original design:
	// it should only prompt an idle agent to check its durable inbox, not
	// bias it toward one file.
	wakeMessage = "You have new handoff mail. If idle, run ready_for_next.sh."
)

// PIDFilePath is where the live "swarmforge up" process's own PID is
// recorded, so "swarmforge down" can find and signal it from another
// terminal.
func PIDFilePath(projectRoot string) string {
	return filepath.Join(projectRoot, ".swarmforge", pidFileName)
}

// SpawnFunc resolves an agent backend + launch spec into an executable
// name and argv[1:]. The default (see Launch) delegates to
// internal/launch's real per-backend argv builders; tests substitute a
// safe stand-in so they never have to actually exec a real agent CLI.
type SpawnFunc func(agent string, spec launch.Spec) (name string, args []string, err error)

func defaultSpawn(agent string, spec launch.Spec) (string, []string, error) {
	argv, err := launch.BuildArgv(agent, spec)
	if err != nil {
		return "", nil, err
	}
	return argv[0], argv[1:], nil
}

// notifier implements daemon.Notifier via tmux send-keys: a role's name
// doubles as its tmux window name, so no lookup table is needed.
type notifier struct {
	tmux *tmux.Client
}

func (n *notifier) Notify(role string) error {
	return n.tmux.Notify(role, wakeMessage)
}

// Run is one live "swarmforge up" swarm: the tmux session backing every
// role's window, and the handoff-delivery and window-exit-poll daemon
// goroutines.
type Run struct {
	ProjectRoot string
	Project     config.Project
	State       state.State

	tmux      *tmux.Client
	roleNames []string // Project.Roles order, for exitPollLoop's expected set

	notifier   *notifier
	cancel     context.CancelFunc
	daemonDone chan struct{}
	pollDone   chan struct{}

	cleanupRole string
	exited      chan string // role names, as their tmux windows disappear

	shutdownOnce sync.Once
	shutdownCh   chan struct{}
}

// Exited is signaled with a role's name each time that role's tmux window
// disappears (its agent process exited: tmux's default remain-on-exit off
// closes a window when its pane's process exits). The cleanup role's exit
// is the signal a caller should treat as "the whole swarm should now shut
// down" -- carried over unchanged from the original's
// index-0-role-exit-tears-down-the-swarm rule.
func (r *Run) Exited() <-chan string {
	return r.exited
}

// CleanupRole is the role whose exit should trigger full swarm teardown.
func (r *Run) CleanupRole() string {
	return r.cleanupRole
}

// AttachCmd returns an *exec.Cmd that attaches a terminal to this run's
// tmux session; the caller wires stdio and calls Run().
func (r *Run) AttachCmd() *exec.Cmd {
	return r.tmux.AttachCmd()
}

// ShutdownCh is closed exactly once Shutdown has run to completion,
// regardless of which trigger called it -- "swarmforge up"'s attach/wait
// loop selects on this to distinguish a plain tmux detach (session still
// alive, keep the daemon running) from a real shutdown.
func (r *Run) ShutdownCh() <-chan struct{} {
	return r.shutdownCh
}

// Launch creates the project's tmux session with one window per configured
// role (staggered by startDelay), each running its agent CLI directly (no
// shell, no PTY-wrapping -- tmux execs the argv itself), starts the
// handoff-delivery daemon and the window-exit-poll loop as goroutines, and
// writes a PID file recording this process, so "swarmforge down" can find
// and signal it from another terminal.
func Launch(projectRoot string, proj config.Project, st state.State, startDelay time.Duration, spawn SpawnFunc) (*Run, error) {
	if spawn == nil {
		spawn = defaultSpawn
	}
	cleanupRole, ok := proj.CleanupRole()
	if !ok {
		return nil, fmt.Errorf("project has no roles configured")
	}
	if st.TmuxSocket == "" || st.TmuxSession == "" {
		return nil, fmt.Errorf("state missing tmux socket/session (run Prepare first)")
	}

	pidPath := PIDFilePath(projectRoot)
	if err := os.MkdirAll(filepath.Dir(pidPath), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o644); err != nil {
		return nil, err
	}

	client := tmux.New(st.TmuxSocket, st.TmuxSession)
	run := &Run{
		ProjectRoot: projectRoot,
		Project:     proj,
		State:       st,
		tmux:        client,
		cleanupRole: cleanupRole.Name,
		exited:      make(chan string, len(proj.Roles)),
		shutdownCh:  make(chan struct{}),
	}
	run.notifier = &notifier{tmux: client}

	stateDir := filepath.Join(projectRoot, ".swarmforge")
	binDir := BinDir(projectRoot)

	// tmux's per-pane "-e PATH=..." is silently dropped for the spawned
	// process's actual environment (confirmed against tmux 3.4: it
	// registers in the session's environment table -- visible via
	// "show-environment" -- but doesn't reach the pane's own environ).
	// What does work: each "tmux new-session"/"new-window" invocation
	// inherits *its own calling client's* environment for the pane it
	// creates. Since every tmux call below runs as a child of this one
	// long-lived Go process, prepending binDir to this process's own PATH
	// once, up front, makes every window -- not just the first -- launch
	// with binDir on PATH.
	if err := os.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH")); err != nil {
		run.shutdownLaunched()
		return nil, err
	}

	for i, r := range proj.Roles {
		if i > 0 && startDelay > 0 {
			time.Sleep(startDelay)
		}
		role, ok := st.RoleByName(r.Name)
		if !ok {
			run.shutdownLaunched()
			return nil, fmt.Errorf("role %q missing from state", r.Name)
		}

		promptFile, err := launch.InstructionFile(stateDir, r.Name)
		if err != nil {
			run.shutdownLaunched()
			return nil, err
		}
		promptText, err := os.ReadFile(promptFile)
		if err != nil {
			run.shutdownLaunched()
			return nil, err
		}

		name, args, err := spawn(r.Agent, launch.Spec{
			Role: r.Name, DisplayName: role.DisplayName, WorktreePath: role.WorktreePath,
			ExtraArgs: r.ExtraArgs, PromptFile: promptFile, PromptText: string(promptText),
		})
		if err != nil {
			run.shutdownLaunched()
			return nil, fmt.Errorf("building launch command for role %q: %w", r.Name, err)
		}

		argv := append([]string{name}, args...)
		env := []string{"SWARMFORGE_ROLE=" + r.Name}

		if i == 0 {
			if err := client.NewSession(r.Name, role.WorktreePath, env, argv); err != nil {
				run.shutdownLaunched()
				return nil, fmt.Errorf("launching role %q: %w", r.Name, err)
			}
			if err := client.DisableRename(); err != nil {
				run.shutdownLaunched()
				return nil, fmt.Errorf("disabling window auto-rename: %w", err)
			}
		} else {
			if err := client.NewWindow(r.Name, role.WorktreePath, env, argv); err != nil {
				run.shutdownLaunched()
				return nil, fmt.Errorf("launching role %q: %w", r.Name, err)
			}
		}

		run.roleNames = append(run.roleNames, r.Name)
	}

	ctx, cancel := context.WithCancel(context.Background())
	run.cancel = cancel
	run.daemonDone = make(chan struct{})
	run.pollDone = make(chan struct{})
	go run.daemonLoop(ctx)
	go run.exitPollLoop(ctx)

	return run, nil
}

// shutdownLaunched tears down whatever the tmux session already has, for
// cleanup when Launch fails partway through.
func (r *Run) shutdownLaunched() {
	_ = r.tmux.KillSession()
	_ = os.Remove(PIDFilePath(r.ProjectRoot))
}

func (r *Run) daemonLoop(ctx context.Context) {
	defer close(r.daemonDone)
	for {
		daemon.PollOnce(r.State, r.notifier, time.Now())
		if waitOrDone(ctx, PollInterval, pollCheckInterval) {
			return
		}
	}
}

// exitPollLoop polls the session's live window names against the expected
// role set and reports any that have disappeared on r.exited -- the
// tmux-window replacement for the old per-agent Wait() goroutines, since a
// tmux window isn't a Go value we can block on directly.
func (r *Run) exitPollLoop(ctx context.Context) {
	defer close(r.pollDone)
	expected := make(map[string]bool, len(r.roleNames))
	for _, n := range r.roleNames {
		expected[n] = true
	}
	for {
		live, err := r.tmux.ListWindows()
		if err == nil {
			liveSet := make(map[string]bool, len(live))
			for _, n := range live {
				liveSet[n] = true
			}
			for name := range expected {
				if !liveSet[name] {
					delete(expected, name)
					r.exited <- name
				}
			}
		}
		// else: session is gone entirely; the KillSession/Shutdown path
		// already accounts for that -- nothing more to report here.
		if waitOrDone(ctx, PollInterval, pollCheckInterval) {
			return
		}
	}
}

// waitOrDone sleeps for total, checking ctx.Done() every check interval,
// and returns true if ctx was canceled before total elapsed.
func waitOrDone(ctx context.Context, total, check time.Duration) bool {
	deadline := time.Now().Add(total)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return true
		case <-time.After(check):
		}
	}
	return false
}

// Shutdown stops the delivery and exit-poll daemons, kills the tmux
// session (which sends SIGHUP to every window's process tree -- sufficient
// on its own, no per-process signal escalation needed), and removes the
// PID file. It's the single code path every shutdown trigger funnels
// through -- "swarmforge down" (via a SIGTERM this process's signal
// handler routes here), the cleanup role's own tmux window exiting, and a
// top-level error-path safety net can all call it; a sync.Once makes that
// safe to do from more than one of them without double-closing anything.
func (r *Run) Shutdown() {
	r.shutdownOnce.Do(func() {
		if r.cancel != nil {
			r.cancel()
			<-r.daemonDone
			<-r.pollDone
		}
		_ = r.tmux.KillSession()
		_ = os.Remove(PIDFilePath(r.ProjectRoot))
		close(r.shutdownCh)
	})
}

// ReadPID reads the PID recorded by a live "swarmforge up" process.
func ReadPID(projectRoot string) (int, error) {
	b, err := os.ReadFile(PIDFilePath(projectRoot))
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0, fmt.Errorf("malformed PID file: %w", err)
	}
	return pid, nil
}
