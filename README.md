# agent-sm

A state machine that takes one GitHub issue to a reviewed branch. The program owns the loop:
each step returns a typed outcome, one table (`next()` in `main.go`) decides the transition, and
agents (`claude -p`) only run inside single steps. Every step is declared once in the `steps`
table in `main.go`: its function, outcomes, role prompt, output schema, allow list and model.

    prepare -> plan -> investigate (one session per unit) -> execute -> check -> review -> publish

Exits: `ready`, `gave-up`, `errored`, `split`, `escalated`, `stopped`.

## Usage

    go build
    agent-sm <repo-dir> <issue-number>          # run one issue; prints the exit, status 0 only for ready
    agent-sm serve [--repo DIR] [issue] [addr]  # live page (default 127.0.0.1:8787); --repo lets it start runs
    agent-sm stats [file]                       # per-state cost, models, tools used, denials
    agent-sm allow [file]                       # allow lists suggested by the stats, as SM_ALLOW_FILE JSON

Sub-issues: on the page, an issue with GitHub sub-issues shows its progress. Selecting it queues
its open sub-issues in its place, one after another; the issue itself never runs. A sub-issue is
skipped when GitHub lists an open issue as blocking it, when its body's Dependencies section names
an open issue, or when it has sub-issues of its own (select that one directly). The command line
still runs exactly the one issue it is given.

The repo's working tree must be clean; the run creates the branch `sm/issue-<n>`. It needs
`claude`, `git` and `gh` on the PATH.

There is no resume: a run always starts at `prepare`. A stopped or failed run leaves its branch
in place; delete it before running the issue again.

## Environment

| Variable | Default | Meaning |
|---|---|---|
| `SM_PUBLISH` | empty | empty: stop at a local staged branch. `pr`: commit, rebase, push, open a draft PR. `merge`: also merge it. |
| `SM_BASE` | `origin/main` | Branch that worktrees start from and publish rebases onto. |
| `SM_BUDGET` | `2.00` | Dollar limit for one agent session. |
| `SM_RUN_BUDGET` | `20` | Dollar limit for the whole run; no step starts once it is reached. `0` turns it off. |
| `SM_TIMEOUT` | `60` | Minutes before an agent session and its child processes are killed. |
| `SM_MODEL_<STATE>` | CLI default | Model for one state, e.g. `SM_MODEL_PLAN=haiku`, `SM_MODEL_REVIEW=opus`. |
| `SM_CHECKS` | cargo fmt check, clippy, test | Check commands, one per line. |
| `SM_FMT` | `cargo fmt --all` | Formatter run before the checks. Not run when `SM_CHECKS` is set and this is not. |
| `SM_ALLOW_FILE` | none | JSON `{"state": "tools"}` that replaces a state's default allow list. |
| `SM_EXTRA_ALLOW` | none | Allow entries added to every agent step. |
| `SM_ADD_DIRS` | none | Comma-separated directories agents may read outside the repo. |
| `SM_NOTES_DIR` | none | Operator notes added to dispatches: `all.md`, or `<role>.md` for one step. |
| `SM_AUDIT` | off | `1`: load every tool and bypass permissions (outward commands stay denied) to measure what steps use. |
| `SM_GHIDRA_COPY` | `none` | Ghidra project copy the agents may use. |
| `SM_EXPORT` | `~/rescue-re/export` | Ghidra export; `prepare` checks its `STAMP` and tells the agents. |
| `SM_EPIC` | none | Epic that a follow-up issue joins (split, or three failed fix rounds). |
| `SM_LINKS` | none | Comma-separated gitignored files to symlink into a worktree (serve). |
| `SM_STATS` | `agent-sm-stats.jsonl` | Where session stats are appended. |
| `SM_SCRATCH_ROOT`, `SM_SCRATCH`, `SM_EVENTS` | `~/.local/state/agent-sm` (or `$XDG_STATE_HOME/agent-sm`), `<root>/<issue>`, `<scratch>/events.jsonl` | Where run data goes. |

A session that hits `SM_TIMEOUT` returns `timeout`, one that hits its budget returns
`over-budget`; both end the run with `gave-up` and are not rerun. A killed session reports no
cost, so the run total leaves it out and the exit event counts it in `sessions_with_unknown_cost`.

## Run data

- `<scratch>/events.jsonl`: one line per step, per agent session and the exit. When a new run of the issue starts, the old log is renamed to `events-<date>-<time>.jsonl`, not deleted.
- The scratch root is durable, so run history survives a reboot. Agents also write probe output (traces, dumps) there; delete an issue's directory when you no longer need it.
- `<scratch>/status.json`: the running session's live state. `<scratch>/run.log`: output of a run started from the page.
- `agent-sm-stats.jsonl` (`SM_STATS`): one line per agent session. It is local output and is gitignored.
- `runs/`: archived attempts kept in the repo as evidence for the issues that cite them. Nothing writes there.

## Platforms

Linux and macOS. The code uses process groups, `sh -c` and symlinks, so it does not build on
Windows; use WSL.

## Tests

    go vet ./... && go test ./...
    SM_SMOKE=1 go test -run Smoke -v    # one real, cheap agent call
