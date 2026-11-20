# agent-sm verification map

The maintained source for verifying agent-sm's user-facing behavior. Read this index, then use
the matching feature file as the recipe. Commands assume the repo root as working directory and
`V=.claude/skills/verify-agent-sm/verify.sh`.

## Baseline preconditions

- `$V launch` printed `ready: http://127.0.0.1:8799 ...` and `$V doctor` is all `ok`.
- The seed is the archived run `runs/359/attempt3`: issue 359, 10 steps, exit `ready`, $1.86.
  `$V launch attempt1` swaps in a run that ended `gave-up` with denied commands.
- The selected repository is this checkout (serve auto-selects the repo it starts in).
- Never drive an instance `verify.sh` did not start (the user's own page is on port 8787).

## Driving conventions

- Start every recipe from the baseline unless its preconditions say otherwise.
- HTTP actions go through `$V api METHOD PATH [JSON]`. Page actions go through a browser tool
  on `http://127.0.0.1:8799/`, using the handles in SKILL.md (ids, button names, `data-*`).
- After a page load or click, wait for the expected text; the page renders after a fetch.
- Treat commands as literal. `$PWD` means the repo root.
- Starting a run (`Run` button, `POST /api/run` with issue numbers, `agent-sm <repo> <issue>`)
  spends agent credits and creates a branch and worktree. Only with the user's go-ahead.

## Proof and skip reporting

- Capture the action and the resulting state, in `$($V evidence)/<feature>/`.
- Page proof: handle values before and after, plus a screenshot showing the `agent-sm` header.
- HTTP proof: the command and full output including the `HTTP <code>` line.
- CLI proof: command, stdout, stderr, exit code.
- Page claims about a run are cross-checked against `$V api GET /api/state`.
- Report an unreachable entry point with the command tried and the unmet precondition. Do not
  report it as verified through another entry point.

## Feature entry contract

Each feature file has an H1, one paragraph on the user-visible behavior, then exactly four H2s
in order: `Sub-features`, `How to get to it (user POV)`, `Driving it with verify.sh`, `Gotchas`.
Driving starts with `Preconditions:` and pairs each user action with an exact command or handle
and the observable result. Keep implementation details out.

## Features

- [Run view](./run-view.md) covers the header, current-run cards, pipeline selection, steps table and denied commands for a finished or stalled run.
- [Repository setup](./repo-setup.md) covers typing a path, the folder picker, and the errors for non-Git or missing folders.
- [Issue queue](./issue-queue.md) covers listing and filtering issues, selection, run controls, and the Run/Stop refusals; live runs are gated.
- [Stats and allow CLI](./stats-cli.md) covers `agent-sm stats` and `agent-sm allow` on a stats file.

## Not mapped yet

- The one-shot CLI run `agent-sm --config FILE <repo-dir> <issue>` end to end (costs credits; needs `gh` auth and an issue).
- Publishing (`SM_PUBLISH=pr|merge`), sub-issue expansion, re-run naming (`sm/issue-<n>-1`).
- `reconcile` and `issues preview|apply|status|cancel` (need a Jira tenant and a service profile).
- Transcripts (`Save transcripts`) and the live agent feed during a running step.
