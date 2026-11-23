# agent-sm

A state machine that takes one GitHub issue to a reviewed branch. The program owns the loop:
each step returns a typed outcome, one table (`next()` in `main.go`) decides the transition, and
agents (`copilot -p` by default, `claude -p`, or `opencode run`) only run inside single steps. Every step is declared once in the `steps`
table in `main.go`: its function, outcomes, role prompt, output schema, allow list and model.

    prepare -> plan -> investigate (one session per unit) -> execute -> check -> review -> publish

Exits: `ready`, `gave-up`, `errored`, `split`, `escalated`, `stopped`. A blocked execute goes back to plan once
(usually to widen the file list), with the executor's report; blocked again, the run gives up.
Prepare and publish also give up when the issue is already closed or another attempt's PR for it is open.

`prepare` runs the checks on the untouched base first. If they already fail, the run stops as `errored`
before any agent starts, since no fix round of an issue that may not touch that code could pass. With a
publish level (not in service mode), it also files one `bug` issue titled `Base <base> check failed: <command>`
(or comments on the open one with that title) and links the blocked issue to it. That bug's body contains
`agent-sm: fixes-base`, which skips the base check; add the line to any issue meant to repair the base.
A profile's `base_checks` replaces the full list for this step; the Rust profile runs only `fmt` and `clippy`
there, since `cargo test` on the base took about 7 minutes. A base that passed is remembered by its tree and check list under `$SM_SCRATCH_ROOT/base-passed/`, so later
runs on the same base skip the check (delete that directory to force it). `serve` also gives each repository
one `CARGO_TARGET_DIR` (`$SM_SCRATCH_ROOT/repos/<repo>/cargo-target`) unless it is already set, so a new
worktree does not rebuild a Rust project from scratch.

## Usage

    go run .                                    # auto-configure and start the web UI
    go build
    agent-sm --config /trusted/go.json <repo-dir> <issue-number>
    agent-sm --config /trusted/go.json serve [--repo DIR] [issue] [addr]
    agent-sm stats [file]                       # per-state cost, models, tools used, denials
    agent-sm allow [file]                       # allow lists suggested by the stats, as SM_ALLOW_FILE JSON

Sub-issues: on the page, an issue with GitHub sub-issues shows its progress. Selecting it queues
its open sub-issues in its place; the issue itself never runs. If all its
sub-issues are already closed, selecting it closes the issue as completed (not in service mode). A sub-issue is
skipped when GitHub lists an open issue as blocking it, when its body's explicit dependency entries
name an open issue, or when it has sub-issues of its own (select that one directly). Body dependencies
are list entries under `Dependencies` (for example `- #5`) or lines starting with `Depends on` or
`Blocked by` under `Dependencies`/`Depends on`. Parent/Epic/Related lines and contextual prose
(including Story-gate explanations) are not blockers. Native GitHub blockers are also checked
before an agent starts, including for direct CLI runs; failed lookups stop the run.
The command line still runs exactly the one issue it is given. Failed queue entries show the
recorded error, when available, rather than only an `errored` label.

Re-running an issue: a run that does not merge leaves its worktree and its `sm/issue-<n>` branch,
because for a local-only run the staged changes there are the result. Earlier attempts are
never discarded by a new run. If the branch or worktree is occupied, the next attempt uses
`sm/issue-<n>-1` and `<n>-sm-1`, then `-2`, `-3`, and so on, choosing the first available pair.
Local branches, branches on origin, existing PR heads (including closed PRs), and worktree
paths all reserve their names. Failed lookups stop the run rather than assuming a name is free.
Every attempt starts fresh from the issue's declared/configured base, not from earlier work.
The page marks previous work as kept; web attempts also retain separate logs. Publishing
refuses to overwrite a remote branch that appeared after the name was selected.

The repo's working tree must be clean; the run creates an available `sm/issue-<n>` branch. It needs
`copilot`, `claude`, or `opencode`, plus `git` and `gh` on the PATH, authenticated independently of the desktop
app, plus the target project's build/test tools. Checks must be configured; there is no
implicit Cargo default. `stats` and `allow` do not require a project profile.

With no arguments or config, `go run .` starts the web UI in the current Git repo. It detects the
language of each selected repo (Go from `go.mod`, Rust from `Cargo.toml`, .NET from a root `*.sln`
or `*.slnx`) and runs that language's checks, and prefers the installed Copilot CLI, then Claude, then OpenCode.
Use **Browse folders** to navigate local folders and select a Git working tree, or enter its path
manually. The picker validates Git before enabling selection and resolves subfolders to their
repository root; linked worktrees are supported too. Non-Git folders and unreadable/missing paths
show an error, and nothing is initialized or uploaded. Folders belong to the computer running
agent-sm (the same PC for a local launch). Unknown languages
use a basic `git diff --check`; configure real build/test checks for meaningful verification.
You can override the detected language profile and agent in the page. The page is loopback-only and token-protected, but
choosing a repo gives the agent access to work in that repo, so only select repositories you trust.

There is no resume: a run always starts at `prepare`. A stopped or failed run leaves its branch
in place; delete it before running the issue again.

## Environment

| Variable | Default | Meaning |
|---|---|---|
| `SM_PUBLISH` | empty | empty: stop at a local staged branch. `pr`: commit, rebase, push, open a draft PR. `merge`: also merge it. A rebase conflict goes to a `resolve` agent (Haiku) that merges the conflicted files; the checks then rerun, and markers left or a `blocked` answer abort the rebase. |
| `SM_BASE` | `origin/main` | Branch that worktrees start from and publish rebases onto. |
| `SM_PROVIDER` | `copilot` | Runner: `copilot`, `claude`, or `opencode`. |
| `SM_SESSION_LIMIT` | Copilot: `100`; Claude/OpenCode: `2` | Session limit in the provider's native units: Copilot AI credits (minimum 100) or Claude/OpenCode USD. |
| `SM_RUN_LIMIT` | Copilot: `500`; Claude/OpenCode: `20` | Run limit in the same native units; `0` disables it. A nonzero Copilot limit must be at least 100. Unknown usage prevents another step under a run limit. |
| `SM_CONCURRENCY` | `1` | Starting value of the page's **In parallel** field: how many queued issues `serve` runs at once (1–16, across all selected repositories). Choosing another number and pressing Run changes it for the whole server; lowering it lets running jobs finish and holds new ones back. Each job gets its own `<n>-sm` worktree and branch; one issue never runs twice at once, and a queued issue that is already running waits. Session and run limits apply to each job, so the total spend can reach this many times the run limit. Stop and Ctrl-C end every running job. Profile field: `concurrency`. |
| `SM_BUDGET`, `SM_RUN_BUDGET` | none | Legacy dollar-limit aliases for Claude and OpenCode. Rejected for Copilot rather than converted to credits. |
| `SM_TIMEOUT` | `60` | Minutes before an agent session and its child processes are killed. |
| `SM_MODEL_<STATE>` | Copilot: `auto`; Claude: sonnet 5.5, opus 5.5 for review, haiku 5.5 for `SUMMARIZE` (the follow-up body); sonnet 5.5 for `DIAGNOSE` (the failed-run audit); haiku 5.5 for `RESOLVE` (publish rebase conflicts); OpenCode: `opencode/deepseek-v4.1-flash-free`; plan/review prefer `jev-1.13-free`, then `opencode/glm-5.3-flash`, when installed | Explicit CLI model ID per step. The page offers the selected provider's models as suggestions, while keeping the value editable. The web input wins for queued runs. |
| `SM_CHECKS` | profile (required) | Check commands, one per line. Empty checks fail configuration. |
| `SM_FMT` | profile or none | Formatter run before staging/checks; failure is explicit. Setting `SM_CHECKS` without `SM_FMT` disables the profile formatter. |
| `SM_ALLOW_FILE` | none | Claude-only JSON `{"state": "tools"}`; invalid files fail startup. Prefer profile `commands` for portable shell tools. |
| `SM_EXTRA_ALLOW` | none | Claude-only allow entries added to every agent step. |
| `SM_ADD_DIRS` | none | Comma-separated directories agents may read outside the repo. |
| `SM_NOTES_DIR` | none | Operator notes added to dispatches: `all.md`, or `<role>.md` for one step. |
| `SM_AUDIT` | on for Claude (off in service mode and for Copilot/OpenCode) | Claude only: load every tool and bypass permissions (outward commands stay denied) to measure tool use. On by default while runs still error; `SM_AUDIT=0` or `"audit": false` turns it off. Not a sandbox. |
| `SM_TRANSCRIPTS` | `true` | Local transcript saving for every provider (`true`/`false` or `1`/`0`), on by default while runs still error. A capture failure is logged and never fails the step. Independent of permission audit. |
| `SM_GHIDRA_COPY` | `none` | Legacy-profile-only Ghidra project copy. |
| `SM_EXPORT` | `~/rescue-re/export` | Legacy-profile-only export; generic runs do not inspect it. |
| `SM_EPIC` | none | Epic named in a follow-up issue. A follow-up (three failed fix rounds, or a partial delivery whose review lists remaining work the issue asked for) is filed with any publish level as a sub-issue of the issue it came from; after a partial PR it names that PR, and the PR gets a comment naming it. If filing fails, the run comments on the issue instead. A split files nothing: the issue stays open, so the run comments the breakdown on it for a person to turn into sub-issues. |
| `SM_LINKS` | none | Comma-separated gitignored files to symlink into a worktree (serve), added to the profile's `links`. |
| `SM_STATS` | `$SM_SCRATCH_ROOT/stats.jsonl` | Where session stats are appended. |
| `SM_SCRATCH_ROOT`, `SM_SCRATCH`, `SM_EVENTS` | `~/.local/state/agent-sm` (or `$XDG_STATE_HOME/agent-sm`), `<root>/<issue>`, `<scratch>/events.jsonl` | Where run data goes. |

A failed card on the board (errored, gave up, stalled) has a **Diagnose** button. It runs an auditor session (`diagnose` step, outside the state graph; it is never run on its own) that reads the run's `events.jsonl`, `run.log`, `ctx.json` and `transcripts/` and writes `diagnosis.json`: the cause, the fix, and whether it is a `bug` or a `follow-up`. A popup shows it and offers to create that issue (labelled `bug` or `follow-up` when the label exists; a follow-up becomes a sub-issue). Filing is unavailable in service management mode.

A session that hits `SM_TIMEOUT` returns `timeout`; an observed native limit exhaustion
returns `over-budget`. Both end with `gave-up`. Unknown/missing usage is never reported as
free and stops a bounded run before its next step. Unknown CLI errors fail closed as invalid,
not as completed work. Native billing limits can be soft/provider-enforced, not exact money caps.
Copilot's 100-credit minimum is a per-step limit, not a required charge. If fewer than 100
credits remain under the run limit, the next Copilot step stops instead of exceeding that limit.
Explicit lower limits are rejected; changing providers on the page uses that provider's defaults
rather than interpreting credits as dollars or vice versa.

## Profiles and tools

Pass an explicit operator-owned JSON file; configuration is never auto-discovered in a target
branch. See [Go](examples/go.json), [.NET unit testing](examples/dotnet.json), [Rust](examples/rust.json), and
[legacy Claude/Rust/Ghidra](examples/claude-legacy.json). Copy and adapt the examples outside
the target checkout. Precedence is defaults, profile, environment, then validated web overrides.
Relative `notes_dir` and `add_dirs` paths resolve against the profile's directory. Queued runs
receive an immutable resolved snapshot, including models and limits.

Supported fields are `provider`, `models` (state to CLI model ID), `checks`, `base_checks` (what
`prepare` runs on the untouched base; default `checks`), `format`,
`required_test_reports`, `session_limit`, `run_limit`, `timeout_minutes`, `commands`,
`notes_dir`, `add_dirs`, `links` (gitignored repo files such as a ROM dump, symlinked from the main checkout into each worktree; missing ones are skipped), `base`, `jira`, `management`, `transcripts`, `concurrency`, and the Claude-specific `claude_allow`,
`claude_extra_allow`, `audit`, and `legacy_rust_ghidra`. Unknown fields fail startup.
The page can select Copilot, Claude, or OpenCode; switching providers uses that provider's limit defaults,
while provider-specific options still require a compatible profile.

`checks` are trusted shell commands run deterministically in the worktree, including after a
publish rebase. They have timeout/Stop cancellation and must not mutate the checked candidate.
`format` runs before staging. `commands` separately grants agent access per role, for example
`{"execute":["dotnet build","dotnet test"]}`. Prefixes use plain command/subcommand words,
not shell expressions. They are translated to each runner's permission syntax and RTK forms;
granting `dotnet test` does not grant every `dotnet` command. Program-owned GitHub/history
mutations stay outside worker command profiles. Permission lists are not an OS sandbox:
execute untrusted repositories/tests in a disposable, appropriately isolated environment.

The .NET example is for the reference project's .NET 10 Microsoft.Testing.Platform runner
and its UnitTest project's installed TRX extension, not a universal .NET invocation. It does
not cover IntegrationTest, which needs its own infrastructure and reporting configuration.
`SM_CHECK_RESULTS` is a fresh directory for every check pass; required TRX report globs are
relative to it. Missing reports, zero executed tests, failed tests, or skipped required tests
fail validation. Enable equivalent evidence for every additional required test suite.
`SM_SCRATCH` is also available to check commands. SDKs, feeds, Docker, and packages remain
the target project's prerequisites; agent-sm does not install them.

Generic roles read the target's applicable instructions; use shared `AGENTS.md` and explicit
role notes for portable guidance. Copilot also loads its normal repository instructions.
Do not pass app configuration as an agent-sm profile: `.github/github-app.yml` remains owned
by the Copilot app. Old rescue-specific prompts are selected only by `legacy_rust_ghidra`.

OpenCode defaults to DeepSeek Flash. The decision-heavy plan and review steps prefer `jev-1.13-free`, then OpenCode's `opencode/glm-5.3-flash`, when the installed catalog offers them; otherwise they stay on DeepSeek Flash. Short names such as `deepseek-v4.1-flash-free`, `glm-5.3-flash`, and `jev-1.13-free` are expanded to their provider-qualified IDs. When OpenCode refuses a model because a free allowance or rate limit is spent, the step retries with up to three other catalog models from the same coding families (jev, GLM Flash, DeepSeek Flash/Pro, beam). A free-usage refusal skips `-free` models, since they share one allowance, and moves to a provider-backed model instead of retrying the same one.

OpenCode runs each step with a generated private agent definition derived from the state's allow list, so writes and shell commands stay restricted. Its `--format json` event stream supplies the final answer, tool calls, session ID, token counts, and per-step cost; the answer still goes through the same strict local JSON validation. It uses `--standalone`, disables project configuration, and keeps the target checkout's own OpenCode config from weakening permissions.

Copilot uses `--output-format=json` JSONL events, strict local JSON validation, and
`--usage-output-file` for native credits. The last complete main-agent response is decoded
without Markdown rendering or line wrapping, and requires a successful terminal `result`
event. Tool/sub-agent messages and partial deltas cannot substitute for that response;
prose, code fences, malformed JSON and off-schema content are still rejected, not repaired.
Its live feed currently shows start/end, not tool calls; unavailable model/tool/turn/usage
data is marked unknown. Claude retains its stream feed, and OpenCode exposes tool calls,
turns, tokens, and cost from its JSON events. All runners reject missing, duplicate,
malformed, or off-schema result fields and failed subprocesses. Stats separate providers and units; old Claude stats remain readable.
`allow` emits Claude syntax only; it does not infer a Copilot policy from absent telemetry.

Worker MCP servers, delegation, and broad permission bypasses are not enabled for Copilot:
its visible tools are explicitly restricted to file/search/shell tools. Add trusted command
tools through `commands`; Jira uses the separate program-owned transport below. The runner
does not pass `--trust-folder` or alter your saved CLI trust settings. Review inherited CLI
configuration/hooks and use a clean automation account for untrusted targets.

## GitHub and linked Jira

GitHub remains the queue; Jira tickets are not independent run inputs. An issue may declare:

```markdown
## Jira
- **Jira issue:** https://your-team.atlassian.net/browse/TEAM-42
- **Jira Epic:** https://your-team.atlassian.net/browse/TEAM-1
- **Base branch:** `feature/integration` (parent Story #12).
```

Only `Jira issue` identifies this ticket's counterpart. The declared base is used for worktree
creation, the task branch, rebasing, and the draft PR's actual base. A backticked branch may
have a trailing parenthetical note without other backticked values; service edits retain it when
preserving the body. If no base is declared, `SM_BASE`/profile `base` applies. A linked Jira key
is included in the PR title.

Jira is off by default. Adapt [the Action-owned example](examples/jira-actions.json), including
your workflow's exact status names. It assumes the target repository already has a trusted
lifecycle Action, like the reference project's `sync-jira-status.yml`; agent-sm neither
installs nor replaces it. Use this independently after human review/merge:

```sh
agent-sm --config /trusted/project.json reconcile <repo-dir> <github-issue-number>
```

Standalone reconciliation uses `JIRA_USER_EMAIL` and `JIRA_API_TOKEN` from the process
environment, never profile credentials or the desktop Atlassian connection. Optional `jira.api_base`
supports a scoped-token gateway (`https://api.atlassian.com/ex/jira/<cloudId>`); `jira.site`
must still be the browser site. Both URLs must refer to the same tenant. Redirects are rejected.
Credentials are not forwarded to worker agents.

Reconciliation is **read-only status verification**: it verifies the explicit counterpart and
exact GitHub backlink, closing-issue reference, Jira PR-title token, and intended PR base.
Assignment establishes the implementation phase; only an open non-draft closing PR
establishes review, and its verified merge establishes delivery. Every PR the program opens
says "Close #N"; drafts do not advance delivery, and child completion never advances a parent. Conflicts are reported, not overwritten.
Assignee selection, Start date initialization, transitions, and failed-Action recovery stay with
the existing Action/agent integration; status alignment is not a claim that those are verified.

Results are `already aligned`, `conflict`, `blocked`, or `skipped-unavailable`; only aligned
status exits reconciliation with status 0. During intake, conflicts/invalid mappings block that
run; unavailable integrations are recorded without blocking independent code work.
Off/Action-owned modes do not add ticket creation, labels, hierarchy, sprint edits, automatic
review readiness, or additional merge permissions. Service-owned management is separate below.

## Service-owned tracker management

This opt-in mode makes the **native Go service** the tracker writer, using `gh` for GitHub
and native HTTP for Jira. LLMs only propose structured plans; they do not get tools for tracker
CRUD, retries, or monitoring. Existing `off` and `actions` profiles keep their behavior;
only `management.mode: "service"` enables the new management writes. Tracker operations and
recovery do not launch agent sessions; live tenant behavior still requires sandbox verification.

Adapt [the service profile](examples/service-management.json) in an operator-owned file outside
the target checkout. Set `management.host` and `repo` to the intended GitHub scope. Jira uses
`jira.mode: "service"`, the browser `site`, project/epic, exact issue-type/status names, and the
Start date field ID. `initial_statuses` explicitly lists earlier statuses from which the
service may advance work; unknown states block rather than guess. The service discovers a
unique direct transition to the destination status, never an implicit multi-step workflow.
`identities` explicitly maps GitHub logins to Jira account IDs;
`github_assignee` selects the configured GitHub assignee. Use only `gh auth` and environment
`JIRA_USER_EMAIL` / `JIRA_API_TOKEN` for credentials, never profile secrets or desktop connections.
The example contains placeholders, not real identities or working tenant settings. Jira-required
custom creation fields can be supplied per exact type name, for example
`"create_fields": {"Epic": {"customfield_123": "Epic name"}}`; discover actual field IDs and
values in your project, not from this example. No Jira labels are added.

Service GitHub tokens (`GH_TOKEN`, `GITHUB_TOKEN` and Enterprise variants) and Jira credentials
are removed from agent/check environments. Agent tool policies also deny tracker commands,
including RTK forms; audit/extra-permission bypasses are rejected in service mode. These are
not an OS/network sandbox: use separate least-privilege agent authentication and do not expose
service credentials in files the workers can read.

Before enabling service mode, disable overlapping Jira lifecycle Actions, issue-close and
parent-roll-up Actions, and Jira-writing skills/other agents. List workflow filenames or IDs in
`management.workflows` for preflight checks. `writers_disabled: true` is the operator's
attestation that **all** competing writers are disabled, not an instruction that disables them.
An empty workflow list does not prove there are none. Preflight can inspect only configured
workflows; other writers require operator verification. No external repository is edited automatically.

**Management permission is independent of `SM_PUBLISH`.** That variable still owns push,
PR creation and merge authorization. Enabling management separately authorizes issue updates
**even when Publish is local-only**; local-only is not a tracker-write prohibition.

### Propose, preview, approve, apply

A WorkPlan is JSON with an `id` and `items`. Each item has `id`, `title`, `body`, and
`kind` (`epic`, `story`, or `task`). Optional fields are `number` (existing GitHub issue),
`jira_key` (existing Jira counterpart), `parent`, `depends_on`, `remove_dependencies`,
`base`, `assignee`, `github_type`, `remove_parent`, `labels`, and `remove_labels`.
Labels are additive unless explicitly listed for removal; unrelated labels are preserved.
Parent and dependency references are **local item IDs
in that plan**, not issue numbers, Jira keys, or cross-plan references. For example:

```json
{
  "id": "example-plan",
  "items": [
    {"id": "story", "title": "Example feature", "body": "Define the outcome", "kind": "story"},
    {"id": "first", "title": "Implement", "body": "Implement the feature", "kind": "task", "parent": "story"},
    {"id": "second", "title": "Verify", "body": "Verify the feature", "kind": "task", "parent": "story", "depends_on": ["first"]}
  ]
}
```

The CLI interface is:

```sh
agent-sm --config /trusted/service.json issues preview <repo-dir> PLAN.json
agent-sm --config /trusted/service.json issues apply <repo-dir> DIGEST
agent-sm --config /trusted/service.json issues status <repo-dir>
agent-sm --config /trusted/service.json issues cancel <repo-dir> DIGEST
agent-sm --config /trusted/service.json reconcile <repo-dir> <github-issue-number>
agent-sm --config /trusted/service.json reconcile <repo-dir> <github-issue-number> --apply
```

Preview is read-only. Review its scope, plan, expected remote state, operations and digest.
Explicitly apply **that digest**, never an automatically selected latest plan. Structural
create/edit, hierarchy and dependency changes require approval; configured routine lifecycle
updates are automatic. Optional coding-planner `issue_plan` output and follow-up work become
pending proposal files, not automatic ticket creation. They need the same preview/approval path.
Reconcile is read-only by default on the CLI; `--apply` explicitly authorizes lifecycle repair,
not unapproved structural changes. Off/Action-owned modes remain read-only.

Plans are limited to 100 items/1 MiB and must be acyclic; remote graph validation stops
explicitly at 1,000 nodes instead of accepting incomplete evidence. `title` is an explicit
replacement. An empty `body` preserves existing descriptions; a nonempty body explicitly
replaces them after approval, with canonical links added by the service. Omitted edges are
not removals. `remove_parent: true` explicitly detaches an existing non-task; Jira Sub-tasks
must retain their Story parent. Native issue types/labels must already be available.
Worker context keeps human comments but excludes the journal-identified service progress
comment, and rejects issue/comment context above 1 MiB. GitHub collections fail explicitly
above 16 MiB rather than accepting incomplete evidence.

New and changed proposals need stable, distinct plan IDs. Completed applies are no-ops.
Started, approved batches resume after restart and on eligible web reconciliation, using the
original approval and per-operation drift checks. Re-previewing that unchanged in-flight plan
returns its original artifact. Ambiguous creates search for the exact marker/backlink before
retry; uncertainty never authorizes another POST. Resolve unknown outcomes on the trackers,
then retry the same digest. `issues cancel` stops a batch's recovery **without rollback or
deleting anything**; inspect partial resources and use explicit existing issue numbers/Jira
keys with a new plan ID before proposing replacement work. Do not edit the journal to repair it.

In the web panel, paste the WorkPlan, choose **Preview (read-only)**, inspect the displayed
JSON/digest, check its approval box, then choose **Apply approved digest**. Nothing auto-applies.
Editing the input, another preview, reconciliation, or an apply attempt clears approval.
Use **Refresh status** to inspect managed operation/item maps and sync errors, and
**Reconcile / retry** to explicitly request reconciliation/eligible retries. The web reconcile
action can write configured lifecycle repairs; it is not the read-only CLI verification command.
After an uncertain apply response, inspect status rather than blindly submitting it again.

The UI uses `GET /api/management` for `{enabled, state}`; state includes preview/item maps
and may include operations and sync errors. `POST /api/management/preview` takes the WorkPlan
directly and returns `{digest, scope, plan, expected, operations}`. Apply takes `{digest}` at
`POST /api/management/apply`; reconcile takes `{}` at `POST /api/management/reconcile`.
Both return state or an explicit non-success error. POSTs use the existing `X-SM-Token`.
Proposal and remote content is rendered as text, never trusted HTML.

### Lifecycle and limits

While `serve` runs, the service watches registered PRs and resumes managed tracking after
restart. A one-shot command runs no daemon and cannot observe later review/merge events;
keep `serve` running or reconcile manually. This does not resume a stopped coding run.
Lifecycle follows verified assignment, review-ready closing PR and merge evidence; drafts
do not imply delivery, and completing a child never rolls up its parent.
Start dates use a verified assignment event's UTC date and never overwrite an existing date.
Missing events remain pending rather than substituting today's date. Stopping/failing a run
pauses its assignment/date automation until a new implementation starts; existing PR monitoring
continues. Automatic polling stops after verified delivery; explicit reconcile still checks
completed items. A human reopening delivered work requires intervention, not an automatic re-close.

GitHub/Jira changes are **not atomic**: inspect per-operation partial results and sync errors.
Ambiguous creates block for reconciliation/operator resolution instead of blind retry.
The state-root lock coordinates processes on the same host only; there is no multi-host
coordination. Do not run competing instances from separate state roots or hosts for the same
trackers. No sprint management, broad bidirectional mirroring, automatic external-repo edits,
or extra publication/merge authorization is implied. Local syntax/fixture checks do not verify
real GitHub/Jira behavior, live credentials, tenant configuration, or integration performance.
The durable journal is under `<state-root>/management/<repository-hash>/`, separate from
rotating run logs, with private files and a per-repository process lock. It currently rewrites
the complete history; large installations should compact/split this state before scaling it.

## Run data

- Web run data is stored under `<scratch-root>/repos/<repository-hash>/<issue>/`, with numbered retries in `attempt-1/`, `attempt-2/`, etc.
- `<scratch>/events.jsonl`: structured step results, errors (including CLI stderr), agent sessions and the exit. If a scratch directory is reused, the old event log is renamed to `events-<date>-<time>.jsonl`, not deleted.
- The scratch root is durable, so run history survives a reboot. Agents also write probe output (traces, dumps) there; delete an issue's directory when you no longer need it.
- `<scratch>/status.json`: the running session's live state. `<scratch>/run.log`: output of a run started from the page.
- Transcripts are on by default. To turn them off, untick **Save transcripts** before Run, set `"transcripts": false` in a profile, or use `SM_TRANSCRIPTS=0`. Each agent call/retry gets a separate private directory at `<scratch>/transcripts/<provider>-<step>-<unique>/`; its path is recorded in `events.jsonl`.
  - `request.json`: exact CLI arguments (including prompts), working directory, schema and start time; no environment dump.
  - `stdout.log` / `stderr.log`: raw CLI streams captured before JSON validation, including malformed output. Claude's stdout includes its message/tool stream and native usage.
  - OpenCode keeps the generated private `opencode.json` agent definition next to the raw streams, so the effective permissions can be inspected.
  - Copilot also retains `usage.json` and copies its native session events to `messages.jsonl` after the subprocess exits. This requires CLI support for `--session-id`, `--no-remote-export` and the documented local session-state layout. Session export to GitHub is disabled for these captured Copilot calls; no gist/share upload is requested.
  - `result.json`: agent-call outcome, finish time and any capture error. A logging failure prevents successful workflow continuation, even if the provider itself returned a valid result; provider usage statistics still describe that provider call.
  - Captures contain only messages exposed by the CLI, not hidden model reasoning. Cancellation, output limits, startup failure or disk errors can leave partial captures; a missing native session log is recorded explicitly. Transcripts are not reconstructed for past runs.
  - Transcripts can contain source code, secrets or sensitive tool output. Directories are private (`0700`), app-created files use `0600`, and nothing expires automatically. Review/delete them when no longer needed; Copilot also retains its original local session history separately. This does **not** enable Claude's permission-bypassing **permission audit**.
- `stats.jsonl` under the run history (`SM_STATS`): one line per agent session, with its time (`t`) and repository (`repo`), shared by every working directory.
- `agent-sm stats [file]` summarizes usage and models by provider/state. The web UI follows the latest run; queue history is in memory and resets on restart, but saved run files remain available. There is no older-run picker yet.
- `runs/`: archived attempts kept in the repo as evidence for the issues that cite them. Nothing writes there.

## Platforms

Linux, macOS and Windows. Platform differences are isolated in `filelock_unix.go` /
`filelock_windows.go` (journal locking: `flock` vs `LockFileEx`) and `proc_unix.go` /
`proc_windows.go` (process groups and group kill: `setpgid`/`kill(-pid)` vs
`CREATE_NEW_PROCESS_GROUP`/`taskkill /T`/`CTRL_BREAK_EVENT`). Windows caveats:

- Check commands run via `sh -c`, so a `sh` (e.g. Git for Windows) must be on `PATH`.
- Group kill uses `taskkill /T`; if that misses a descendant, the one-second pipe
  timeout ends the session anyway.
- `SM_LINKS` symlinks need Windows Developer Mode (or an elevated process).
- Tests that fake providers with `#!/bin/sh` scripts self-skip on Windows; run the
  full suite on Linux, macOS or WSL.

## Tests

GitHub Actions runs formatting checks, `go vet`, race-enabled tests, and a build on pull
requests and pushes to `main`, using the Go version in `go.mod`. Live agent smoke tests
remain opt-in and are not run in CI.

    go vet ./... && go test ./...
    scripts/test-affected.sh                             # only tests affected by the diff vs origin/main
    SM_SMOKE=1 go test -run Smoke -v                     # opt-in: one real Copilot session
    SM_SMOKE=1 SM_PROVIDER=claude go test -run Smoke -v  # opt-in: one real Claude session
    SM_SMOKE=1 SM_PROVIDER=opencode go test -run Smoke -v # opt-in: one real OpenCode session

Normal tests use fake CLIs and temporary repositories; they do not call paid agents or mutate Jira.
CLI flags/usage follow the installed CLI's help. Live provider and target-.NET compatibility
must be checked with these explicit smoke tests in your authorized environment.
