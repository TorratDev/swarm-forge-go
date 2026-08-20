# SwarmForge

**A single-binary agent orchestration platform that turns swarms of AI agents into reliable, professional software engineers.**

SwarmForge coordinates several AI coding-agent CLIs (`claude`, `codex`, `copilot`, `grok`) working in parallel on the same project: each configured role gets its own git worktree and its own real tmux window, and roles hand off work to each other through a durable, file-based message queue.

This is a from-scratch Go rewrite of the original Babashka/shell implementation. There's no more `./swarm` curl+tar bootstrap and no OS-specific terminal-emulator adapters — one `swarmforge` binary does everything, and each project gets its own isolated tmux session (its own socket, so it never collides with your own tmux) with one window per configured role.

## Prerequisites

- `git`
- `tmux` >= 3.0
- Go 1.24+ (only to build; the built binary has no further runtime dependency beyond `tmux` and the agent CLIs below)
- At least one configured agent backend: `claude`, `codex`, `copilot`, or `grok`

## Install

```sh
go install github.com/TorratDev/swarm-forge-go/cmd/swarmforge@main
```

or build from a checkout:

```sh
go build -o swarmforge ./cmd/swarmforge
```

or download a prebuilt binary from the [Releases](https://github.com/TorratDev/swarm-forge-go/releases) page.

## Getting Started

Scaffold a new project from one of the built-in packs (see [Packs](#packs) below):

```sh
swarmforge init --pack two-pack path/to/project
cd path/to/project
swarmforge up
```

`init` writes `swarmforge.yaml` plus `swarmforge/roles/*.prompt` and `swarmforge/constitution/` into the target directory — plain files you can edit afterward, generated once and never overwritten. `up` then validates that config, creates a git worktree per role, pre-accepts Claude Code's workspace-trust dialog for `claude` roles, creates the project's tmux session with one window per role running that role's agent CLI directly, and attaches your terminal to it.

To stop a swarm from another terminal:

```sh
swarmforge down path/to/project   # defaults to the current directory
```

### Detaching and reattaching

This is a real tmux session, so native tmux keys work as usual: `prefix+w` lists/switches windows, `prefix+<n>` jumps to window *n*, `prefix+d` detaches.

Detaching does **not** stop the swarm — the handoff daemon runs inside the `swarmforge up` process itself, so that process stays alive (and keeps delivering handoffs) for the swarm's whole lifetime, whether or not anyone is attached to look at it. After detaching:

```sh
swarmforge attach path/to/project   # reattach; defaults to the current directory
```

Only `swarmforge down`, a signal to the `swarmforge up` process, or the **first role listed** in `swarmforge.yaml` (the "cleanup role") exiting on its own tears down the entire swarm.

## Packs

A pack is a declarative role topology, embedded in the `swarmforge` binary and materialized by `init`. Three ship today:

| Pack | Roles | Flow |
|---|---|---|
| `two-pack` | `coder`, `cleaner` | `coder` → `cleaner` → `coder` — a quick implement/refine loop, no specification or QA overhead |
| `four-pack` | `specifier`, `coder`, `refactorer`, `architect` | `specifier` → `coder` → `refactorer` → `architect` → `specifier` — Gherkin specification plus one architecture/hardening pass |
| `six-pack` | `specifier`, `coder`, `cleaner`, `architect`, `hardender`, `QA` | `specifier` → `coder` → `cleaner` → `architect` → `hardender` → `QA` → completion — every quality gate as its own role |

List and lint the embedded packs:

```sh
swarmforge pack list
swarmforge pack lint          # lints every pack
swarmforge pack lint six-pack # lints just one
```

## The `swarmforge.yaml` File

`init` generates this from a pack; `up` reads whatever is on disk, so hand edits stick. It's the data-driven replacement for the old `window <role> <agent> <worktree> [mode] [args]` config-file DSL:

```yaml
name: two-pack
roles:
  - name: coder
    agent: claude
    worktree: master
    receive_mode: task
    extra_args: ["--model", "haiku"]
  - name: cleaner
    agent: claude
    worktree: cleaner
    receive_mode: batch
    extra_args: ["--model", "sonnet"]
```

- `name` maps to `swarmforge/roles/<name>.prompt`; must be unique and must not contain `_`.
- `agent` is one of `claude`, `codex`, `copilot`, `grok`.
- `worktree` is `master` (or `none`) to run in the main working directory, or any other unique name to get `.worktrees/<name>` on branch `swarmforge-<name>`.
- `receive_mode` is `task` (default) or `batch`. `batch` roles consume every currently queued equal-priority handoff as one batch instead of one task at a time.
- `extra_args` are passed straight through to the agent CLI's argv.
- The **first role in the list** is the cleanup role (see [Detaching and reattaching](#detaching-and-reattaching)).

### Permission mode for `claude` and `grok` roles

SwarmForge auto-injects a permission-mode flag so an unattended agent doesn't stall waiting for approval. By default it injects `--permission-mode acceptEdits` (auto-approves file edits, still stops on Bash/tool calls). To let a role auto-approve everything, add `--yolo`, `--always-approve`, or `--permission-mode bypassPermissions` to that role's `extra_args`:

```yaml
  - name: coder
    agent: claude
    worktree: coder
    extra_args: ["--yolo"]
```

`bypassPermissions` is opt-in per role — never the default.

## Handoff Protocol

Agents don't message each other directly. Each role's worktree gets a `.swarmforge/handoffs/` directory (`outbox`, `sent`, `failed`, `inbox/{new,in_process,completed}`), and while a swarm is running, a delivery goroutine inside `swarmforge up` polls every role's outbox, copies validated handoffs into each recipient's inbox, and wakes the recipient with a `tmux send-keys` message into its window.

Agents interact with this queue through three commands (also installed on `PATH` under their original script names — `swarm_handoff.sh`, `ready_for_next.sh`, `done_with_current.sh` — so existing role prompts work unmodified):

- `swarm_handoff.sh <draft-file>` validates a draft and queues it into the sender's outbox.
- `ready_for_next.sh` accepts the next task or batch, per the role's configured receive mode.
- `done_with_current.sh` completes the current task or batch, then immediately reports the next one.

A draft is headers only; the command generates the delivered payload. Two message types:

```text
type: git_handoff
to: <role>[,<role>...]
priority: NN
task: <short-stable-task-name>
commit: <10-character-commit-abbrev>
```

```text
type: note
to: <role>[,<role>...]
priority: NN
message: <one line, max 80 chars>
```

`commit` must resolve to exactly one real commit object; `swarm_handoff.sh` canonicalizes it before queuing. `priority` is two digits, lower delivered first; handoff filenames (`<priority>_<timestamp>_<sequence>_from_<sender>_to_<recipients>.handoff`) are constructed to sort lexicographically in exactly that delivery order.

Recipients run `ready_for_next.sh` when notified or on restart. `NO_TASK` means stop waiting; `TASK: <path>` means treat the printed `PAYLOAD` as the task; `BATCH: <path>` means process each printed `BATCH_ITEM` in order. `done_with_current.sh` after finishing immediately reports the next task/batch the same way. Agents should never hand-edit, merge, stage, or commit anything under `.swarmforge/`.

## Constitution Structure

```text
swarmforge/
  roles/
    <role>.prompt
  constitution.prompt
  constitution/
    articles/
      engineering.prompt   # shared base article
      handoffs.prompt      # shared base article
      workflow.prompt      # shared base article
      project.prompt       # pack-specific overlay
      ...
```

`constitution.prompt` tells every agent to read every file under `constitution/articles/`. `engineering.prompt`, `handoffs.prompt`, and `workflow.prompt` are shared base articles every pack includes by default (`swarmforge pack lint` fails a pack that drops `handoffs` without an explicit replacement). Packs add their own overlay files — `project.prompt` describes the pack's role topology; `six-pack` additionally ships `local-engineering.prompt` and `local-workflow.prompt` as small additive specializations that sit alongside the base articles rather than replacing them.

## Development

```sh
go build ./...
go test ./...
go test -race ./...
```

`internal/tmux` and the tmux-backed `internal/orchestrator` tests skip themselves if `tmux` isn't on `PATH`.

Package layout:

```text
cmd/swarmforge/       entry point; argv0 dispatch for the legacy script names
internal/
  cli/                command handlers (init, up, down, attach, handoff, ready, done, pack)
  config/              swarmforge.yaml schema + validation
  state/               .swarmforge/state.json + project-root discovery
  handoff/             header parse/serialize, filenames, draft validation, queue state machine
  daemon/               outbox -> inbox delivery poll loop
  gitutil/               git plumbing: worktrees, commit canonicalization
  trust/                 ~/.claude.json trust-dialog patcher
  launch/                 per-backend argv builders
  tmux/                   tmux(1) session/window lifecycle, send-keys, exit polling
  orchestrator/           startup sequencing, live swarm (launch/daemon/teardown)
  pack/                   pack schema, embedded pack definitions, generator
```
