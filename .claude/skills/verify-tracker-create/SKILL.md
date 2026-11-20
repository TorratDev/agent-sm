---
name: verify-tracker-create
description: Exhaustively check that service-management `apply` never creates a GitHub or Jira issue twice, under every schedule of failures, lost responses, crashes and stale searches up to a bound. Use before finishing any change to management.go, management_apply.go, management_remote.go, management_graph.go, management_verify.go, or the fake trackers in management_test.go, and whenever a change touches how tracker issues are created, journaled or recovered.
---

# Verify tracker creation

In service-management mode (`management.mode: "service"`), `agent-sm issues apply` creates GitHub
issues and Jira tickets from an approved plan. Creation is not atomic and the process can die or
lose a response at any point, so the code journals an intent before each create and refuses to
create again when an earlier outcome is unknown.

This check runs the **real** `manager.apply` against the fake trackers under every schedule of
faults up to a bound, restarting the service after each failure, and checks three rules after
every apply. There is no hand-written model: the Go code is what runs.

## The rules

| Rule | Plain statement | Harm if broken |
|---|---|---|
| `NoDuplicate` | No plan item ever exists twice on GitHub or twice on Jira. | Duplicate tickets in real trackers that the service never cleans up. |
| `NoSecondCreate` | For one plan item, at most one create request ever leaves the service per tracker, even when the first one returned an error. | An "error" can be a create that did land; a second request is how duplicates start. |
| `CompleteMeansPaired` | When the service reports a batch complete, every item exists exactly once on GitHub and once on Jira, and applying again writes nothing. | A batch reported done with tickets missing, or a "finished" batch that writes again. |

## Does my change touch it?

    .claude/skills/verify-tracker-create/check.sh stale

It lists the covered files that changed since the sync commit (the file `SYNC` here). Covered:

| File | What the check exercises there |
|---|---|
| `management.go` | `manager.op` (intent journal: `pending` before the call, `done` after), `manager.locked`, `manager.save` |
| `management_apply.go` | `manager.apply`, `ensureGitHub`, `ensureJira`, `findJira`, `itemMarker`, and the calls `apply` makes after creation (`linkAndEdit`, `mirrorDependency`) |
| `management_remote.go` | `preflight`, `ghPages`, `jiraPages`, `typeName`, as `apply` calls them |
| `management_graph.go` | `validateRemoteGraph`, as `apply` calls it |
| `management_verify.go` | `verifyPaired` |
| `management_test.go` | `fakeTrackers`, `testManager`, `simplePlan`: the stand-in for GitHub and Jira that the check trusts |

The check itself is `management_formal_test.go` (build tag `formal`, so `go test ./...` and CI
skip it). Run the check when `stale` lists anything, and for any change to how issues are
created, searched for, journaled or resumed, wherever it lives.

## Run

    .claude/skills/verify-tracker-create/check.sh          # about 20 s
    .claude/skills/verify-tracker-create/check.sh deep     # about 10 min, before merging a change to op/ensure*

Needs only the Go toolchain named in `go.mod`. The script runs from the repo root and puts temp
files on `/dev/shm` when it exists; on a disk the journal's fsyncs make the default run take
minutes. Bounds can be set by hand: `FORMAL_FAULTS_ONE` (one-item plan, default 2, `deep` 3) and
`FORMAL_FAULTS_THREE` (story with two tasks, default 1).

A pass ends with two lines like:

    one item: all rules hold in 2110 schedules of at most 2 faults (1686 completed the batch)
    story with two tasks: all rules hold in 167 schedules of at most 1 faults (143 completed the batch)

The schedule counts change whenever `apply` makes more or fewer tracker calls; that is expected.
A run where no schedule completes the batch fails as vacuous.

## What a schedule is

One schedule = an empty journal and empty trackers, a preview, then `apply` of the approved
digest, repeated after every failure or crash with a fresh `manager` on the same journal
directory (what an operator retry or a restart does), up to faults + 2 applies. At each tracker
call, at most one of these can happen:

- **fail**: the call returns an error and the tracker did nothing.
- **lost**: a write takes effect on the tracker, but the response is lost.
- **crash**: the service dies just before sending the call.
- **crash-after**: a write takes effect, then the service dies before recording it.
- **stale**: the GitHub issue listing or the Jira search answers without newly created issues.

Crashes happen only at tracker calls. That covers every journal ordering, because nothing but a
journal save happens between two calls, and a save is atomic (temp file, fsync, rename).

## Read a failure

    RULE BROKEN: NoDuplicate: item "story" exists 2 times on GitHub and 1 times on Jira
    Shortest schedule found (2 faults):
      apply 1: github POST /repos/owner/repo/issues (call 6) takes effect on the tracker, but the response is lost
      apply 2: github GET /repos/owner/repo/issues (call 12) answers from a stale index that does not show newly created issues yet
    Create requests sent: map[github story:2 jira story:1]
    All 27 calls: ...

Read it as a story: which apply, which call, what went wrong there, then the rule that broke.
The run stops at the first broken rule and tries fewer faults first, so the schedule shown is a
shortest one. To see whether another rule also breaks, run one rule alone:
`FORMAL_ONLY=NoSecondCreate .claude/skills/verify-tracker-create/check.sh` (also `NoDuplicate`,
`CompleteMeansPaired`).

**Real bug or check error?**

- It is a real bug when the schedule is something the world can do (every fault above is) and
  the calls listed are the ones your code makes. Tell the user the schedule in those words. Do
  not weaken a rule or lower a bound to make it pass, and do not change product code beyond the
  task unless the user asks.
- It is a check error when the message starts with `explorer:` (the run was not deterministic:
  look for a new source of randomness, such as call order depending on map iteration), when the
  fake trackers do not behave like the real ones for a call you added (fix `fakeTrackers` in
  `management_test.go`), or when a new create path uses a marker other than
  `agent-sm:<plan id>:<item id>` (the check counts items by that marker; see `formalRules` and
  `formalAPI.call`).
- `unhandled fake GitHub ...` / `unhandled fake Jira ...` means `apply` now makes a call the fake
  does not know. Add it to `fakeTrackers.call` faithfully; do not skip it.

## When a change is intended

- New tracker call inside `apply`: nothing to update unless the fake lacks it. If it is a search
  that can lag behind writes, add its key next to `ghSearch`/`jiraSearch` in
  `management_formal_test.go` so it gets the **stale** fault.
- New kind of thing created (a third tracker, a new create endpoint): add its create key next to
  `ghCreate`/`jiraCreate`, and count it in `formalRules`.
- A rule should genuinely change (for example, a deliberate retry of creates): that is a product
  decision. Stop and ask the user; show them the rule's plain statement and harm from the table.

After the check passes on the changed code, and you have read the diff of the covered files and
confirmed this file still describes them (functions named in the table exist and do what it
says), move the sync commit forward: commit the change, then write that commit's hash into
`.claude/skills/verify-tracker-create/SYNC`. Do not move it for a change you did not recheck.

## Scope

Verified, at the sync commit: the three rules hold for every schedule of at most 2 faults on a
one-item plan (2,110 schedules) and at most 1 fault on a story with two tasks and a dependency
(167 schedules). The `deep` run also passed at that
commit: at most 3 faults on the one-item plan, 87,386 schedules.

Not covered, so a pass says nothing about them:

- Real GitHub and Jira. The trackers are `fakeTrackers`; `remoteTrackers.call` (HTTP, ETag
  cache, rate-limit handling) never runs. Whether real search lag stays within "stale" is assumed.
- More faults than the bound, and plans other than the two above (existing issues via `number`
  or `jira_key`, parent removal, labels, dependency removal).
- Two services at once. One process runs at a time; the `flock` in `manager.locked` and separate
  hosts or state roots are not exercised.
- A torn or lost journal write. `manager.save` is treated as atomic and durable.
- Humans or other tools editing or deleting issues mid-batch, `issues cancel`, and the lifecycle
  code (`syncOne`, `reconcile`, `resume`, `watch`, `deactivate`).
- Progress. The rules are about never doing harm, not about finishing: see below.

Known behavior, by design (asserted by `TestManagementIntentBeforePOSTIsDurable`): any failure
or crash after `manager.op` has saved a create intent blocks the batch with `create outcome
unknown`, even when no create request was ever sent, for example one failed issue listing on the
first apply. Retrying the same digest never recovers; the operator has to cancel and preview a
new plan. The check reports these schedules as not completed, not as broken rules.

After changes that skipped this check, use `/maintain-formal-verification-skill` to bring the
check and this file back in line.
