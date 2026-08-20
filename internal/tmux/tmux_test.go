package tmux

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func requireTmux(t *testing.T) {
	t.Helper()
	if !Available() {
		t.Skip("tmux not available")
	}
}

func requirePython(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
}

func testClient(t *testing.T) *Client {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "test.sock")
	c := New(socket, "swarmforge-test")
	t.Cleanup(func() { _ = c.KillSession() })
	return c
}

func idleArgv() []string {
	return []string{"python3", "-c", "import time\nwhile True:\n    time.sleep(1)\n"}
}

func TestNewSessionAndHasSession(t *testing.T) {
	requireTmux(t)
	requirePython(t)
	c := testClient(t)

	if c.HasSession() {
		t.Fatalf("HasSession() = true before NewSession")
	}
	if err := c.NewSession("coder", t.TempDir(), nil, idleArgv()); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if !c.HasSession() {
		t.Fatalf("HasSession() = false after NewSession")
	}
}

func TestNewWindowAndListWindows(t *testing.T) {
	requireTmux(t)
	requirePython(t)
	c := testClient(t)

	if err := c.NewSession("coder", t.TempDir(), nil, idleArgv()); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if err := c.NewWindow("cleaner", t.TempDir(), nil, idleArgv()); err != nil {
		t.Fatalf("NewWindow: %v", err)
	}

	windows, err := c.ListWindows()
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
}

func TestNotifyDeliversWakeMessage(t *testing.T) {
	requireTmux(t)
	requirePython(t)
	c := testClient(t)

	if err := c.NewSession("cleaner", t.TempDir(), nil, idleArgv()); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if err := c.Notify("cleaner", "You have new handoff mail."); err != nil {
		t.Fatalf("Notify: %v", err)
	}

	out, err := c.CapturePane("cleaner")
	if err != nil {
		t.Fatalf("CapturePane: %v", err)
	}
	if !strings.Contains(out, "You have new handoff mail.") {
		t.Fatalf("captured pane content does not contain the wake message:\n%s", out)
	}
}

func TestEnvFlagsSetPerWindowEnvironment(t *testing.T) {
	requireTmux(t)
	requirePython(t)
	c := testClient(t)

	dir := t.TempDir()
	if err := c.NewSession("coder", dir,
		[]string{"SWARMFORGE_ROLE=coder"},
		[]string{"python3", "-c", "import os,time\nopen('marker','w').write(os.environ.get('SWARMFORGE_ROLE',''))\nwhile True:\n    time.sleep(1)\n"},
	); err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	markerPath := filepath.Join(dir, "marker")
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(markerPath); err == nil {
			if string(b) != "coder" {
				t.Fatalf("marker file = %q, want %q", b, "coder")
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("marker file never appeared at %s", markerPath)
}

func TestKillSessionIsIdempotent(t *testing.T) {
	requireTmux(t)
	requirePython(t)
	c := testClient(t)

	if err := c.NewSession("coder", t.TempDir(), nil, idleArgv()); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if err := c.KillSession(); err != nil {
		t.Fatalf("first KillSession: %v", err)
	}
	if c.HasSession() {
		t.Fatalf("HasSession() = true after KillSession")
	}
	if err := c.KillSession(); err != nil {
		t.Fatalf("second KillSession (already gone) should be a no-op, got: %v", err)
	}
}

func TestSocketPathIsDeterministicAndIsolated(t *testing.T) {
	a, err := SocketPath("/some/project")
	if err != nil {
		t.Fatal(err)
	}
	b, err := SocketPath("/some/project")
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("SocketPath not deterministic: %q != %q", a, b)
	}
	other, err := SocketPath("/some/other-project")
	if err != nil {
		t.Fatal(err)
	}
	if a == other {
		t.Fatalf("SocketPath collided for different project roots: %q", a)
	}
}
