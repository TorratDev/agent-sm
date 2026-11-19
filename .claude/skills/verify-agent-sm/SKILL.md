---
name: verify-agent-sm
description: Launch an isolated agent-sm web page (the `serve` UI at 127.0.0.1) with a seeded archived run, drive it through its HTTP API and a browser, and capture proof. Use to confirm a change to agent-sm works in the real app (page, run view, repository picker, issue queue, `stats`/`allow` CLI), not only in `go test`.
---

# Verify agent-sm

agent-sm is one Go binary. Its primary user surface is the local web page served by
`agent-sm serve` (one HTML file, `web/index.html`, polling a JSON API in `serve.go`). Secondary
surfaces: the one-shot CLI (`agent-sm <repo> <issue>`, `stats`, `allow`, `issues`, `reconcile`).

Everything below goes through one helper, run from the repo root:

    .claude/skills/verify-agent-sm/verify.sh launch [attempt1|attempt2|attempt3|attempt4]
    .claude/skills/verify-agent-sm/verify.sh doctor
    .claude/skills/verify-agent-sm/verify.sh api METHOD PATH [JSON]
    .claude/skills/verify-agent-sm/verify.sh evidence
    .claude/skills/verify-agent-sm/verify.sh stop

Read [features/README.md](features/README.md) before driving; each feature file is a recipe.

## Launch

    .claude/skills/verify-agent-sm/verify.sh launch

Needs `go`, `git`, `curl` on PATH. It builds the current checkout to a scratch binary (the
checked-in-ignored `./agent-sm` is not touched), seeds one archived run of issue 359 from
`runs/359/` (default `attempt3`, which ended `ready`), and starts
`agent-sm serve 127.0.0.1:8799` from the repo root with `SM_SCRATCH_ROOT` and `SM_STATS` pointed
at scratch. It starts from an empty data dir every time; there are no migrations.

Ready is the line `ready: http://127.0.0.1:8799  evidence: <dir>` (exit 0). The helper prints it
only once `/api/state` returns the seeded run, not merely when the port answers. On failure it
says so and points at `server.log`.

- Port: `VERIFY_PORT` (default 8799; the user's own instance defaults to 8787, never use that).
- State: `VERIFY_HOME` (default `~/.local/state/agent-sm-verify`): `instance/` is scratch,
  `evidence/<run-id>/` is proof. Both are outside the checkout on purpose: real runs require a
  clean working tree.
- Two instances can run side by side only with different `VERIFY_HOME` **and** `VERIFY_PORT`.
  `launch` refuses when its instance is already up or the port belongs to something else.
  Never drive an instance this helper did not start.
- Teardown: `verify.sh stop` (see Cleanup).

## Doctor

    .claude/skills/verify-agent-sm/verify.sh doctor

Read-only; exit 0 only if every line is `ok`. It checks: the recorded pid is alive and is
running the scratch binary; the page answers with the agent-sm title; `/api/graph` has
transitions; the seeded run 359 is shown; the build matches the checkout (HEAD plus a `+dirty`
mark for uncommitted changes outside `.claude`). A `FAIL build matches checkout` means you edited
code after launching: `stop`, then `launch` again. Run doctor first whenever anything looks off.

## Drive

Two tiers. Use the tier the feature file names; a page journey is not proven by the API alone.

**HTTP (what the page itself calls).** `verify.sh api` adds the page's `X-SM-Token` (scraped
from `GET /`, as a browser would receive it) and prints the body plus an `HTTP <code>` line.

    verify.sh api GET  /api/state                       # {issue, status, events}: the run shown
    verify.sh api GET  /api/graph                       # transitions: [{from, out, to, exit?, limit?}]
    verify.sh api GET  "/api/queue?repo=$PWD"           # {can_run, provider, models, queued, history, ...}
    verify.sh api GET  "/api/issues?repo=$PWD"          # open issues (runs `gh api`, read-only network)
    verify.sh api POST /api/folders "{\"path\":\"$PWD\"}"   # {path, parent, directories, repo | git_error}
    verify.sh api POST /api/run '{"issues":[]}'         # 400 "bad request": safe refusal path

`POST /api/run` with real issue numbers and `POST /api/stop` are the Run and Stop buttons. Run
spends agent credits and creates a worktree and an `sm/issue-<n>` branch in the selected repo.
Do not call it unless the user asked for a live run in this conversation.

**Page (any browser tool the agent has: built-in browser, Playwright, CDP).** Open
`http://127.0.0.1:8799/`. Stable handles, all from `web/index.html`:

| What | Handle |
|---|---|
| Header: issue, state pill, status line, usage | `#issue`, `#pill`, `#line`, `#cost` |
| Setup: path, picker button, profile, agent, apply | `#repoPath`, button `Browse folders`, `#language`, `#agent`, button `Use setup` |
| Folder picker dialog | `dialog#folderPicker` (`Choose a repository folder`): `#folderPath`, buttons `Go`, `Home`, `Up`, `Cancel`, `Use this repository`; status `#folderStatus`; entries `#folderList button[data-path]` |
| Issues: filter, refresh, cards, count | `#filter` (aria-label `Filter issues`), button `Refresh`, `#issueGrid input[data-n="<number>"]`, `#issueCount`, `#selCount` |
| Run controls | `#publish`, `#budget`, `#transcripts`, `#audit`, `#stopBtn`, `#runBtn`, messages `#msg`, parallel runs `#lanes .lane[data-n="<n>"]` (click pins the run view; `#follow` unpins), finished chips `#recent button[data-n]`, header count `#jobs` |
| Model inputs | `#models input[data-state="<state>"]` (aria-label `<state> model`) |
| Run view | cards `#cards`, feed `#feed`, pipeline `#pipe button[data-s="<state>"]` (`aria-pressed`), detail `#detail`, steps `#steps tr`, `#stepCount`, denied `#deniedBox` / `#denied` |

The page fetches after load and re-renders on a poll (10 s idle, 2 s while running). After a
navigation or click, wait for the expected text (for example `#stepCount` non-empty), not a
fixed sleep, and re-find elements after each render: the pipeline and steps are rebuilt.

No headless-screenshot command is shipped: Firefox's `--screenshot` fires before the page has
fetched its data (it captures `waiting for data...`), so it proves nothing about run state.

**CLI.** The scratch binary is `$VERIFY_HOME/instance/agent-sm` (default
`~/.local/state/agent-sm-verify/instance/agent-sm`); seeded stats are `instance/stats.jsonl`.

## Evidence

`verify.sh evidence` prints (and creates) this run's directory,
`$VERIFY_HOME/evidence/<run-id>/`. Put artifacts in a subdirectory named after the feature:

- API: the command and its full output, e.g. `verify.sh api GET /api/state > "$(verify.sh evidence)/run-view/state.json"`.
- Page: a text record of the handles' values before the action, the action, and the values
  after; plus a screenshot with the `agent-sm` header visible when the browser tool can save one.
- CLI: command, stdout, stderr, exit code.
- Always: `verify.sh doctor > "$(verify.sh evidence)/doctor.txt"` so the proof names the build.

Proof standards:

- Exercise the real user path: the page, or the exact request the page sends. `SM_SELF`
  (a stand-in binary for runs) is a test hook; a proof that relies on it is not a proof of a run.
- Capture the action and the resulting state, not only the final screen.
- Check side effects next to what is visible: the page's steps against `/api/state` events; a
  started run against `<scratch>/repos/<hash>/<issue>/attempt-N/` files, `git worktree list`
  and `git branch --list 'sm/issue-*'` in the target repo.
- Record the feature ID and entry point with each artifact. An entry point you could not reach
  is reported as not verified, with the reason; it is not covered by a different entry point.
- Know what "safe" paths still do: loading the page with a repo selected runs `gh api` against
  GitHub (read-only). `Publish: local branch only` still spends credits and still creates a
  local branch and worktree; with `management.mode: "service"` it can still write to trackers.

## Cleanup

    .claude/skills/verify-agent-sm/verify.sh stop

Kills only the pid it recorded, and only if that pid is still running the scratch binary; never
kill agent-sm by name (the user may have their own on 8787). It copies `server.log` into the
evidence directory, deletes `instance/`, and prints the evidence path. Evidence is never
deleted; remove old `evidence/<run-id>/` directories by hand when they are no longer needed.
Run `stop` after a failed attempt too. Close any browser tab you opened.

If a live run was started, `stop` ends the server and its child run, but the target repo keeps
the `sm/issue-<n>` branch and `<n>-sm` worktree by design. Report them; delete them only if the
user says so.

## Helpers

`verify.sh` (executable, bash) is the only helper; its five subcommands are shown at the top.
`launch attempt1` seeds a run that ended `gave-up` with denied commands instead of the default.
