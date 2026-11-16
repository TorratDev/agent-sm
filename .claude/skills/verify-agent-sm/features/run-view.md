# Run view

The page shows one run: which issue, how it ended or where it is, what each step returned and cost, and where each state's results lead. It reads run files only, so it replays a finished run as well as a live one.

## Sub-features

- `run-finished` shows issue, exit state and total usage in the header and the cards.
- `run-steps` lists every step with result, model, turns, tools, denials and usage.
- `run-pipeline-select` selects a state and shows its outgoing results and last result.
- `run-denied` lists commands the allow list refused, per state.
- `run-stalled` flags a run with no exit and no recent update.
- `run-empty` shows the no-run state when no run data exists.

## How to get to it (user POV)

- Open the page; with a repository selected it shows that repository's newest run.
- Start `agent-sm serve <issue>` to pin one issue's run.
- Start `agent-sm serve` outside any repository to follow the newest run under the state root.

## Driving it with verify.sh

Preconditions:

- Baseline (`$V launch`, seed `attempt3`), `$V doctor` all `ok`.
- A browser tool is on `http://127.0.0.1:8799/` and `#stepCount` has text.

- **Finished header.** Load the page. Read `document.title`, `#issue`, `#pill`, `#line`, `#cost`. They read `#359 ready - agent-sm`, `#359`, `ready`, `run finished`, `$1.86`; the `State` card reads `ended: ready`.
- **Steps.** Read `#stepCount` and `#steps tr`. `10 so far`; rows run prepare, plan, investigate, execute, check, review `fail`, execute, check, review `pass`, publish `ok`, and the last row's run usage is `$1.86`.
- **Cross-check.** Run `$V api GET /api/state > "$($V evidence)/run-view/state.json"`. `issue` is `"359"`, there are 10 events with `out`, and the last event is `{"exit":"ready"}` with `cost_usd` 1.8591174.
- **Default selection.** Read `#pipe button[aria-pressed="true"]` and `#detail h3`. Both name `publish`, the last state that ran.
- **Select a state.** Click `#pipe button[data-s="review"]`. Only `review` has `aria-pressed="true"`; `#detail` reads `review`, `agent step on claude-sonnet-5-5 · ran 2x · $0.36`, the chips `pass → publish` and `fail → execute` carry class `taken`, and `Last result` shows a `PASS` verdict.
- **Selection survives a poll.** Wait 11 s and read `#detail h3` again. Still `review`.
- **Denied commands.** Run `$V stop`, then `$V launch attempt1`, and reload. `#pill` reads `gave-up`, `#deniedBox` is visible, and `#denied` lists `investigate` with `7 denied` and `execute` with `2 denied`. With `attempt3`, `#deniedBox` is hidden.
- **Stalled run.** Run `$V stop`, then `$V launch attempt4`, and reload. `#pill` starts with `no update for` and `#line` reads `the run may have been killed`.
- **Proof.** Save the before/after handle values to `$($V evidence)/run-view/page.txt` and a screenshot of the selected state with the header in view.

## Gotchas

- The page renders after its first fetch. Reading handles right after navigation returns `no run` / `waiting for data...`.
- `#pipe` and `#steps` are rebuilt on render. Re-find the button after each click or poll instead of reusing an element reference.
- The archived events carry no timestamps, so `attempt4` shows `no update for <about 30 million> min`. Assert the prefix, not the number.
- `run-empty` has no seed: `launch` always seeds a run and refuses to report ready without it. Report it as not verified.
- The live feed (`#feed`) and the `Unit` / `This step` cards need a `status.json` from a running session; no seed has one. Report them as not verified rather than asserting on the empty feed.
- Pinning with `agent-sm serve <issue>` is not what `launch` starts; that entry point needs its own instance and is not covered by the page journey.
