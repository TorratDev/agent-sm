# Stats and allow CLI

`agent-sm stats [file]` summarizes agent sessions per step: cost, models, tools used and never used, denials. `agent-sm allow [file]` turns the same file into suggested Claude allow lists as JSON. Both only read the file and need no profile.

## Sub-features

- `stats-report` prints one block per state with sessions, usage, models and tool use.
- `stats-missing` fails clearly on a missing file.
- `allow-json` prints `{"<state>": "<tools>"}` JSON for `SM_ALLOW_FILE`.
- `allow-copilot` refuses when `SM_PROVIDER=copilot`.

## How to get to it (user POV)

- Run `agent-sm stats` (it reads `$SM_STATS`, default `$SM_SCRATCH_ROOT/stats.jsonl`), or pass a file.
- Run `agent-sm allow [file]` the same way.

## Driving it with verify.sh

Preconditions:

- Baseline (`$V launch` builds the binary and seeds stats from `runs/359/attempt3/stats.jsonl`).
- `I=~/.local/state/agent-sm-verify/instance` (or `$VERIFY_HOME/instance`).

- **Report.** Run `$I/agent-sm stats $I/stats.jsonl; echo "exit $?"`. Exit `0`; blocks for `plan`, `investigate`, `execute` and `review`, each starting `<state>: <n> sessions, $<total> known total` and followed by `models: claude-sonnet-5-5 x<n>`.
- **Missing file.** Run `$I/agent-sm stats /nonexistent; echo "exit $?"`. Stderr `open /nonexistent: no such file or directory`, exit `2`.
- **Allow.** Run `env -u SM_PROVIDER $I/agent-sm allow $I/stats.jsonl | python3 -m json.tool`. Valid JSON whose keys are step names and whose values are comma-separated tool entries such as `Bash(grep:*)`.
- **Allow under Copilot.** Run `SM_PROVIDER=copilot $I/agent-sm allow $I/stats.jsonl; echo "exit $?"`. Stderr `allow suggestions use Claude syntax; use profile commands for Copilot`, exit `2`.
- **Proof.** Save each command with stdout, stderr and exit code under `$($V evidence)/stats-cli/`.

## Gotchas

- Without a file argument both commands read `$SM_STATS` or `$SM_SCRATCH_ROOT/stats.jsonl`, which may be the user's real stats. Always pass the seeded file.
- With fewer than 10 sessions per state the report adds `note: only <n> sessions, unused is not proof yet (want 10+)`. That is expected for the seed.
- The archived seed's Bash command names include fragments of shell text (`Bash(s/Runs:*)`). Assert the JSON shape, not a specific entry.
- The binary is deleted by `$V stop`. Drive the CLI before stopping.
