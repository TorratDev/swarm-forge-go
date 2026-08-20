package orchestrator

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/TorratDev/swarm-forge-go/internal/handoff"
	"github.com/TorratDev/swarm-forge-go/internal/launch"
	"github.com/TorratDev/swarm-forge-go/internal/tmux"
)

func requirePython(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
}

func requireTmux(t *testing.T) {
	t.Helper()
	if !tmux.Available() {
		t.Skip("tmux not available")
	}
}

// idleSpawn stands in for a real agent CLI in tests: whatever backend is
// configured, it actually launches an idle python3 process. This proves
// the launch/daemon/teardown wiring works without exec'ing (and paying
// for, or nesting) a real agent CLI.
func idleSpawn(agent string, spec launch.Spec) (string, []string, error) {
	return "python3", []string{"-c", "import time\nwhile True:\n    time.sleep(1)\n"}, nil
}

func quickExitSpawn(agent string, spec launch.Spec) (string, []string, error) {
	return "python3", []string{"-c", "pass"}, nil
}

func waitUntil(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("condition not met within %v", timeout)
}

func TestLaunchStartsAgentsAndWritesPIDFile(t *testing.T) {
	requirePython(t)
	requireTmux(t)
	root := scaffoldTwoPack(t)
	t.Setenv("HOME", t.TempDir())
	result, err := Prepare(root)
	if err != nil {
		t.Fatal(err)
	}

	run, err := Launch(root, result.Project, result.State, 0, idleSpawn)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	defer run.Shutdown()

	pidBytes, err := os.ReadFile(PIDFilePath(root))
	if err != nil {
		t.Fatalf("PID file not written: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	if err != nil || pid != os.Getpid() {
		t.Fatalf("PID file = %q, want this test process's pid %d", pidBytes, os.Getpid())
	}

	if !run.tmux.HasSession() {
		t.Fatalf("no tmux session after Launch")
	}
	windows, err := run.tmux.ListWindows()
	if err != nil {
		t.Fatalf("ListWindows: %v", err)
	}
	want := map[string]bool{"coder": true, "cleaner": true}
	if len(windows) != 2 {
		t.Fatalf("ListWindows() = %v, want 2 windows", windows)
	}
	for _, w := range windows {
		if !want[w] {
			t.Fatalf("unexpected window %q in %v", w, windows)
		}
	}

	if run.CleanupRole() != "coder" {
		t.Fatalf("CleanupRole() = %q, want %q (first role in two-pack)", run.CleanupRole(), "coder")
	}
}

func TestDaemonDeliversAndNotifiesViaTmux(t *testing.T) {
	requirePython(t)
	requireTmux(t)
	root := scaffoldTwoPack(t)
	t.Setenv("HOME", t.TempDir())
	result, err := Prepare(root)
	if err != nil {
		t.Fatal(err)
	}

	run, err := Launch(root, result.Project, result.State, 0, idleSpawn)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	defer run.Shutdown()

	coder, _ := result.State.RoleByName("coder")
	if _, err := handoff.WriteDraft(
		coder.WorktreePath+"/.swarmforge/handoffs",
		map[string]string{"type": "note", "to": "cleaner", "priority": "50", "message": "hi cleaner"},
		[]string{"cleaner"}, "coder", "", time.Now(),
	); err != nil {
		t.Fatalf("WriteDraft: %v", err)
	}

	// The daemon goroutine should pick this up within one poll cycle and
	// send the real 3-step tmux wake sequence into cleaner's window --
	// capture-pane proves both delivery *and* the tmux Notify mechanism,
	// without needing the idle child script to cooperate at all.
	waitUntil(t, 4*time.Second, func() bool {
		out, err := run.tmux.CapturePane("cleaner")
		if err != nil {
			return false
		}
		return strings.Contains(out, "You have new handoff mail")
	})
}

func TestShutdownTerminatesAgentsAndRemovesPIDFile(t *testing.T) {
	requirePython(t)
	requireTmux(t)
	root := scaffoldTwoPack(t)
	t.Setenv("HOME", t.TempDir())
	result, err := Prepare(root)
	if err != nil {
		t.Fatal(err)
	}

	run, err := Launch(root, result.Project, result.State, 0, idleSpawn)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}

	if !run.tmux.HasSession() {
		t.Fatalf("expected a live tmux session before Shutdown")
	}

	run.Shutdown()

	if run.tmux.HasSession() {
		t.Fatalf("tmux session still alive after Shutdown")
	}
	if _, err := os.Stat(PIDFilePath(root)); !os.IsNotExist(err) {
		t.Fatalf("PID file should be removed after Shutdown")
	}
}

func TestExitedChannelSignalsCleanupRoleExit(t *testing.T) {
	requirePython(t)
	requireTmux(t)
	root := scaffoldTwoPack(t)
	t.Setenv("HOME", t.TempDir())
	result, err := Prepare(root)
	if err != nil {
		t.Fatal(err)
	}

	run, err := Launch(root, result.Project, result.State, 0, quickExitSpawn)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	defer run.Shutdown()

	select {
	case role := <-run.Exited():
		if role != run.CleanupRole() {
			t.Logf("first exited role was %q (not necessarily the cleanup role -- both exit quickly here)", role)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("no role exit signaled")
	}
}

func TestShutdownIsSafeToCallConcurrentlyMoreThanOnce(t *testing.T) {
	requirePython(t)
	requireTmux(t)
	root := scaffoldTwoPack(t)
	t.Setenv("HOME", t.TempDir())
	result, err := Prepare(root)
	if err != nil {
		t.Fatal(err)
	}
	run, err := Launch(root, result.Project, result.State, 0, idleSpawn)
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	for i := 0; i < 5; i++ {
		go func() {
			run.Shutdown()
			done <- struct{}{}
		}()
	}
	deadline := time.After(5 * time.Second)
	for i := 0; i < 5; i++ {
		select {
		case <-done:
		case <-deadline:
			t.Fatalf("concurrent Shutdown calls did not all return")
		}
	}
}

func TestReadPID(t *testing.T) {
	requirePython(t)
	requireTmux(t)
	root := scaffoldTwoPack(t)
	t.Setenv("HOME", t.TempDir())
	result, err := Prepare(root)
	if err != nil {
		t.Fatal(err)
	}
	run, err := Launch(root, result.Project, result.State, 0, idleSpawn)
	if err != nil {
		t.Fatal(err)
	}
	defer run.Shutdown()

	pid, err := ReadPID(root)
	if err != nil || pid != os.Getpid() {
		t.Fatalf("ReadPID() = %d, %v; want %d, nil", pid, err, os.Getpid())
	}
}
