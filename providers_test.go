package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func useProfile(t *testing.T, p Profile) {
	t.Helper()
	old := settings
	settings = p
	t.Cleanup(func() { settings = old })
}

func writeFile(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	// Fake provider/tracker binaries are sh scripts; Windows cannot execute them.
	if runtime.GOOS == "windows" && strings.HasPrefix(body, "#!") {
		t.Skipf("%s needs a Unix shell; run the suite on Linux or WSL", filepath.Base(path))
	}
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}

func TestResultValidation(t *testing.T) {
	for _, raw := range []string{
		`{}`, `{"verdict":"PASS"}`, `{"verdict":"UNKNOWN","reasons":[]}`,
		`{"verdict":"PASS","reasons":null}`, `{"verdict":"PASS","reasons":[1]}`,
		`{"verdict":"PASS","reasons":[],"extra":true}`, `{"verdict":"PASS","verdict":"REVISE","reasons":[]}`,
		`{"verdict":"PASS","reasons":[]} {}`, "```json\n{}\n```", `null`, `{"verdict":[],"reasons":[]}`,
	} {
		var out struct {
			Verdict string
			Reasons []string
		}
		if err := validateResult([]byte(raw), steps[Review].Schema, &out); err == nil {
			t.Errorf("accepted invalid result %s", raw)
		}
	}
	var out struct {
		Verdict string
		Reasons []string
	}
	if err := validateResult([]byte(`{"verdict":"PASS","reasons":["verified"]}`), steps[Review].Schema, &out); err != nil {
		t.Fatal(err)
	}
	if err := validateResult([]byte(`{"status":"plan","objective":"x","points":[],"points_derived":true,"files":[],"verification":"default","needs_investigation":true,"fits_one_pr":true,"units":[{"id":"a","title":"b"}]}`),
		steps[Plan].Schema, new(any)); err == nil {
		t.Fatal("accepted incomplete nested unit")
	}
}

func TestProfileResolution(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "profile.json")
	if err := os.Mkdir(filepath.Join(dir, "notes"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, `{"checks":["go test ./..."],"notes_dir":"notes","models":{"execute":"example-model"}}`, 0o600)
	t.Setenv("SM_MODEL_EXECUTE", "override-model")
	p, err := loadProfile(path)
	if err != nil {
		t.Fatal(err)
	}
	if p.Provider != "pi" || p.NotesDir != filepath.Join(dir, "notes") || p.Models["execute"] != "override-model" {
		t.Fatalf("wrong resolution: %+v", p)
	}
	for _, invalid := range []string{
		`{"unknown":true}`, `{"checks":[]}`, `{"checks":[" "]}`, `{"checks":["true"],"session_limit":0}`,
		`{"checks":["true"],"models":{"check":"auto"}}`, `{"checks":["true"],"commands":{"execute":["git push"]}}`,
		`{"checks":["true"],"commands":{"execute":["rtk proxy git push"]}}`,
		`{"checks":["true"],"commands":{"execute":["dotnet test; curl evil"]}}`,
		`{"checks":["true"],"required_test_reports":["../old.trx"]}`,
		`{"checks":["true"],"claude_allow":{"execute":"Bash"}}`,
	} {
		writeFile(t, path, invalid, 0o600)
		if _, err := loadProfile(path); err == nil {
			t.Errorf("accepted invalid profile %s", invalid)
		}
	}
}

func TestQueuedProfile(t *testing.T) {
	p := defaultProfile("pi")
	p.Checks = []string{"true"}
	useProfile(t, p)
	got, err := queuedProfile(runRequest{Provider: "pi", Budget: "150", Models: map[string]string{"execute": "chosen-model"}})
	if err != nil || got.Models["execute"] != "chosen-model" || got.SessionLimit != 150 {
		t.Fatalf("queued overrides: %+v %v", got, err)
	}
	if len(settings.Models) != 0 || settings.SessionLimit != 2 {
		t.Fatal("queue mutated the server's profile")
	}
	for _, req := range []runRequest{
		{Budget: "0"}, {Budget: "-1"}, {Budget: "NaN"},
		{Models: map[string]string{"check": "model"}},
	} {
		if _, err := queuedProfile(req); err == nil {
			t.Errorf("accepted invalid queued override %+v", req)
		}
	}
	got, err = queuedProfile(runRequest{Provider: "pi"})
	if err != nil || got.Provider != "pi" || got.SessionLimit != 2 || got.RunLimit != 20 {
		t.Fatalf("provider selection: %+v %v", got, err)
	}
}

func TestQueuedLanguageProfile(t *testing.T) {
	p := defaultProfile("pi")
	p.Checks = []string{"true"}
	useProfile(t, p)
	got, err := queuedProfile(runRequest{Language: "go", Provider: "pi"})
	if err != nil || got.Provider != "pi" || len(got.Checks) != 3 || !strings.Contains(got.Checks[0], "gofmt") {
		t.Fatalf("Go profile selection: %+v %v", got, err)
	}
	got, err = queuedProfile(runRequest{Language: "dotnet", Provider: "pi"})
	if err != nil || len(got.TestReports) != 1 || !strings.HasPrefix(got.Checks[0], "dotnet build") {
		t.Fatalf(".NET profile selection: %+v %v", got, err)
	}
	if _, err := queuedProfile(runRequest{Language: "python"}); err == nil {
		t.Fatal("accepted an unknown language profile")
	}
}

// realTempDir is t.TempDir() as Git reports it: Windows' temp path can hold 8.3 short names
// (RUNNER~1), and a symlinked temp directory would not match either.
func realTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestCanonicalRepo(t *testing.T) {
	dir := realTempDir(t)
	repo := filepath.Join(dir, "repo")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "init", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %s: %v", out, err)
	}
	subdir := filepath.Join(repo, "nested")
	if err := os.Mkdir(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := canonicalRepo(subdir)
	if err != nil || got != repo {
		t.Fatalf("canonical repo = %q, %v; want %q", got, err, repo)
	}
	if _, err := canonicalRepo(filepath.Join(dir, "not-a-repo")); err == nil {
		t.Fatal("accepted a non-repository path")
	}
}

func TestRunScratchSeparatesRepositories(t *testing.T) {
	t.Setenv("SM_SCRATCH_ROOT", t.TempDir())
	a := runScratch("/work/first", 12)
	b := runScratch("/work/second", 12)
	if a == b || filepath.Base(a) != "12" || filepath.Base(b) != "12" {
		t.Fatalf("repository scratch paths collided or lost issue id: %q, %q", a, b)
	}
}

func TestFolderBrowser(t *testing.T) {
	root := realTempDir(t)
	t.Setenv("HOME", root)
	t.Setenv("USERPROFILE", root)
	repo, plain, linked := filepath.Join(root, "repo"), filepath.Join(root, "plain folder"), filepath.Join(root, "linked")
	if err := os.Mkdir(plain, 0o755); err != nil {
		t.Fatal(err)
	}
	testGit(t, root, "init", "-q", "-b", "main", repo)
	testGit(t, repo, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-q", "--allow-empty", "-m", "base")
	testGit(t, repo, "worktree", "add", "-q", "-b", "linked", linked)
	nested := filepath.Join(repo, "nested")
	if err := os.Mkdir(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "not-a-folder.txt")
	writeFile(t, file, "do not list file contents", 0o600)
	mux := http.NewServeMux()
	folderRoutes(mux, func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodPost || r.Header.Get("X-SM-Token") != "secret" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return false
		}
		return true
	})
	request := func(method, token, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, "/api/folders", strings.NewReader(body))
		req.Header.Set("X-SM-Token", token)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		return w
	}
	for _, tc := range []struct {
		name, path, wantRepo string
	}{
		{"home", "", ""},
		{"non-git", plain, ""},
		{"repository", repo, repo},
		{"nested folder", nested, repo},
		{"linked worktree", linked, linked},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, _ := json.Marshal(map[string]string{"path": tc.path})
			w := request("POST", "secret", string(body))
			var listing folderListing
			if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &listing) != nil {
				t.Fatalf("folder response: %d %s", w.Code, w.Body)
			}
			if listing.Repo != tc.wantRepo || (listing.GitError == "") != (tc.wantRepo != "") {
				t.Fatalf("Git validation: %+v", listing)
			}
			if listing.Path == "" || listing.Parent != filepath.Dir(listing.Path) {
				t.Fatalf("navigation paths: %+v", listing)
			}
			if tc.name == "home" {
				if len(listing.Directories) != 3 || listing.Directories[1].Name != "plain folder" {
					t.Fatalf("expected only navigable folders: %+v", listing.Directories)
				}
			}
		})
	}
	for _, path := range []string{file, filepath.Join(root, "missing")} {
		body, _ := json.Marshal(map[string]string{"path": path})
		if w := request("POST", "secret", string(body)); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "cannot read folder") {
			t.Fatalf("invalid path accepted: %d %s", w.Code, w.Body)
		}
	}
	for _, body := range []string{`{"unknown":true}`, `{} {}`, `null`, `{`} {
		if w := request("POST", "secret", body); w.Code != http.StatusBadRequest {
			t.Fatalf("invalid folder request accepted: %s (%d)", body, w.Code)
		}
	}
	for _, tc := range []struct{ method, token string }{{"GET", "secret"}, {"POST", ""}, {"POST", "wrong"}} {
		if w := request(tc.method, tc.token, "{}"); w.Code != http.StatusForbidden {
			t.Fatalf("unauthorized browsing accepted: %d", w.Code)
		}
	}
	t.Run("Git unavailable", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		body, _ := json.Marshal(map[string]string{"path": repo})
		w := request("POST", "secret", string(body))
		var listing folderListing
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &listing) != nil || listing.Repo != "" || !strings.Contains(listing.GitError, `exec: "git"`) {
			t.Fatalf("missing Git was accepted: %d %s", w.Code, w.Body)
		}
	})
}

func TestAutoLanguageProfile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "go.mod"), "module example\n", 0o644)
	goProfile, err := autoLanguageProfile(dir)
	if err != nil || len(goProfile.Checks) != 3 || !strings.Contains(goProfile.Checks[0], "gofmt") {
		t.Fatalf("Go auto-detection: %+v %v", goProfile, err)
	}

	generic, err := autoLanguageProfile(t.TempDir())
	if err != nil || len(generic.Checks) != 1 || generic.Checks[0] != "git diff --check" {
		t.Fatalf("generic fallback: %+v %v", generic, err)
	}

	dotnetDir := t.TempDir()
	writeFile(t, filepath.Join(dotnetDir, "Shop.sln"), "", 0o644)
	dotnet, err := autoLanguageProfile(dotnetDir)
	if err != nil || len(dotnet.TestReports) != 1 {
		t.Fatalf(".NET auto-detection: %+v %v", dotnet, err)
	}

	rustDir := t.TempDir()
	writeFile(t, filepath.Join(rustDir, "Cargo.toml"), "[workspace]\n", 0o644)
	rust, err := autoLanguageProfile(rustDir)
	if err != nil || len(rust.Checks) != 4 || !strings.HasPrefix(rust.Checks[0], "cargo fmt") || rust.Format != "cargo fmt --all" || len(rust.Commands["execute"]) == 0 {
		t.Fatalf("Rust auto-detection: %+v %v", rust, err)
	}
}

func TestQueuedProfileAutoDetectsRepo(t *testing.T) {
	p := defaultProfile("pi")
	p.Checks = []string{"go vet ./..."} // what a no-config `serve` started in a Go checkout gets
	useProfile(t, p)
	rustDir := t.TempDir()
	writeFile(t, filepath.Join(rustDir, "Cargo.toml"), "[workspace]\n", 0o644)

	got, err := queuedProfile(runRequest{Repo: rustDir})
	if err != nil || got.Checks[0] != "go vet ./..." {
		t.Fatalf("a configured server must keep its checks: %+v %v", got.Checks, err)
	}
	autoSettings = true
	t.Cleanup(func() { autoSettings = false })
	got, err = queuedProfile(runRequest{Repo: rustDir})
	if err != nil || !strings.HasPrefix(got.Checks[0], "cargo fmt") {
		t.Fatalf("a no-config server must detect the queued repo: %+v %v", got.Checks, err)
	}
	got, err = queuedProfile(runRequest{Repo: rustDir, Language: "server"})
	if err != nil || got.Checks[0] != "go vet ./..." {
		t.Fatalf("language=server must keep the server checks: %+v %v", got.Checks, err)
	}
}

func TestExampleProfiles(t *testing.T) {
	paths, err := filepath.Glob("examples/*.json")
	if err != nil || len(paths) == 0 {
		t.Fatal("missing example profiles")
	}
	for _, path := range paths {
		if _, err := loadProfile(path); err != nil {
			t.Errorf("%s: %v", path, err)
		}
	}
}

func TestSnapshotDoesNotReadChangedEnvironment(t *testing.T) {
	p := defaultProfile("pi")
	p.Checks = []string{"true"}
	p.Models["plan"] = "queued-model"
	b, _ := json.Marshal(p)
	t.Setenv("SM_RESOLVED_CONFIG", string(b))
	t.Setenv("SM_PROVIDER", "claude")
	t.Setenv("SM_CHECKS", "false")
	t.Setenv("SM_MODEL_PLAN", "changed-model")
	got, err := loadProfile("")
	if err != nil || got.Provider != "pi" || got.Checks[0] != "true" || got.Models["plan"] != "queued-model" {
		t.Fatalf("snapshot changed: %+v %v", got, err)
	}
}

func TestGenericPrompts(t *testing.T) {
	useProfile(t, defaultProfile("pi"))
	for _, state := range []State{Plan, Investigate, Execute, Review} {
		prompt := rolePrompt(state) + dispatch(&Ctx{}, steps[state].Role, "")
		for _, legacy := range []string{"rust-mm", "Ghidra", "rescue-re", "cargo fmt", "py-desmume"} {
			if strings.Contains(prompt, legacy) {
				t.Errorf("%v contains %s", state, legacy)
			}
		}
	}
}

func TestTranscriptConfiguration(t *testing.T) {
	p, err := decodeProfile([]byte(`{"checks":["true"],"transcripts":true}`), "")
	if err != nil || !p.Transcripts {
		t.Fatalf("profile transcript opt-in = %+v, %v", p, err)
	}
	useProfile(t, p)
	enabled, disabled := true, false
	for _, override := range []*bool{nil, &disabled, &enabled} {
		got, err := queuedProfile(runRequest{Transcripts: override})
		want := override == nil || *override
		if err != nil || got.Transcripts != want || !settings.Transcripts {
			t.Fatalf("queue opt-in = %+v, %v; want %v", got, err, want)
		}
		if snap := queueSnapshot(&Runner{repo: t.TempDir()}, got); snap["transcripts"] != want {
			t.Fatalf("queue default = %v, want %v", snap["transcripts"], want)
		}
	}
	path := filepath.Join(t.TempDir(), "profile.json")
	writeFile(t, path, `{"checks":["true"],"transcripts":true}`, 0o600)
	t.Setenv("SM_RESOLVED_CONFIG", "")
	for _, value := range []string{"0", "1", "false", "true", "", "invalid"} {
		t.Run("environment/"+value, func(t *testing.T) {
			t.Setenv("SM_TRANSCRIPTS", value)
			got, err := loadProfile(path)
			if value == "" || value == "invalid" {
				if err == nil || !strings.Contains(err.Error(), "SM_TRANSCRIPTS") {
					t.Fatalf("accepted invalid opt-in: %v", err)
				}
			} else if err != nil || got.Transcripts != (value == "1" || value == "true") {
				t.Fatalf("environment opt-in = %+v, %v", got, err)
			}
		})
	}
	t.Setenv("SM_TRANSCRIPTS", "true")
	auto, err := autoProfile(t.TempDir())
	if err != nil || !auto.Transcripts {
		t.Fatalf("automatic profile ignored opt-in: %+v, %v", auto, err)
	}
	t.Setenv("SM_TRANSCRIPTS", "invalid")
	if _, err := autoProfile(t.TempDir()); err == nil {
		t.Fatal("automatic profile accepted invalid opt-in")
	}
	t.Setenv("SM_RESOLVED_CONFIG", `{"checks":["true"],"transcripts":true}`)
	resolved, err := loadProfile("")
	if err != nil || !resolved.Transcripts {
		t.Fatalf("resolved opt-in was overridden by environment: %+v, %v", resolved, err)
	}
}

func TestAgentTranscripts(t *testing.T) {
	for _, provider := range []string{"pi"} {
		for _, mode := range []string{"disabled", "valid", "malformed", "failure", "startup", "timeout"} {
			t.Run(provider+"/"+mode, func(t *testing.T) {
				dir, scratch := t.TempDir(), t.TempDir()
				p := defaultProfile(provider)
				p.Transcripts = mode != "disabled"
				if mode == "timeout" {
					p.TimeoutMinutes = .002
				}
				useProfile(t, p)
				writeFile(t, filepath.Join(dir, provider), `#!/bin/sh
usage=""
session=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --usage-output-file) shift; usage="$1";;
    --session-id) shift; session="$1";;
  esac
  shift
done
if [ -n "$SM_TEST_STDIN" ]; then cat > "$SM_TEST_STDIN"; fi
printf '%s' "$SM_TEST_STDOUT"
printf 'diagnostic\nsecond line\n' >&2
if [ "$SM_TEST_MODE" = "timeout" ]; then sleep 300; fi
if [ "$SM_TEST_MODE" = "failure" ]; then exit 1; fi
`, 0o755)
				if mode == "startup" {
					writeFile(t, filepath.Join(dir, provider), "#!/agent-sm-missing-interpreter\n", 0o755)
				}
				t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
				t.Setenv("SM_STATS", filepath.Join(dir, "stats.jsonl"))
				t.Setenv("SM_TEST_MODE", mode)
				t.Setenv("SM_TEST_SECRET", "must-not-dump-environment")
				t.Setenv("SM_TEST_STDIN", filepath.Join(scratch, "stdin.txt"))
				cost := 0.1
				result := `{"verdict":"PASS","reasons":[]}`
				if mode == "malformed" {
					result = `{"verdict":"PASS"}`
				}
				raw := piOutput(t, result, &cost)
				stderr := "diagnostic\nsecond line\n"
				if mode == "startup" {
					raw, stderr = "", ""
				}
				t.Setenv("SM_TEST_STDOUT", raw)
				oldEvents := eventsPath
				eventsPath = filepath.Join(scratch, "events.jsonl")
				t.Cleanup(func() { eventsPath = oldEvents })
				c := &Ctx{Repo: dir, Scratch: scratch}
				for attempt := 0; attempt < 2; attempt++ {
					got := agent(c, Review, "exact prompt\nwith newline", new(any))
					want := Ok
					// missing-messages stays Ok: a capture gap is logged, never fails the step.
					if mode == "malformed" || mode == "failure" || mode == "startup" {
						want = Invalid
					} else if mode == "timeout" {
						want = Timeout
						if provider == "pi" {
							// A complete structured result was emitted before cancellation.
							want = Invalid
						}
					}
					if got != want {
						t.Fatalf("attempt %d = %s, want %s", attempt, got, want)
					}
					entries, err := os.ReadDir(filepath.Join(scratch, "transcripts"))
					if mode == "disabled" {
						if !os.IsNotExist(err) {
							t.Fatalf("disabled logging created transcripts: %v", err)
						}
						continue
					}
					if err != nil || len(entries) != attempt+1 {
						t.Fatalf("retry captures = %v, %v", entries, err)
					}
					for _, entry := range entries {
						capture := filepath.Join(scratch, "transcripts", entry.Name())
						for name, expected := range map[string]string{
							"stdout.log": raw, "stderr.log": stderr,
						} {
							b, err := os.ReadFile(filepath.Join(capture, name))
							if err != nil || string(b) != expected {
								t.Fatalf("%s changed: %q, %v", name, b, err)
							}
						}
						for _, name := range []string{"", "stdout.log", "stderr.log", "request.json", "result.json"} {
							info, err := os.Stat(filepath.Join(capture, name))
							if err != nil {
								t.Fatal(err)
							}
							if info.Mode().Perm()&0o077 != 0 {
								t.Fatalf("capture is not private: %s (%v)", name, info.Mode())
							}
						}
						request, err := os.ReadFile(filepath.Join(capture, "request.json"))
						if err != nil {
							t.Fatal(err)
						}
						var req struct {
							Args   []string
							Schema string
						}
						if err := json.Unmarshal(request, &req); err != nil {
							t.Fatal(err)
						}
						promptOK := strings.Contains(strings.Join(req.Args, " "), "@")
						if req.Schema != steps[Review].Schema || !promptOK ||
							strings.Contains(string(request), "must-not-dump-environment") || strings.Contains(string(request), "bypassPermissions") {
							t.Fatalf("incorrect request or permissions: %s", request)
						}
						result, err := os.ReadFile(filepath.Join(capture, "result.json"))
						if err != nil || !strings.Contains(string(result), `"outcome": "`+want.String()+`"`) {
							t.Fatalf("capture result = %s, %v", result, err)
						}
					}
				}
			})
		}
	}
}

func TestTranscriptIOFailure(t *testing.T) {
	dir := t.TempDir()
	p := defaultProfile("pi")
	p.Transcripts = true
	useProfile(t, p)
	oldEvents := eventsPath
	eventsPath = filepath.Join(dir, "events.jsonl")
	t.Cleanup(func() { eventsPath = oldEvents })
	writeFile(t, filepath.Join(dir, "transcripts"), "not a directory", 0o600)
	// Capture is diagnostics: when it cannot be opened the step still runs, and the failure is logged.
	marker := filepath.Join(dir, "started")
	writeFile(t, filepath.Join(dir, "pi"), "#!/bin/sh\ntouch '"+marker+"'\n", 0o755)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	agent(&Ctx{Repo: dir, Scratch: dir}, Review, "test", new(any))
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("capture failure stopped the step from running")
	}
	b, err := os.ReadFile(eventsPath)
	if err != nil || !strings.Contains(string(b), "transcript-error") {
		t.Fatalf("capture error not surfaced: %s, %v", b, err)
	}
	capture, err := newTranscript(&Ctx{Repo: dir, Scratch: t.TempDir()}, Review)
	if err != nil {
		t.Fatal(err)
	}
	cancelled := false
	if err := capture.request([]string{"-p", "test"}, Review, func() { cancelled = true }); err != nil {
		t.Fatal(err)
	}
	if err := capture.stdout.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := capture.stdout.Write([]byte("cannot save")); err == nil || !cancelled {
		t.Fatalf("capture write error did not cancel: %v, cancelled=%v", err, cancelled)
	}
	if err := capture.finish(Ok); err == nil {
		t.Fatal("capture write/close error was ignored")
	}
	b, err = os.ReadFile(filepath.Join(capture.dir, "result.json"))
	if err != nil || !strings.Contains(string(b), `"outcome": "ok"`) || !strings.Contains(string(b), "capture_error") {
		t.Fatalf("capture failure not recorded beside the real outcome: %s, %v", b, err)
	}
}

func TestCheckCommandsAndReports(t *testing.T) {
	dir := t.TempDir()
	p := defaultProfile("pi")
	p.TimeoutMinutes = .001
	useProfile(t, p)
	start := time.Now()
	if _, err := checkCommand(dir, "sleep 300"); err == nil || time.Since(start) > 3*time.Second {
		t.Fatalf("check timeout failed: %v", err)
	}
	for _, counters := range []string{`total="0" executed="0" passed="0"`, `total="2" executed="1" passed="1"`, `total="2" executed="2" passed="1"`} {
		writeFile(t, filepath.Join(dir, "test.trx"), `<TestRun><ResultSummary><Counters `+counters+`/></ResultSummary></TestRun>`, 0o600)
		if err := verifyTestReports(dir, []string{"*.trx"}); err == nil {
			t.Fatal("accepted empty/skipped/failed required tests")
		}
	}
	writeFile(t, filepath.Join(dir, "test.trx"), `<TestRun><ResultSummary><Counters total="2" executed="2" passed="2"/></ResultSummary></TestRun>`, 0o600)
	if err := verifyTestReports(dir, []string{"*.trx"}); err != nil {
		t.Fatal(err)
	}
	if err := verifyTestReports(t.TempDir(), []string{"*.trx"}); err == nil {
		t.Fatal("accepted stale/missing report")
	}
}

func TestUnknownUsageStopsRun(t *testing.T) {
	fake(t, map[State]func(*Ctx) Out{Plan: func(c *Ctx) Out { c.CostUnknown++; return Direct }})
	if exit := run(&Ctx{RunBudget: 20, Scratch: t.TempDir()}, Prepare); exit != GaveUp {
		t.Fatalf("unknown usage continued: %v", exit)
	}
}

func TestMain(m *testing.M) {
	sandboxOff = true // fake agents are sh scripts on the host; sandbox_test.go covers the sandbox
	if os.Getenv("AGENT_SM_TEST_HELPER") == "1" {
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestPrepareStopsOnBrokenBaseAndFilesNothing(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "init"}, {"remote", "add", "origin", repo}} {
		if out, err := sh(repo, "git", args...); err != nil {
			t.Fatalf("%s %v", out, err)
		}
	}
	calls := filepath.Join(dir, "gh-calls")
	writeFile(t, filepath.Join(dir, "gh"), `#!/bin/sh
echo "$*" >> "`+calls+`"
case "$1 $2" in
  "issue view") printf '%s\n' "{\"title\":\"docs task\",\"body\":\"$SM_TEST_BODY\",\"url\":\"u\"}";;
  "api "*) echo '[]';;
  "issue list") printf '%s' "$SM_TEST_EXISTING";;
  "issue create") echo https://example/issues/9;;
  "issue comment") ;;
  "pr list") echo '[]';;
  *) exit 1;;
esac
`, 0o755)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("SM_SCRATCH_ROOT", filepath.Join(dir, "scratch"))
	p := defaultProfile("pi")
	p.Base = "main"
	p.Checks = []string{"echo E0034 duplicate; false"}
	useProfile(t, p)
	run := func(body, existing string) (Out, string) {
		os.Remove(calls)
		t.Setenv("SM_TEST_BODY", body)
		t.Setenv("SM_TEST_EXISTING", existing)
		testGit(t, repo, "checkout", "-q", "main")
		sh(repo, "git", "branch", "-D", "sm/issue-5")
		c := &Ctx{Repo: repo, Issue: 5, Level: levelPR, Scratch: t.TempDir()}
		o := prepare(c)
		b, _ := os.ReadFile(calls)
		return o, string(b)
	}
	runDir := t.TempDir()
	savedEvents := eventsPath
	eventsPath = filepath.Join(runDir, "events.jsonl")
	o, got := run("only docs", "")
	eventsPath = savedEvents
	if o != Fail || strings.Contains(got, "issue create") || strings.Contains(got, "issue comment") {
		t.Fatalf("a broken base must stop the run and file nothing: %v\n%s", o, got)
	}
	// filing that run's diagnosis is the user's call; the bug it files may skip the base check
	t.Setenv("OPENCODE_API_KEY", "")
	os.WriteFile(filepath.Join(runDir, "diagnosis.json"), []byte(`{"exit":"errored","kind":"bug","title":"fmt fails on main","body":"b"}`), 0o644)
	os.Remove(calls)
	if url, err := fileDiagnosis(runDir, repo, 5); err != nil || url != "https://example/issues/9" {
		t.Fatalf("fileDiagnosis = %q, %v", url, err)
	}
	if b, _ := os.ReadFile(calls); !strings.Contains(string(b), fixesBaseMarker) {
		t.Fatalf("a base bug filed from a diagnosis needs %q:\n%s", fixesBaseMarker, b)
	}
	if o, got = run("fix it\\n"+fixesBaseMarker, ""); o != Ok || strings.Contains(got, "issue create") {
		t.Fatalf("the bug's own run must skip the base check: %v\n%s", o, got)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	saved := runCtx
	runCtx = cancelled
	o, got = run("only docs", "")
	runCtx = saved
	if o != Fail || strings.Contains(got, "issue create") || strings.Contains(got, "issue comment") {
		t.Fatalf("a check that never ran must not file anything: %v\n%s", o, got)
	}
	ran := filepath.Join(dir, "check-ran")
	settings.Checks = []string{"echo x >> " + ran}
	for i := 0; i < 2; i++ {
		if o, got = run("only docs", ""); o != Ok {
			t.Fatalf("passing base run %d: %v\n%s", i, o, got)
		}
	}
	if b, _ := os.ReadFile(ran); string(b) != "x\n" {
		t.Fatalf("a base tree that passed must not be checked again, check ran %q", b)
	}
	settings.Checks = []string{"false"} // the full list would fail; base_checks alone guards the base
	settings.BaseChecks = []string{"echo y >> " + ran}
	settings.TestReports = []string{"*.trx"}
	if o, got = run("only docs", ""); o != Ok {
		t.Fatalf("base_checks must replace checks on the base: %v\n%s", o, got)
	}
	if b, _ := os.ReadFile(ran); string(b) != "x\ny\n" {
		t.Fatalf("base_checks did not run once: %q", b)
	}
}

func TestQueuedCLI(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "init"}} {
		if out, err := sh(repo, "git", args...); err != nil {
			t.Fatalf("%s %v", out, err)
		}
	}
	if out, err := sh(repo, "git", "checkout", "-q", "-b", "feature"); err != nil {
		t.Fatalf("%s %v", out, err)
	}
	writeFile(t, filepath.Join(repo, "base-marker.txt"), "feature branch", 0o644)
	for _, args := range [][]string{{"add", "base-marker.txt"}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "feature"}, {"checkout", "-q", "main"}, {"remote", "add", "origin", repo}} {
		if out, err := sh(repo, "git", args...); err != nil {
			t.Fatalf("%s %v", out, err)
		}
	}
	writeFile(t, filepath.Join(dir, "gh"), `#!/bin/sh
case "$1 $2" in
  "issue view") printf '%s\n' '{"title":"fixture task","body":"Base branch: \u0060feature\u0060 (parent Story #12).\n\n## Dependencies\nSibling \u0060depends_on\u0060: \u0060[]\u0060.\n\nStory gates remain #54 -> #12/#22.\nSee #20 for context.","url":"https://github.com/example/repo/issues/1"}';;
  "api "*"/dependencies/blocked_by"*)
    if [ "$SM_TEST_NATIVE_BLOCKER" = 1 ]; then echo '[{"number":7,"state":"open"}]'; else echo '[]'; fi;;
  "pr list") echo '[]';;
  *) exit 1;;
esac
`, 0o755)
	writeFile(t, filepath.Join(dir, "pi"), `#!/bin/sh
sys=""; prev=""; last=""
for a; do
  if [ "$prev" = "--append-system-prompt" ]; then sys=$(cat "$a"); fi
  prev="$a"; last="$a"
done
prompt="$sys $(cat "${last#@}")"
emit() { printf '{"type":"turn_end","message":{"role":"assistant","stopReason":"stop","content":[{"type":"text","text":%s}],"usage":{"cost":{"total":0.1}}}}\n' "$1"; }
case "$prompt" in
  *"You are the planner"*) emit '"{\"status\":\"plan\",\"objective\":\"deliver file\",\"points\":[\"file exists\"],\"points_derived\":false,\"files\":[\"outcome.txt\"],\"verification\":\"default\",\"needs_investigation\":false,\"fits_one_pr\":true,\"units\":[]}"';;
  *"You are the implementation"*) echo delivered > outcome.txt; emit '"{\"status\":\"done\",\"summary\":\"delivered file\"}"';;
  *"You independently review"*) emit '"{\"verdict\":\"PASS\",\"reasons\":[\"file verified\"]}"';;
  *) exit 1;;
esac
`, 0o755)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AGENT_SM_TEST_HELPER", "1")
	t.Setenv("SM_SCRATCH_ROOT", filepath.Join(dir, "scratch"))
	t.Setenv("SM_TRANSCRIPTS", "false")
	p := defaultProfile("pi")
	p.Base = "main"
	p.Checks = []string{"test -f outcome.txt || git diff --quiet HEAD"} // passes on the untouched base too
	p.Transcripts = true
	useProfile(t, p)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	r := &Runner{repo: repo, self: self, stats: filepath.Join(dir, "stats.jsonl")}
	j := Job{Issue: 1, Provider: "pi", Profile: &p}
	r.runOne(&j)
	if j.Err != "" || j.Exit != "ready" {
		t.Fatalf("queued run: %+v", j)
	}
	if j.Base != "origin/feature" {
		t.Fatalf("declared base not propagated: %s", j.Base)
	}
	if _, err := os.Stat(filepath.Join(j.Worktree, "base-marker.txt")); err != nil {
		t.Fatal("run started from configured main instead of declared feature base")
	}
	staged, err := sh(j.Worktree, "git", "diff", "--cached", "--name-only")
	if err != nil || strings.TrimSpace(staged) != "outcome.txt" {
		t.Fatalf("staged result: %q %v", staged, err)
	}
	b, err := os.ReadFile(r.stats)
	if err != nil || !strings.Contains(string(b), `"provider":"pi"`) || strings.Contains(string(b), `"usage_unknown":true`) {
		t.Fatalf("stats: %s %v", b, err)
	}
	firstEvents, err := os.ReadFile(filepath.Join(j.scratch(), "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	retry := Job{Issue: 1, Provider: "pi", Profile: &p}
	r.runOne(&retry)
	if retry.Err != "" || retry.Exit != "ready" || retry.Branch != "sm/issue-1-1" || filepath.Base(retry.Worktree) != "1-sm-1" {
		t.Fatalf("queued retry: %+v", retry)
	}
	if got := testGit(t, j.Worktree, "diff", "--cached", "--name-only"); got != "outcome.txt" {
		t.Fatalf("retry changed previous staged work: %s", got)
	}
	if b, err := os.ReadFile(filepath.Join(j.scratch(), "events.jsonl")); err != nil || string(b) != string(firstEvents) {
		t.Fatal("retry replaced the previous run's events")
	}
	if latest, issue := latestRun(runScratch(repo, 1)); latest != retry.scratch() || issue != "1" {
		t.Fatalf("latest attempt = %s, issue = %s", latest, issue)
	}
	direct := Job{Issue: 1, Repo: repo, Provider: "pi", Profile: &p}
	directScratch := t.TempDir()
	cmd := exec.Command(self, repo, "1")
	cmd.Env = append(childEnv(&direct, r.stats), "SM_SCRATCH="+directScratch, "SM_EVENTS="+filepath.Join(directScratch, "events.jsonl"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("direct CLI retry: %v: %s", err, out)
	}
	if branch := testGit(t, repo, "branch", "--show-current"); branch != "sm/issue-1-2" {
		t.Fatalf("direct CLI selected %s", branch)
	}
	for _, scratch := range []string{j.scratch(), retry.scratch(), directScratch} {
		captures, err := os.ReadDir(filepath.Join(scratch, "transcripts"))
		if err != nil || len(captures) != 3 {
			t.Fatalf("immutable transcript setting lost in child: %s: %v, %v", scratch, captures, err)
		}
	}
	t.Setenv("SM_TEST_NATIVE_BLOCKER", "1")
	blocked := Job{Issue: 1, Provider: "pi", Profile: &p}
	r.runOne(&blocked)
	if blocked.Exit != "errored" || blocked.Err != "blocked by open GitHub dependency #7" {
		t.Fatalf("native blocker did not surface in queue: %+v", blocked)
	}
	events, err := os.ReadFile(filepath.Join(blocked.scratch(), "events.jsonl"))
	if err != nil || strings.Contains(string(events), `"event":"agent"`) {
		t.Fatalf("blocked run started an agent: %s %v", events, err)
	}
	// No network-backed GitHub mutation is implemented by this fixture.
}

func TestChecksPreserveCandidate(t *testing.T) {
	for _, tc := range []struct {
		name, command, format string
		want                  Out
	}{
		{"pass with spaces", "echo WARN shown", "", Pass},
		{"failed check", "false", "", Fail},
		{"modified worktree", "printf changed > 'allowed name.txt'", "", Fail},
		{"modified index", "printf outside > outside.txt && git add outside.txt", "", Fail},
		{"formatter failure", "true", "false", Fail},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := t.TempDir()
			for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "init"}} {
				if out, err := sh(repo, "git", args...); err != nil {
					t.Fatalf("%s %v", out, err)
				}
			}
			writeFile(t, filepath.Join(repo, "allowed name.txt"), "candidate", 0o644)
			p := defaultProfile("pi")
			p.Checks = []string{tc.command}
			p.Format = tc.format
			useProfile(t, p)
			c := &Ctx{Repo: repo, Scratch: t.TempDir(), Files: []string{"allowed name.txt"}}
			if got := check(c); got != tc.want {
				t.Fatalf("got %v want %v: %s", got, tc.want, c.Feedback)
			}
			if tc.want == Pass && !strings.Contains(c.CheckOut, "$ "+tc.command+"\nWARN shown\n") {
				t.Fatalf("passing check output not kept for the reviewer: %q", c.CheckOut)
			}
		})
	}
}

func TestUsageReportCountsUnknownUsage(t *testing.T) {
	got := report([]byte(`{"state":"execute","provider":"pi","cost_usd":1.5}
{"state":"execute","provider":"pi","usage_unknown":true}
{"state":"execute","provider":"pi","cost_usd":0.2}`))
	for _, want := range []string{"pi/execute", "$1.700 known total", "1 unknown-usage"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in %s", want, got)
		}
	}
}

func TestConcurrencyEnv(t *testing.T) {
	t.Setenv("SM_CONCURRENCY", "3")
	p := defaultProfile("pi")
	if err := p.applyLimitEnv(); err != nil || p.Concurrency != 3 {
		t.Fatalf("concurrency = %d, %v", p.Concurrency, err)
	}
	p.Concurrency = 17
	if p.validate() == nil {
		t.Fatal("concurrency 17 accepted")
	}
}

func TestHandledElsewhereStopsDuplicateRuns(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "gh"), `#!/bin/sh
case "$1 $2" in
  "issue view") echo "$SM_TEST_STATE";;
  "pr list") printf '%s' "$SM_TEST_PRS";;
  *) exit 1;;
esac
`, 0o755)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, tc := range []struct{ state, prs, branch, want string }{
		{"OPEN", `[]`, "", ""},
		{"CLOSED", `[]`, "sm/issue-5-2", "issue #5 is closed"},
		{"OPEN", `[{"headRefName":"sm/issue-5-1","url":"u1"}]`, "sm/issue-5-2", "another attempt's PR is open: u1"},
		{"OPEN", `[{"headRefName":"sm/issue-5-2","url":"own"},{"headRefName":"sm/issue-50","url":"other"}]`, "sm/issue-5-2", ""},
		{"OPEN", `not json`, "", ""}, // gh trouble fails open
	} {
		t.Setenv("SM_TEST_STATE", tc.state)
		t.Setenv("SM_TEST_PRS", tc.prs)
		if got := handledElsewhere(&Ctx{Repo: dir, Issue: 5, Branch: tc.branch, Scratch: t.TempDir()}); got != tc.want {
			t.Errorf("%+v: got %q", tc, got)
		}
	}
	var v [numStates]int
	for _, s := range []State{Prepare, Publish} {
		if n := next(s, Blocked, v); n.Exit != GaveUp {
			t.Errorf("%v blocked must give up, got %+v", s, n)
		}
	}
}

func TestWorktreeLinks(t *testing.T) {
	repo, dir := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "pmdb.nds"), []byte("rom"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SM_LINKS", "pmdb.nds") // listed twice: the second is skipped
	if err := linkFiles(repo, dir, []string{"pmdb.nds", "missing.bin"}); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "pmdb.nds")); err != nil || string(b) != "rom" {
		t.Fatalf("pmdb.nds not linked: %q, %v", b, err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "missing.bin")); err == nil {
		t.Fatal("linked a file the main checkout does not have")
	}
	bad := []string{"../x", "/etc/passwd", ".", "a/../../x"}
	if runtime.GOOS == "windows" { // names that only leave the repo on Windows
		bad = append(bad, `\\server\share\x`, `C:\x`, `C:x`, `..\x`)
	}
	for _, bad := range bad {
		p := defaultProfile("pi")
		p.Checks, p.Links = []string{"true"}, []string{bad}
		if p.validate() == nil {
			t.Fatalf("accepted link %q", bad)
		}
	}
}
