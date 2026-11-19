# Repository setup

Before listing issues or starting runs, the user chooses a local Git repository, a language profile and an agent. The path can be typed or picked in a folder dialog that only enables Git working trees and resolves a subfolder to its repository root.

## Sub-features

- `setup-default` preselects the repository the server was started in.
- `setup-typed` applies a typed path with `Use setup`.
- `setup-browse` navigates folders in the picker and uses the detected repository.
- `setup-not-git` refuses a folder that is not a Git working tree, or is missing.
- `setup-profile-agent` switches language profile and agent, with that agent's limit defaults.

## How to get to it (user POV)

- Type into `Repository path` and choose `Use setup`.
- Choose `Browse folders`, navigate, then `Use this repository`.
- Start `agent-sm serve --repo DIR` (or start it inside a repository) to have it preselected.

## Driving it with verify.sh

Preconditions:

- Baseline, `$V doctor` all `ok`.
- A scratch non-Git folder exists: `mkdir -p "$($V evidence)/repo-setup/plain"`.

- **Default.** Load `/#start`. `#repoPath` holds `$PWD` and the `Open issues` panel is visible.
- **Picker opens.** Click the `Browse folders` button. `dialog#folderPicker` opens, `#folderPath` holds `$PWD`, `#folderStatus` reads `Git repository: $PWD`, and `Use this repository` is enabled.
- **Subfolder resolves to root.** Click `#folderList button[data-path="$PWD/web"]`. `#folderPath` ends in `/web`, `#folderStatus` still reads `Git repository: $PWD`. Same request: `$V api POST /api/folders "{\"path\":\"$PWD/web\"}"` returns `"repo":"$PWD"` and `HTTP 200`.
- **Up.** Click `Up`. `#folderPath` is `$PWD` again.
- **Not Git.** Type the scratch `plain` folder into `#folderPath` (status reads `Press Go to check this folder.`), click `Go`. `#folderStatus` starts with `not a Git working tree` and `Use this repository` is disabled. Same request via `$V api POST /api/folders` returns a `git_error` and an empty `repo`.
- **Missing folder.** Run `$V api POST /api/folders '{"path":"/nonexistent-agent-sm"}'`. Body starts with `cannot read folder:` and the code is `HTTP 400`.
- **Use repository.** Navigate back to `$PWD`, click `Use this repository`. The dialog closes, `#repoPath` holds `$PWD`, `#msg` is empty.
- **Typed bad path.** Set `#repoPath` to the scratch `plain` folder and click `Use setup`. `#msg` starts with `Cannot use repository: not a Git working tree` and the `Open issues` panel is hidden. Clear the field and click `Use setup`: `#msg` reads `Enter a local Git repository path.`
- **Agent switch.** Set `#agent` to `Claude`, click `Use setup`. `#provider` reads `claude`, `#budgetUnit` reads `USD`, and `#audit` becomes enabled. `$V api GET "/api/queue?repo=$PWD&provider=claude"` shows `"provider":"claude"`.
- **Token required.** Run `curl -s -o /dev/null -w '%{http_code}\n' -X POST -d '{}' http://127.0.0.1:8799/api/folders`. It prints `403`.
- **Proof.** Save the API outputs and the before/after handle values under `$($V evidence)/repo-setup/`, with a screenshot of the open picker.

## Gotchas

- Typing in `#folderPath` only invalidates the status. Nothing is checked until `Go` (or Enter).
- `Use this repository` also applies the setup; it triggers an issue listing through `gh` (read-only network call).
- The picker lists the server machine's folders. An empty path means the server user's home.
- With `management.mode: "service"` any repository other than the configured one is refused. The baseline does not use a service profile, so that refusal is not covered here.
