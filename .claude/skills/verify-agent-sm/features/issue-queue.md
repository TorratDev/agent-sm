# Issue queue

For the selected repository the page lists open GitHub issues, lets the user filter and select them, set publish level, per-step limit and models, and start or stop runs. Runs queue one after another; an issue with sub-issues is replaced by its runnable sub-issues.

## Sub-features

- `queue-list` lists open issues with labels, age, sub-issue progress and blocked state.
- `queue-filter` narrows the list by number, title or label.
- `queue-select` selects issues and enables `Run` with a count.
- `queue-controls` shows provider, limit unit and minimum, run limit, and per-step model inputs.
- `queue-run` starts runs and shows running, queued and finished chips. Gated: spends credits.
- `queue-stop` stops the running job and clears the queue. Gated with `queue-run`.
- `queue-refusals` rejects malformed run requests, missing tokens and foreign hosts.

## How to get to it (user POV)

- Open the page with a repository selected; the `Open issues` panel appears.
- Choose `Refresh` to re-list issues now (the list is cached for 30 s).
- Tick issue cards, then choose `Run`; choose `Stop` to stop.

## Driving it with verify.sh

Preconditions:

- Baseline, `$V doctor` all `ok`.
- `gh` is authenticated for this repository's remote: `gh api 'repos/{owner}/{repo}' --jq .full_name` prints the repository name.
- For `queue-filter` and `queue-select`: the repository has at least two open issues. Check with `$V api GET "/api/issues?repo=$PWD"`.
- For `queue-run` and `queue-stop`: the user asked for a live run in this conversation and named the issue.

- **List.** Load the page. `#issueCount` equals the number of objects from `$V api GET "/api/issues?repo=$PWD"`, and each has a card `#issueGrid input[data-n="<number>"]`. With zero issues the grid reads `No open issues.`
- **Controls.** Read `#provider`, `#budgetUnit`, `#budget`, `#runBudget`. With Copilot: `copilot`, `credits`, `100`, `Run limit: 500 credits`; `#audit` is disabled; `#models` has inputs for `execute`, `investigate`, `plan`, `review`. `$V api GET "/api/queue?repo=$PWD"` shows the same `session_limit`, `run_limit` and `models`.
- **Filter.** Type a title word of one issue into `#filter`. `#issueCount` reads `1 of <total>` and only that card remains. A word no issue has gives `No issue matches the filter.`
- **Select.** Clear the filter and tick two cards. `#selCount` reads `2 issues selected`, `#runBtn` is enabled and reads `Run 2 issues`. Untick both: `Nothing selected`, `#runBtn` disabled.
- **Refusals.** Run `$V api POST /api/run '{"issues":[]}'`: `bad request`, `HTTP 400`. Run `$V api POST /api/run '{"issues":[1],"publish":"merge"}'`: `bad request`, `HTTP 400` (the page may only ask for `pr`). Run `curl -s -o /dev/null -w '%{http_code}\n' -X POST -d '{"issues":[1]}' http://127.0.0.1:8799/api/run`: `403`. Run `curl -s -w ' %{http_code}\n' -H 'Host: evil.test' http://127.0.0.1:8799/api/graph`: `forbidden host 403`. After all four, `$V api GET "/api/queue?repo=$PWD"` still shows `"running":null` and `"queued":[]`.
- **Run (gated).** Tick the named issue, leave `Publish` on `local branch only`, click `#runBtn`. `#qstate` shows `running #<n>`, the header shows `#<n>`, and the steps table grows. Side effects to record: `git worktree list` gains `<n>-sm`, `git branch --list 'sm/issue-*'` gains `sm/issue-<n>`, and `~/.local/state/agent-sm-verify/instance/state/repos/*/<n>/attempt-1/` holds `events.jsonl` and `run.log`.
- **Stop (gated).** Click `#stopBtn`. `#qstate` shows `#<n> stopped` and the pill reads `stopped`. The branch and worktree remain; report them.
- **Proof.** Save API outputs, handle values before and after, and a screenshot under `$($V evidence)/issue-queue/`. Copy the run's `events.jsonl` there before `$V stop`, which deletes the scratch state.

## Gotchas

- When this map was seeded the repository had no open issues, so `queue-filter` and `queue-select` could not be driven. With an empty list, report them as not verified; do not create issues to make them pass.
- The issue list is cached for 30 s per repository. Use `Refresh` (or `?refresh=1`) after changing issues on GitHub.
- Listing issues calls GitHub through `gh`. Offline or unauthenticated, `#msg` reads `Cannot list issues: listing issues failed: ...` and the grid stays empty; that is a failed precondition, not an app bug.
- A real run requires a clean working tree in the target repository and `copilot` or `claude` signed in.
- `local branch only` is not a dry run: it spends credits and leaves a branch and worktree.
- The second refusal's issue number is never used (the request is rejected before any lookup), so it is safe to send.
- The run and stop sub-features were written from the code and README, not yet observed. Fix this file from what a real gated run shows.
