package main

import (
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
		t.Skipf("%s needs a Unix shell; run the suite on Linux, macOS or WSL", filepath.Base(path))
	}
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}

func copilotOutput(t *testing.T, content string) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{"type": "assistant.message", "data": map[string]string{"content": content}})
	if err != nil {
		t.Fatal(err)
	}
	return string(b) + "\n" + "{\"type\":\"result\",\"exitCode\":0}\n"
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
	if p.Provider != "copilot" || p.NotesDir != filepath.Join(dir, "notes") || p.Models["execute"] != "override-model" {
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
	writeFile(t, path, `{"checks":["true"]}`, 0o600)
	t.Setenv("SM_BUDGET", "2")
	if _, err := loadProfile(path); err == nil {
		t.Fatal("silently interpreted dollars as Copilot credits")
	}
}

func TestQueuedProfile(t *testing.T) {
	p := defaultProfile("copilot")
	p.Checks = []string{"true"}
	useProfile(t, p)
	got, err := queuedProfile(runRequest{Provider: "copilot", Budget: "150", Models: map[string]string{"execute": "chosen-model"}})
	if err != nil || got.Models["execute"] != "chosen-model" || got.SessionLimit != 150 {
		t.Fatalf("queued overrides: %+v %v", got, err)
	}
	if len(settings.Models) != 0 || settings.SessionLimit != 100 {
		t.Fatal("queue mutated the server's profile")
	}
	for _, req := range []runRequest{
		{Audit: true}, {Budget: "0"}, {Budget: "99.99"}, {Budget: "NaN"},
		{Models: map[string]string{"check": "model"}},
	} {
		if _, err := queuedProfile(req); err == nil {
			t.Errorf("accepted invalid queued override %+v", req)
		}
	}
	got, err = queuedProfile(runRequest{Provider: "claude"})
	if err != nil || got.Provider != "claude" || got.SessionLimit != 2 || got.RunLimit != 20 {
		t.Fatalf("provider selection: %+v %v", got, err)
	}
}

func TestProviderLimitDefaultsAndValidation(t *testing.T) {
	for _, tc := range []struct {
		name, provider string
		session, total float64
		wantErr        string
	}{
		{"Copilot minimum", "copilot", 100, 100, ""},
		{"Copilot unlimited total", "copilot", 100, 0, ""},
		{"Copilot below session minimum", "copilot", 99.99, 500, "session_limit must be at least 100"},
		{"Copilot below total minimum", "copilot", 100, 99.99, "run_limit must be 0 (unlimited) or at least 100"},
		{"Claude unaffected", "claude", 1, 5, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := defaultProfile(tc.provider)
			p.Checks, p.SessionLimit, p.RunLimit = []string{"true"}, tc.session, tc.total
			err := p.validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("validation = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestLoadProfileProviderLimitDefaults(t *testing.T) {
	t.Setenv("SM_RESOLVED_CONFIG", "")
	for _, tc := range []struct {
		name, body, provider string
		session, total       float64
		wantErr              string
	}{
		{"Copilot defaults", `{"checks":["true"]}`, "", 100, 500, ""},
		{"Claude defaults", `{"provider":"claude","checks":["true"]}`, "", 2, 20, ""},
		{"environment selects Copilot", `{"provider":"claude","checks":["true"]}`, "copilot", 100, 500, ""},
		{"environment selects Claude", `{"provider":"copilot","checks":["true"]}`, "claude", 2, 20, ""},
		{"explicit Copilot session", `{"checks":["true"],"session_limit":99}`, "", 0, 0, "session_limit must be at least 100"},
		{"explicit Copilot total", `{"checks":["true"],"run_limit":99}`, "", 0, 0, "run_limit must be 0 (unlimited) or at least 100"},
		{"explicit Claude limits", `{"provider":"claude","checks":["true"],"session_limit":1,"run_limit":5}`, "", 1, 5, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SM_PROVIDER", tc.provider)
			path := filepath.Join(t.TempDir(), "profile.json")
			writeFile(t, path, tc.body, 0o600)
			p, err := loadProfile(path)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("loadProfile = %+v, %v; want %q", p, err, tc.wantErr)
				}
			} else if err != nil || p.SessionLimit != tc.session || p.RunLimit != tc.total {
				t.Fatalf("loadProfile = %+v, %v; want limits %v / %v", p, err, tc.session, tc.total)
			}
		})
	}
	t.Run("environment limit is not raised", func(t *testing.T) {
		t.Setenv("SM_PROVIDER", "copilot")
		t.Setenv("SM_SESSION_LIMIT", "99")
		path := filepath.Join(t.TempDir(), "profile.json")
		writeFile(t, path, `{"checks":["true"]}`, 0o600)
		if _, err := loadProfile(path); err == nil || !strings.Contains(err.Error(), "session_limit must be at least 100") {
			t.Fatalf("invalid environment limit accepted: %v", err)
		}
	})
	t.Run("snapshot uses its own provider", func(t *testing.T) {
		t.Setenv("SM_PROVIDER", "copilot")
		t.Setenv("SM_RESOLVED_CONFIG", `{"provider":"claude","checks":["true"]}`)
		p, err := loadProfile("")
		if err != nil || p.Provider != "claude" || p.SessionLimit != 2 || p.RunLimit != 20 {
			t.Fatalf("snapshot = %+v, %v", p, err)
		}
	})
}

func TestQueuedProviderLimits(t *testing.T) {
	p := defaultProfile("claude")
	p.Checks, p.SessionLimit, p.RunLimit = []string{"true"}, 1, 5
	useProfile(t, p)
	copilot, err := queuedProfile(runRequest{Provider: "copilot"})
	if err != nil || copilot.SessionLimit != 100 || copilot.RunLimit != 500 {
		t.Fatalf("Copilot switch = %+v, %v", copilot, err)
	}
	snapshot := queueSnapshot(&Runner{repo: t.TempDir()}, copilot)
	if snapshot["session_limit"] != float64(100) || snapshot["run_limit"] != float64(500) || snapshot["min_session_limit"] != 100 {
		t.Fatalf("Copilot queue limits = %v", snapshot)
	}
	claude, err := queuedProfile(runRequest{Provider: "claude", Budget: "3.5"})
	if err != nil || claude.SessionLimit != 3.5 || claude.RunLimit != 5 {
		t.Fatalf("Claude limits = %+v, %v", claude, err)
	}
	claude, err = queuedProfile(runRequest{Provider: "claude", Budget: "0.001"})
	if err != nil || claude.SessionLimit != 0.001 {
		t.Fatalf("small Claude limit = %+v, %v", claude, err)
	}
}

func TestQueuedLanguageProfile(t *testing.T) {
	p := defaultProfile("copilot")
	p.Checks = []string{"true"}
	useProfile(t, p)
	got, err := queuedProfile(runRequest{Language: "go", Provider: "claude"})
	if err != nil || got.Provider != "claude" || len(got.Checks) != 3 || !strings.Contains(got.Checks[0], "gofmt") {
		t.Fatalf("Go profile selection: %+v %v", got, err)
	}
	got, err = queuedProfile(runRequest{Language: "dotnet", Provider: "copilot"})
	if err != nil || len(got.TestReports) != 1 || !strings.HasPrefix(got.Checks[0], "dotnet build") {
		t.Fatalf(".NET profile selection: %+v %v", got, err)
	}
	if _, err := queuedProfile(runRequest{Language: "python"}); err == nil {
		t.Fatal("accepted an unknown language profile")
	}
}

func TestCanonicalRepo(t *testing.T) {
	dir := t.TempDir()
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
	root := t.TempDir()
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
	p := defaultProfile("claude")
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

func TestAutoProfileProviderLimits(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	t.Setenv("SM_PROVIDER", "")
	writeFile(t, filepath.Join(dir, "claude"), "#!/bin/sh\n", 0o755)
	p, err := autoProfile(t.TempDir())
	if err != nil || p.Provider != "claude" || p.SessionLimit != 2 || p.RunLimit != 20 {
		t.Fatalf("automatic Claude profile = %+v, %v", p, err)
	}
	writeFile(t, filepath.Join(dir, "copilot"), "#!/bin/sh\n", 0o755)
	p, err = autoProfile(t.TempDir())
	if err != nil || p.Provider != "copilot" || p.SessionLimit != 100 || p.RunLimit != 500 {
		t.Fatalf("automatic Copilot profile = %+v, %v", p, err)
	}
	t.Run("explicit limits are honored", func(t *testing.T) {
		t.Setenv("SM_SESSION_LIMIT", "125")
		t.Setenv("SM_RUN_LIMIT", "300")
		p, err := autoProfile(t.TempDir())
		if err != nil || p.SessionLimit != 125 || p.RunLimit != 300 {
			t.Fatalf("automatic profile ignored configured limits: %+v, %v", p, err)
		}
	})
	t.Run("lower limit is rejected", func(t *testing.T) {
		t.Setenv("SM_RUN_LIMIT", "20")
		if _, err := autoProfile(t.TempDir()); err == nil || !strings.Contains(err.Error(), "run_limit must be 0 (unlimited) or at least 100") {
			t.Fatalf("automatic profile silently raised run limit: %v", err)
		}
	})
	t.Run("explicit provider keeps its units", func(t *testing.T) {
		t.Setenv("SM_PROVIDER", "claude")
		t.Setenv("SM_SESSION_LIMIT", "1")
		t.Setenv("SM_RUN_LIMIT", "5")
		p, err := autoProfile(t.TempDir())
		if err != nil || p.Provider != "claude" || p.SessionLimit != 1 || p.RunLimit != 5 {
			t.Fatalf("automatic profile ignored selected provider: %+v, %v", p, err)
		}
	})
}

func TestClaudeLegacyEnvironment(t *testing.T) {
	t.Setenv("SM_PROVIDER", "claude")
	t.Setenv("SM_CHECKS", "true")
	t.Setenv("SM_EXTRA_ALLOW", "Bash(custom check:*)")
	t.Setenv("SM_AUDIT", "1")
	p, err := loadProfile("")
	if err != nil || p.ClaudeExtra != "Bash(custom check:*)" || !p.Audit {
		t.Fatalf("legacy overrides lost: %+v %v", p, err)
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
	p := defaultProfile("copilot")
	p.Checks = []string{"true"}
	p.Models["plan"] = "queued-model"
	b, _ := json.Marshal(p)
	t.Setenv("SM_RESOLVED_CONFIG", string(b))
	t.Setenv("SM_PROVIDER", "claude")
	t.Setenv("SM_CHECKS", "false")
	t.Setenv("SM_MODEL_PLAN", "changed-model")
	got, err := loadProfile("")
	if err != nil || got.Provider != "copilot" || got.Checks[0] != "true" || got.Models["plan"] != "queued-model" {
		t.Fatalf("snapshot changed: %+v %v", got, err)
	}
}

func TestCopilotPolicy(t *testing.T) {
	p := defaultProfile("copilot")
	p.Commands["execute"] = []string{"dotnet test"}
	useProfile(t, p)
	execute := strings.Join(copilotArgs(Execute, "task", "/tmp/usage", 1), "\n")
	review := strings.Join(copilotArgs(Review, "task", "/tmp/usage", 1), "\n")
	if !strings.Contains(execute, "--allow-tool=shell(dotnet test:*)") ||
		strings.Contains(review, "--allow-tool=shell(dotnet test:*)") ||
		!strings.Contains(review, "--deny-tool=write") ||
		!strings.Contains(execute, "--deny-tool=shell(rtk proxy git push:*)") {
		t.Fatal("incorrect role-specific command policy")
	}
	for _, bad := range []string{"--allow-all", "--trust-folder", "--allow-tool=shell(dotnet:*)"} {
		if strings.Contains(execute, bad) {
			t.Errorf("overbroad policy: %s", bad)
		}
		if !strings.Contains(execute, "--output-format=json") || strings.Contains(execute, "--silent") {
			t.Fatal("structured responses must not use the CLI's rendered text output")
		}
	}
	t.Setenv("COPILOT_ALLOW_ALL", "true")
	t.Setenv("JIRA_API_TOKEN", "test-token")
	env := strings.Join(cleanAgentEnv(), "\n")
	if strings.Contains(env, "test-token") || strings.Contains(env, "COPILOT_ALLOW_ALL=true") {
		t.Fatal("inherited bypass or Jira credential reached agent")
	}
}

func TestGenericPrompts(t *testing.T) {
	useProfile(t, defaultProfile("copilot"))
	for _, state := range []State{Plan, Investigate, Execute, Review} {
		prompt := rolePrompt(state) + dispatch(&Ctx{}, steps[state].Role, "")
		for _, legacy := range []string{"rust-mm", "Ghidra", "rescue-re", "cargo fmt", "py-desmume"} {
			if strings.Contains(prompt, legacy) {
				t.Errorf("%v contains %s", state, legacy)
			}
		}
	}
}

func TestCopilotUsage(t *testing.T) {
	credits, model, err := copilotUsage([]byte(`{"totalNanoAiu":250000000,"modelMetrics":{"actual-model":{}}}`))
	if err != nil || credits != .25 || model != "actual-model" {
		t.Fatalf("%v %s %v", credits, model, err)
	}
	for _, raw := range []string{`{}`, `{"totalNanoAiu":null}`, `{"totalNanoAiu":-1}`, `invalid`} {
		if _, _, err := copilotUsage([]byte(raw)); err == nil {
			t.Errorf("accepted unknown usage %s", raw)
		}
	}
}

func TestCopilotResult(t *testing.T) {
	content := `{"verdict":"PASS","reasons":["` + strings.Repeat("long response ", 30) +
		`*.cs **literal Markdown** \n\"quoted\" C:\\source\\file.cs café"]}`
	terminal := "{\"type\":\"result\",\"exitCode\":0}\n"
	stream := copilotOutput(t, content)
	message := strings.TrimSuffix(stream, terminal)
	for _, tc := range []struct {
		name, stream string
		valid        bool
	}{
		{"unrendered JSON survives", stream, true},
		{"tool turns before final answer", "{\"type\":\"assistant.turn_start\",\"data\":{}}\n" +
			"{\"type\":\"assistant.message\",\"data\":{\"content\":\"Inspecting files\",\"toolRequests\":[{\"name\":\"view\"}]}}\n" +
			"{\"type\":\"tool.execution_complete\",\"data\":{\"result\":{\"content\":\"tool output\"}}}\n" +
			"{\"type\":\"assistant.message_delta\",\"data\":{\"deltaContent\":\"partial\"}}\n" + stream, true},
		{"ignore child answer", message + "{\"type\":\"assistant.message\",\"agentId\":\"child\",\"data\":{\"content\":\"not the parent answer\"}}\n" + terminal, true},
		{"ignore legacy child answer", message + "{\"type\":\"assistant.message\",\"data\":{\"content\":\"not the parent answer\",\"parentToolCallId\":\"tool\"}}\n" + terminal, true},
		{"last main answer wins", strings.TrimSuffix(copilotOutput(t, "earlier commentary"), terminal) + stream, true},
		{"empty stream", "", false},
		{"rendered plain JSON is not the protocol", content, false},
		{"missing completion", message, false},
		{"missing answer", terminal, false},
		{"nonzero native exit", message + "{\"type\":\"result\",\"exitCode\":1}\n", false},
		{"missing native exit", message + "{\"type\":\"result\"}\n", false},
		{"null native exit", message + "{\"type\":\"result\",\"exitCode\":null}\n", false},
		{"duplicate completion", stream + terminal, false},
		{"event after completion", stream + "{\"type\":\"session.idle\",\"data\":{}}\n", false},
		{"truncated envelope", message + "{\"type\":", false},
		{"non JSON diagnostics", "warning: unexpected text\n" + stream, false},
		{"missing event type", "{}\n" + stream, false},
		{"session error", message + "{\"type\":\"session.error\",\"data\":{\"message\":\"failed\"}}\n" + terminal, false},
		{"tool result is not an answer", "{\"type\":\"tool.execution_complete\",\"data\":{\"content\":\"{}\"}}\n" + terminal, false},
		{"child answer only", "{\"type\":\"assistant.message\",\"agentId\":\"child\",\"data\":{\"content\":\"{}\"}}\n" + terminal, false},
		{"uncompleted new turn", message + "{\"type\":\"assistant.turn_start\",\"data\":{}}\n" + terminal, false},
		{"uncompleted delta", message + "{\"type\":\"assistant.message_delta\",\"data\":{\"deltaContent\":\"partial\"}}\n" + terminal, false},
		{"pending tool call", message + "{\"type\":\"assistant.message\",\"data\":{\"content\":\"{}\",\"toolRequests\":[{\"name\":\"view\"}]}}\n" + terminal, false},
		{"missing content", message + "{\"type\":\"assistant.message\",\"data\":{}}\n" + terminal, false},
		{"empty final answer", message + copilotOutput(t, ""), false},
		{"no fallback to earlier valid JSON", message + copilotOutput(t, "not JSON"), false},
		{"malformed model JSON", copilotOutput(t, "{\"verdict\":\"PASS\",\"reasons\":[\"raw\nnewline\"]}"), false},
		{"off schema model JSON", copilotOutput(t, `{"verdict":"PASS"}`), false},
		{"duplicate model keys", copilotOutput(t, `{"verdict":"PASS","verdict":"REVISE","reasons":[]}`), false},
		{"fenced model JSON", copilotOutput(t, "```json\n"+content+"\n```"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := copilotResult([]byte(tc.stream))
			if err == nil {
				err = validateResult(got, steps[Review].Schema, new(any))
			}
			if !tc.valid {
				if err == nil {
					t.Fatal("accepted invalid or incomplete result")
				}
				return
			}
			if err != nil || string(got) != content {
				t.Fatalf("changed or rejected raw message: %q, %v", got, err)
			}
		})
	}
}

func TestProviderResults(t *testing.T) {
	for _, provider := range []string{"claude", "copilot"} {
		for _, mode := range []string{"valid", "incomplete", "failure", "unknown-usage", "budget", "resumed"} {
			if mode == "resumed" && provider == "copilot" {
				continue // Claude-only stream shape
			}
			t.Run(provider+"/"+mode, func(t *testing.T) {
				dir := t.TempDir()
				p := defaultProfile(provider)
				p.SessionLimit = 1
				runBudget := 5.0
				if provider == "copilot" {
					p.SessionLimit, runBudget = 100, 500
				}
				useProfile(t, p)
				writeFile(t, filepath.Join(dir, provider), `#!/bin/sh
usage=""
while [ "$#" -gt 0 ]; do
  if [ "$1" = "--usage-output-file" ]; then shift; usage="$1"; fi
  shift
done
if [ "$PROVIDER" = "copilot" ]; then
  if [ "$FIXTURE_MODE" != "unknown-usage" ]; then
    amount=100000000
    if [ "$FIXTURE_MODE" = "budget" ]; then amount=100000000000; fi
    printf '{"totalNanoAiu":%s,"modelMetrics":{"actual-model":{}}}' "$amount" > "$usage"
  fi
  if [ "$FIXTURE_MODE" = "incomplete" ]; then
    echo '{"type":"assistant.message","data":{"content":"{}"}}'
  else
    echo '{"type":"assistant.message","data":{"content":"{\"verdict\":\"PASS\",\"reasons\":[]}"}}'
  fi
  echo '{"type":"result","exitCode":0}'
else
  cost=',"total_cost_usd":0.1'
  if [ "$FIXTURE_MODE" = "unknown-usage" ]; then cost=""; fi
  subtype=success
  if [ "$FIXTURE_MODE" = "budget" ]; then subtype=error_max_budget_usd; fi
  result='{"verdict":"PASS","reasons":[]}'
  if [ "$FIXTURE_MODE" = "incomplete" ]; then result='{}'; fi
  # A background task finishing resumes the session: an earlier, superseded result comes first.
  if [ "$FIXTURE_MODE" = "resumed" ]; then echo '{"type":"result","subtype":"success","is_error":false,"num_turns":45,"total_cost_usd":0.04,"structured_output":{"verdict":"FAIL","reasons":["early"]}}'; fi
  printf '{"type":"result","subtype":"%s","is_error":false%s,"structured_output":%s}\n' "$subtype" "$cost" "$result"
fi
if [ "$FIXTURE_MODE" = "failure" ]; then exit 1; fi
`, 0o755)
				t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
				t.Setenv("SM_STATS", filepath.Join(dir, "stats.jsonl"))
				t.Setenv("PROVIDER", provider)
				t.Setenv("FIXTURE_MODE", mode)
				c := &Ctx{Repo: dir, Scratch: dir, RunBudget: runBudget}
				var r struct {
					Verdict string
					Reasons []string
				}
				got := agent(c, Review, "bounded test", &r)
				want := Ok
				if mode == "incomplete" || mode == "failure" {
					want = Invalid
				}
				if mode == "budget" {
					want = OverBudget
				}
				if got != want {
					t.Fatalf("got %v want %v", got, want)
				}
				if mode == "unknown-usage" && c.CostUnknown != 1 {
					t.Fatal("missing usage treated as free")
				}
				if mode == "resumed" && (r.Verdict != "PASS" || c.Cost != 0.1 || c.CostUnknown != 0) {
					t.Fatalf("resumed session: verdict %q cost %v unknown %d; want last result, cumulative cost", r.Verdict, c.Cost, c.CostUnknown)
				}
			})
		}
	}
}

func TestAuditDefault(t *testing.T) {
	for _, tc := range []struct {
		name, profile, env string
		want, wantErr      bool
	}{
		{"claude default", `{"provider":"claude","checks":["true"]}`, "-", true, false},
		{"copilot default", `{"provider":"copilot","checks":["true"]}`, "-", false, false},
		{"profile off", `{"provider":"claude","checks":["true"],"audit":false}`, "-", false, false},
		{"env off", `{"provider":"claude","checks":["true"]}`, "0", false, false},
		{"env on copilot", `{"provider":"copilot","checks":["true"]}`, "1", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "profile.json")
			writeFile(t, path, tc.profile, 0o600)
			t.Setenv("SM_RESOLVED_CONFIG", "")
			t.Setenv("SM_PROVIDER", "")
			t.Setenv("SM_AUDIT", "")
			if tc.env == "-" {
				os.Unsetenv("SM_AUDIT")
			} else {
				t.Setenv("SM_AUDIT", tc.env)
			}
			p, err := loadProfile(path)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if err == nil && p.Audit != tc.want {
				t.Fatalf("audit = %v, want %v", p.Audit, tc.want)
			}
		})
	}
}

func TestTranscriptConfiguration(t *testing.T) {
	p, err := decodeProfile([]byte(`{"checks":["true"],"transcripts":true}`), "")
	if err != nil || !p.Transcripts || p.Audit {
		t.Fatalf("profile transcript opt-in = %+v, %v", p, err)
	}
	useProfile(t, p)
	enabled, disabled := true, false
	for _, override := range []*bool{nil, &disabled, &enabled} {
		got, err := queuedProfile(runRequest{Transcripts: override})
		want := override == nil || *override
		if err != nil || got.Transcripts != want || got.Audit || !settings.Transcripts {
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
	for _, provider := range []string{"claude", "copilot"} {
		for _, mode := range []string{"disabled", "valid", "malformed", "failure", "startup", "timeout", "missing-messages"} {
			if provider == "claude" && mode == "missing-messages" {
				continue
			}
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
if [ -n "$usage" ]; then
  printf '{"totalNanoAiu":100000000,"modelMetrics":{"fixture":{}}}' > "$usage"
fi
if [ -n "$session" ] && [ "$SM_TEST_MODE" != "missing-messages" ]; then
  mkdir -p "$COPILOT_HOME/session-state/$session"
  printf '%s' "$SM_TEST_MESSAGES" > "$COPILOT_HOME/session-state/$session/events.jsonl"
fi
printf '%s' "$SM_TEST_STDOUT"
printf 'diagnostic\nsecond line\n' >&2
if [ "$SM_TEST_MODE" = "timeout" ]; then sleep 300; fi
if [ "$SM_TEST_MODE" = "failure" ]; then exit 1; fi
`, 0o755)
				if mode == "startup" {
					writeFile(t, filepath.Join(dir, provider), "#!/agent-sm-missing-interpreter\n", 0o755)
				}
				t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
				t.Setenv("COPILOT_HOME", filepath.Join(dir, "copilot-home"))
				t.Setenv("SM_STATS", filepath.Join(dir, "stats.jsonl"))
				t.Setenv("SM_TEST_MODE", mode)
				t.Setenv("SM_TEST_SECRET", "must-not-dump-environment")
				messages := "{\"type\":\"assistant.message\",\"data\":{\"content\":\"tool discussion\"}}\n{\"type\":\"tool.execution_complete\",\"data\":{\"result\":\"tool output\"}}\n"
				t.Setenv("SM_TEST_MESSAGES", messages)
				raw := "{\"verdict\":\"PASS\",\"reasons\":[]}\n"
				if mode == "malformed" {
					raw = "{\"verdict\":\"PASS\",\"reasons\":[\"raw\nnewline\"]}\n"
				}
				if provider == "copilot" {
					raw = copilotOutput(t, raw)
				} else {
					result := `{"verdict":"PASS","reasons":[]}`
					if mode == "malformed" {
						result = `{"verdict":"PASS"}`
					}
					raw = "unparseable line\n" +
						"{\"type\":\"assistant\",\"message\":{\"content\":[{\"type\":\"text\",\"text\":\"working\"}]}}\n" +
						"{\"type\":\"user\",\"message\":{\"content\":[{\"type\":\"tool_result\",\"content\":\"tool output\"}]}}\n" +
						"{\"type\":\"result\",\"is_error\":false,\"total_cost_usd\":0.1,\"structured_output\":" + result + "}\n"
				}
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
						if provider == "claude" {
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
						if req.Schema != steps[Review].Schema || !strings.Contains(strings.Join(req.Args, "\n"), "exact prompt\nwith newline") ||
							strings.Contains(string(request), "must-not-dump-environment") || strings.Contains(string(request), "bypassPermissions") {
							t.Fatalf("incorrect request or permissions: %s", request)
						}
						result, err := os.ReadFile(filepath.Join(capture, "result.json"))
						if err != nil || !strings.Contains(string(result), `"outcome": "`+want.String()+`"`) {
							t.Fatalf("capture result = %s, %v", result, err)
						}
						if provider == "copilot" {
							if !strings.Contains(string(request), "--no-remote-export") || strings.Contains(string(request), "--share") {
								t.Fatal("transcript enabled remote sharing")
							}
							if mode == "missing-messages" || mode == "startup" {
								if !strings.Contains(string(result), "messages_error") {
									t.Fatalf("missing messages not reported: %s", result)
								}
								continue
							}
							b, err := os.ReadFile(filepath.Join(capture, "messages.jsonl"))
							if err != nil || string(b) != messages {
								t.Fatalf("native messages changed: %q, %v", b, err)
							}
							b, err = os.ReadFile(filepath.Join(capture, "usage.json"))
							if err != nil || !strings.Contains(string(b), `"totalNanoAiu":100000000`) {
								t.Fatalf("native usage missing: %s, %v", b, err)
							}
						}
					}
				}
			})
		}
	}
}

func TestTranscriptIOFailure(t *testing.T) {
	dir := t.TempDir()
	p := defaultProfile("claude")
	p.Transcripts = true
	useProfile(t, p)
	oldEvents := eventsPath
	eventsPath = filepath.Join(dir, "events.jsonl")
	t.Cleanup(func() { eventsPath = oldEvents })
	writeFile(t, filepath.Join(dir, "transcripts"), "not a directory", 0o600)
	// Capture is diagnostics: when it cannot be opened the step still runs, and the failure is logged.
	marker := filepath.Join(dir, "started")
	writeFile(t, filepath.Join(dir, "claude"), "#!/bin/sh\ntouch '"+marker+"'\n", 0o755)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
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

func TestCopilotStepCreditLimit(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		session, total, used float64
		wantLimit            string
	}{
		{"unlimited total", 100, 0, 0, "100"},
		{"remaining above minimum", 200, 500, 375, "125"},
		{"remaining at minimum", 100, 500, 400, "100"},
		{"remaining below minimum", 100, 500, 400.01, ""},
		{"invalid session limit", 99, 0, 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			capture := filepath.Join(dir, "limit")
			writeFile(t, filepath.Join(dir, "copilot"), `#!/bin/sh
while [ "$#" -gt 0 ]; do
  case "$1" in
    --max-ai-credits) shift; printf '%s' "$1" > "$SM_TEST_CAPTURE";;
    --usage-output-file) shift; usage="$1";;
  esac
  shift
done
printf '{"totalNanoAiu":100000000,"modelMetrics":{}}' > "$usage"
printf '%s\n' '{"type":"assistant.message","data":{"content":"{\"verdict\":\"PASS\",\"reasons\":[]}"}}' '{"type":"result","exitCode":0}'
`, 0o755)
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("SM_TEST_CAPTURE", capture)
			t.Setenv("SM_STATS", filepath.Join(dir, "stats.jsonl"))
			oldEvents := eventsPath
			eventsPath = filepath.Join(dir, "events.jsonl")
			t.Cleanup(func() { eventsPath = oldEvents })
			p := defaultProfile("copilot")
			p.SessionLimit = tc.session
			useProfile(t, p)
			c := &Ctx{Repo: dir, Scratch: dir, RunBudget: tc.total, Cost: tc.used}
			var result struct {
				Verdict string
				Reasons []string
			}
			outcome := agent(c, Review, "credit limit test", &result)
			if tc.wantLimit == "" {
				if outcome != OverBudget {
					t.Fatalf("outcome = %v, want over-budget", outcome)
				}
				if _, err := os.Stat(capture); !os.IsNotExist(err) {
					t.Fatal("Copilot started with less than 100 credits")
				}
				b, err := os.ReadFile(eventsPath)
				if err != nil || !strings.Contains(string(b), "at least 100 AI credits") {
					t.Fatalf("missing budget diagnostic: %s %v", b, err)
				}
				return
			}
			b, err := os.ReadFile(capture)
			if outcome != Ok || err != nil || string(b) != tc.wantLimit {
				t.Fatalf("outcome = %v, passed limit = %s (%v); want %s", outcome, b, err, tc.wantLimit)
			}
		})
	}
}

func TestCheckCommandsAndReports(t *testing.T) {
	dir := t.TempDir()
	p := defaultProfile("copilot")
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
	if exit := run(&Ctx{RunBudget: 20}, Prepare); exit != GaveUp {
		t.Fatalf("unknown usage continued: %v", exit)
	}
}

func TestMain(m *testing.M) {
	if os.Getenv("AGENT_SM_TEST_HELPER") == "1" {
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestPrepareStopsOnBrokenBaseAndFilesOneBug(t *testing.T) {
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
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	t.Setenv("SM_SCRATCH_ROOT", filepath.Join(dir, "scratch"))
	p := defaultProfile("claude")
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
	o, got := run("only docs", "")
	if o != Fail || !strings.Contains(got, "issue create --title Base main check failed: echo E0034 duplicate; false") ||
		!strings.Contains(got, "Blocks: #5") || !strings.Contains(got, "issue comment 5 --body Blocked") {
		t.Fatalf("broken base: %v\n%s", o, got)
	}
	if o, got = run("only docs", "https://example/issues/9"); o != Fail || strings.Contains(got, "issue create") ||
		!strings.Contains(got, "issue comment https://example/issues/9 --body Also blocks #5.") {
		t.Fatalf("second blocked issue must reuse the bug: %v\n%s", o, got)
	}
	if o, got = run("fix it\\n"+fixesBaseMarker, ""); o != Ok || strings.Contains(got, "issue create") {
		t.Fatalf("the bug's own run must skip the base check: %v\n%s", o, got)
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
	writeFile(t, filepath.Join(dir, "copilot"), `#!/bin/sh
prompt=""
usage=""
session=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    -p) shift; prompt="$1";;
    --usage-output-file) shift; usage="$1";;
    --session-id) shift; session="$1";;
  esac
  shift
done
printf '{"totalNanoAiu":100000000,"modelMetrics":{"fixture-model":{}}}' > "$usage"
if [ -n "$session" ]; then
  mkdir -p "$COPILOT_HOME/session-state/$session"
  printf '{"type":"assistant.message","data":{"content":"fixture session"}}\n' > "$COPILOT_HOME/session-state/$session/events.jsonl"
fi
case "$prompt" in
  *"You are the planner"*) echo '{"type":"assistant.message","data":{"content":"{\"status\":\"plan\",\"objective\":\"deliver file\",\"points\":[\"file exists\"],\"points_derived\":false,\"files\":[\"outcome.txt\"],\"verification\":\"default\",\"needs_investigation\":false,\"fits_one_pr\":true,\"units\":[]}"}}';;
  *"You are the implementation"*) echo delivered > outcome.txt; echo '{"type":"assistant.message","data":{"content":"{\"status\":\"done\",\"summary\":\"delivered file\",\"closes_issue\":true}"}}';;
  *"You independently review"*) echo '{"type":"assistant.message","data":{"content":"{\"verdict\":\"PASS\",\"reasons\":[\"file verified\"]}"}}';;
  *) exit 1;;
esac
echo '{"type":"result","exitCode":0}'
`, 0o755)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	t.Setenv("AGENT_SM_TEST_HELPER", "1")
	t.Setenv("SM_SCRATCH_ROOT", filepath.Join(dir, "scratch"))
	t.Setenv("COPILOT_HOME", filepath.Join(dir, "copilot-home"))
	t.Setenv("SM_TRANSCRIPTS", "false")
	p := defaultProfile("copilot")
	p.Base = "main"
	p.Checks = []string{"test -f outcome.txt || git diff --quiet HEAD"} // passes on the untouched base too
	p.Transcripts = true
	useProfile(t, p)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	r := &Runner{repo: repo, self: self, stats: filepath.Join(dir, "stats.jsonl")}
	j := Job{Issue: 1, Provider: "copilot", Profile: &p}
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
	if err != nil || !strings.Contains(string(b), `"provider":"copilot"`) || strings.Contains(string(b), `"usage_unknown":true`) {
		t.Fatalf("stats: %s %v", b, err)
	}
	firstEvents, err := os.ReadFile(filepath.Join(j.scratch(), "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	retry := Job{Issue: 1, Provider: "copilot", Profile: &p}
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
	direct := Job{Issue: 1, Repo: repo, Provider: "copilot", Profile: &p}
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
	blocked := Job{Issue: 1, Provider: "copilot", Profile: &p}
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
			p := defaultProfile("copilot")
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

func TestCopilotStop(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "copilot"), "#!/bin/sh\nsleep 300\n", 0o755)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	t.Setenv("SM_STATS", filepath.Join(dir, "stats.jsonl"))
	p := defaultProfile("copilot")
	p.TimeoutMinutes = .001
	useProfile(t, p)
	got := agent(&Ctx{Repo: dir, Scratch: dir}, Review, "test", new(any))
	if got != Timeout {
		t.Fatalf("got %v", got)
	}
}

func TestUsageReportSeparatesProviders(t *testing.T) {
	got := report([]byte(`{"state":"execute","provider":"copilot","credits":1.5}
{"state":"execute","provider":"copilot","usage_unknown":true}
{"state":"execute","provider":"claude","cost_usd":0.2}`))
	for _, want := range []string{"copilot/execute", "1.500 known credits", "1 unknown-usage", "claude/execute", "$0.200"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in %s", want, got)
		}
	}
}

func TestConcurrencyEnv(t *testing.T) {
	t.Setenv("SM_CONCURRENCY", "3")
	p := defaultProfile("copilot")
	if err := p.applyLimitEnv(); err != nil || p.Concurrency != 3 {
		t.Fatalf("concurrency = %d, %v", p.Concurrency, err)
	}
	p.Concurrency = 17
	if p.validate() == nil {
		t.Fatal("concurrency 17 accepted")
	}
}
