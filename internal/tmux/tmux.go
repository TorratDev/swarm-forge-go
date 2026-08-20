// Package tmux wraps the subset of tmux(1) SwarmForge needs to run one
// session per project, one window per role: session/window lifecycle,
// direct-argv command exec (no shell in the loop -- see internal/launch's
// package comment), the proven 3-step send-keys wake sequence carried over
// from the original bash implementation's handoffd.bb, and window-existence
// polling for exit detection. Requires tmux >= 3.0, for per-pane -e
// environment flags on new-session/new-window.
package tmux

import (
	"bytes"
	"fmt"
	"hash/crc32"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// SessionName is the tmux session name for every project. Kept a fixed
// constant rather than derived from the project name: each project already
// gets its own isolated socket (see SocketPath), so there's no
// cross-project collision risk, and a fixed name keeps every command line
// in this package -- and any error/help text that quotes one -- uniform.
const SessionName = "swarmforge"

// notifySleep1/notifySleep2 are the delays between the three send-keys
// calls in Notify, carried over unchanged from the original bash
// implementation's handoffd.bb, where they were proven reliable at
// compensating for tmux's own asynchronous key-injection queue.
const (
	notifySleep1 = 150 * time.Millisecond
	notifySleep2 = 50 * time.Millisecond
)

// Available reports whether a tmux binary is on PATH.
func Available() bool {
	_, err := exec.LookPath("tmux")
	return err == nil
}

// SocketPath deterministically derives this project's isolated tmux socket
// path from its absolute root, mirroring the original bash implementation's
// crc32(abs-path)-under-/tmp convention: one socket per project, isolated
// from the user's default tmux server and from every other SwarmForge
// project, with no live tmux process required to compute it.
func SocketPath(projectRoot string) (string, error) {
	abs, err := filepath.Abs(projectRoot)
	if err != nil {
		return "", err
	}
	uid := os.Getuid()
	sum := crc32.ChecksumIEEE([]byte(abs))
	dir := filepath.Join(os.TempDir(), fmt.Sprintf("swarmforge-%d", uid))
	return filepath.Join(dir, fmt.Sprintf("%08x.sock", sum)), nil
}

// Client binds every operation to one project's socket + session.
type Client struct {
	Socket  string
	Session string
}

// New builds a Client for a given socket + session.
func New(socket, session string) *Client {
	return &Client{Socket: socket, Session: session}
}

func (c *Client) run(args ...string) ([]byte, error) {
	full := append([]string{"-S", c.Socket}, args...)
	cmd := exec.Command("tmux", full...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("tmux %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// HasSession reports whether the session is currently live.
func (c *Client) HasSession() bool {
	_, err := c.run("has-session", "-t", c.Session)
	return err == nil
}

// NewSession creates the session and its first window, named windowName,
// running argv directly in workdir (argv[0] resolved via PATH, no shell).
// Being the first tmux invocation against a fresh socket, this also starts
// a new tmux server on that socket -- whose global session environment is
// seeded from this Go process's own environment, which is why env only
// needs to carry the deltas (e.g. SWARMFORGE_ROLE, PATH), not a full
// os.Environ() dump.
func (c *Client) NewSession(windowName, workdir string, env []string, argv []string) error {
	// tmux won't create the socket's parent directory itself; 0700 keeps
	// the per-user socket directory private, matching the original bash
	// implementation's isolation intent.
	if err := os.MkdirAll(filepath.Dir(c.Socket), 0o700); err != nil {
		return err
	}
	args := []string{"new-session", "-d", "-s", c.Session, "-n", windowName, "-c", workdir}
	args = append(args, envFlags(env)...)
	args = append(args, "--")
	args = append(args, argv...)
	_, err := c.run(args...)
	return err
}

// NewWindow adds one more role's window to an already-running session, same
// direct-argv-exec semantics as NewSession.
func (c *Client) NewWindow(windowName, workdir string, env []string, argv []string) error {
	args := []string{"new-window", "-t", c.Session, "-n", windowName, "-c", workdir}
	args = append(args, envFlags(env)...)
	args = append(args, "--")
	args = append(args, argv...)
	_, err := c.run(args...)
	return err
}

func envFlags(env []string) []string {
	flags := make([]string, 0, len(env)*2)
	for _, kv := range env {
		flags = append(flags, "-e", kv)
	}
	return flags
}

// DisableRename turns off the session-wide default for automatic window
// renaming, so a running agent CLI's own title-set escape sequences can't
// rename a window out from under our by-name addressing. Call once, right
// after NewSession.
func (c *Client) DisableRename() error {
	_, err := c.run("set-option", "-t", c.Session, "allow-rename", "off")
	return err
}

// ListWindows returns the live window names in this session, in tmux's own
// window-index order. Returns an error if the session doesn't exist.
func (c *Client) ListWindows() ([]string, error) {
	out, err := c.run("list-windows", "-t", c.Session, "-F", "#{window_name}")
	if err != nil {
		return nil, err
	}
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return nil, nil
	}
	return strings.Split(trimmed, "\n"), nil
}

// Notify runs the proven 3-call wake sequence against session:windowName --
// a literal send-keys of message, then a carriage return, then a line feed
// as two separate named-key calls, with sleeps between each carried over
// unchanged from the original implementation (see notifySleep1/2's doc
// comment). Deliberately not re-innovated into a single call: this exact
// sequence was already proven reliable, and re-proving a faster one isn't
// worth the risk.
func (c *Client) Notify(windowName, message string) error {
	target := c.Session + ":" + windowName
	if _, err := c.run("send-keys", "-t", target, "-l", message); err != nil {
		return err
	}
	time.Sleep(notifySleep1)
	if _, err := c.run("send-keys", "-t", target, "C-m"); err != nil {
		return err
	}
	time.Sleep(notifySleep2)
	if _, err := c.run("send-keys", "-t", target, "C-j"); err != nil {
		return err
	}
	return nil
}

// CapturePane returns the current visible content of windowName's pane, as
// plain text -- tmux's own pane-content dump, useful for tests and
// diagnostics to confirm what a window has actually received/rendered.
func (c *Client) CapturePane(windowName string) (string, error) {
	out, err := c.run("capture-pane", "-t", c.Session+":"+windowName, "-p")
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// KillSession tears down the whole session -- tmux sends SIGHUP to every
// pane's process tree, sufficient on its own for reaping everything an
// agent CLI itself spawned, matching what the original bash implementation
// relied on. Tolerates "session not found" as a no-op success, since the
// session may already be gone (e.g. its last window's process exited on its
// own).
func (c *Client) KillSession() error {
	if !c.HasSession() {
		return nil
	}
	_, err := c.run("kill-session", "-t", c.Session)
	return err
}

// AttachCmd returns an *exec.Cmd for `tmux -S socket attach-session -t
// session`, stdio unset -- the caller wires os.Stdin/Stdout/Stderr, since
// attaching needs a real controlling terminal this package shouldn't assume
// ownership of.
func (c *Client) AttachCmd() *exec.Cmd {
	return exec.Command("tmux", "-S", c.Socket, "attach-session", "-t", c.Session)
}
