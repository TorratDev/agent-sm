package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// worktreeFixture is a main checkout and a linked worktree of it, as runs create them.
func worktreeFixture(t *testing.T) (main, wt string) {
	t.Helper()
	dir := realTempDir(t)
	main, wt = filepath.Join(dir, "main"), filepath.Join(dir, "wt")
	for _, args := range [][]string{
		{"init", "-q", main},
		{"-C", main, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "init"},
		{"-C", main, "worktree", "add", "-q", wt, "-b", "work"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
	}
	return main, wt
}

func TestBwrapSandboxIsolates(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("bwrap is Linux only")
	}
	if err := exec.Command("bwrap", "--ro-bind", "/", "/", "--unshare-all", "true").Run(); err != nil {
		t.Skipf("bwrap cannot create a sandbox here: %v", err)
	}
	_, wt := worktreeFixture(t)
	home, files := realTempDir(t), realTempDir(t)
	t.Setenv("HOME", home)
	t.Setenv("SM_SCRATCH_ROOT", realTempDir(t))
	for _, k := range []string{"GOPATH", "GOMODCACHE", "GOCACHE", "XDG_CACHE_HOME"} {
		t.Setenv(k, "") // derive the caches from the fake home
	}
	writeFile(t, filepath.Join(home, "secret"), "token", 0o600)
	writeFile(t, filepath.Join(files, "prompt.md"), "prompt", 0o600)
	run := func(writable bool, script string) (string, error) {
		args, err := bwrapArgs(wt, files, writable)
		if err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command("bwrap", append(append(args, "--", "sh", "-c"), script)...).CombinedOutput()
		return string(out), err
	}
	out, err := run(true, `cat "$HOME/secret" 2>/dev/null && echo LEAK
echo escape > "$HOME/escape"
cat `+files+`/prompt.md && echo
echo edit > edited.txt && git status --short`)
	if err != nil || strings.Contains(out, "LEAK") || !strings.Contains(out, "prompt") || !strings.Contains(out, "edited.txt") {
		t.Fatalf("sandbox run: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(home, "escape")); err == nil {
		t.Fatal("a write outside the worktree reached the host")
	}
	if b, _ := os.ReadFile(filepath.Join(wt, "edited.txt")); string(b) != "edit\n" {
		t.Fatal("the edit step's write did not reach the worktree")
	}
	if out, err := run(false, "echo x > review.txt"); err == nil {
		t.Fatalf("a read-only step wrote the worktree: %s", out)
	}
	// The module cache is agent-sm's own and outlives the step; the host's stays untouched.
	modcache, err := exec.Command("go", "env", "GOMODCACHE").Output()
	if err != nil {
		t.Skip("no Go toolchain for the cache check")
	}
	mc := strings.TrimSpace(string(modcache))
	if out, err := run(false, "echo m > "+mc+"/marker"); err != nil {
		t.Fatalf("module cache not writable: %v %s", err, out)
	}
	if out, err := run(false, "cat "+mc+"/marker"); err != nil || out != "m\n" {
		t.Fatalf("module cache did not persist between steps: %v %q", err, out)
	}
	if _, err := os.Stat(filepath.Join(mc, "marker")); err == nil {
		t.Fatal("the agent wrote the host's module cache")
	}
	if b, _ := os.ReadFile(filepath.Join(scratchRoot(), "sandbox-cache", "go-mod", "upper", "marker")); string(b) != "m\n" {
		t.Fatal("the module cache is not agent-sm's own")
	}
	if !bwrapOverlay() {
		t.Log("bwrap has no overlays (needs 0.11+): the host's caches are not shared")
		return
	}
	// With overlays the host's cache shows through read-only underneath agent-sm's.
	if err := os.MkdirAll(mc, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(mc, "hostmod"), "host", 0o644)
	if out, err := run(false, "cat "+mc+"/hostmod && echo agent > "+mc+"/hostmod && cat "+mc+"/marker"); err != nil || out != "hostm\n" {
		t.Fatalf("overlay cache: %v %q", err, out)
	}
	if b, _ := os.ReadFile(filepath.Join(mc, "hostmod")); string(b) != "host" {
		t.Fatal("the agent changed the host's module cache through the overlay")
	}
}

// A linked ROM dump in a main checkout under home must be readable, not a dangling symlink.
func TestBwrapSandboxReadsLinkedFiles(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("bwrap is Linux only")
	}
	if err := exec.Command("bwrap", "--ro-bind", "/", "/", "--unshare-all", "true").Run(); err != nil {
		t.Skipf("bwrap cannot create a sandbox here: %v", err)
	}
	main, wt := worktreeFixture(t)
	t.Setenv("HOME", filepath.Dir(main))
	t.Setenv("SM_SCRATCH_ROOT", realTempDir(t))
	for _, k := range []string{"GOPATH", "GOMODCACHE", "GOCACHE", "XDG_CACHE_HOME"} {
		t.Setenv(k, "")
	}
	writeFile(t, filepath.Join(main, "pmdb.nds"), "rom", 0o644)
	old := settings.Links
	settings.Links = []string{"pmdb.nds"}
	t.Cleanup(func() { settings.Links = old })
	if err := linkFiles(main, wt, settings.Links); err != nil {
		t.Fatal(err)
	}
	args, err := bwrapArgs(wt, realTempDir(t), true)
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("bwrap", append(args, "--", "sh", "-c", "cat pmdb.nds; echo x > pmdb.nds && echo WROTE")...).CombinedOutput()
	if !strings.HasPrefix(string(out), "rom") || strings.Contains(string(out), "WROTE") {
		t.Fatalf("linked file in sandbox: %v\n%s", err, out)
	}
}

func TestSandboxEnvDropsCredentials(t *testing.T) {
	t.Setenv("GH_TOKEN", "gh-secret")
	t.Setenv("SSH_AUTH_SOCK", "/run/ssh-secret")
	t.Setenv("SM_SANDBOX_KEEP", "kept")
	env := strings.Join(sandboxEnv(), "\n")
	if strings.Contains(env, "secret") || !strings.Contains(env, "SM_SANDBOX_KEEP=kept") {
		t.Fatalf("sandbox env: %s", env)
	}
}

func TestPodmanArgsMapPathsIntoContainer(t *testing.T) {
	main, wt := worktreeFixture(t)
	files := realTempDir(t)
	common := filepath.Join(main, ".git")
	got := strings.Join(mustPodman(t, wt, files, false, []string{"-p", "--append-system-prompt", filepath.Join(files, "system.md"), "@" + filepath.Join(files, "prompt.md")}), " ")
	for _, want := range []string{
		wt + ":/work:ro", files + ":/sm:ro", common + ":/git:ro",
		"GIT_DIR=/git/worktrees/wt", "GIT_WORK_TREE=/work",
		"img pi -p --append-system-prompt /sm/system.md @/sm/prompt.md",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in: %s", want, got)
		}
	}
	if got := strings.Join(mustPodman(t, main, files, true, nil), " "); !strings.Contains(got, main+":/work ") || !strings.Contains(got, "GIT_DIR=/work/.git") {
		t.Errorf("main checkout args: %s", got)
	}
}

// add_dirs and linked files reach the podman sandbox read-only; ~/x keeps its place under home.
func TestPodmanArgsMountAddDirsAndLinks(t *testing.T) {
	_, wt := worktreeFixture(t)
	if err := os.WriteFile(filepath.Join(wt, "p569.nds"), []byte("rom"), 0o644); err != nil {
		t.Fatal(err)
	}
	home, _ := os.UserHomeDir()
	outside, want := "/data/saves", "/host/data/saves"
	if runtime.GOOS == "windows" {
		outside, want = `D:\data\saves`, "/host/D/data/saves"
	}
	old := settings
	t.Cleanup(func() { settings = old })
	settings.AddDirs = []string{filepath.Join(home, "venvs", "desmume"), outside}
	settings.Links = []string{"p569.nds", "missing.bin"}
	got := strings.Join(mustPodman(t, wt, realTempDir(t), true, nil), "\n")
	for _, w := range []string{
		filepath.Join(home, "venvs", "desmume") + ":/root/venvs/desmume:ro",
		outside + ":" + want + ":ro",
		filepath.Join(wt, "p569.nds") + ":/work/p569.nds:ro",
	} {
		if !strings.Contains(got, w) {
			t.Errorf("missing %q in:\n%s", w, got)
		}
	}
	if strings.Contains(got, "missing.bin") {
		t.Errorf("mounted a link the worktree does not have:\n%s", got)
	}
}

func mustPodman(t *testing.T, repo, files string, writable bool, args []string) []string {
	t.Helper()
	p, err := podmanArgs(repo, files, writable, "img", "agent-sm-test", args)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// A podman that cannot run (no binary, stopped machine) must not be reported as a missing image.
func TestSandboxedPodmanFailureIsNotMissingImage(t *testing.T) {
	oldMode, oldOff := sandboxMode, sandboxOff
	sandboxMode, sandboxOff = "podman", false
	t.Cleanup(func() { sandboxMode, sandboxOff = oldMode, oldOff })
	t.Setenv("PATH", t.TempDir())
	t.Setenv("SM_SANDBOX_IMAGE", "")
	_, err := sandboxed(context.Background(), t.TempDir(), t.TempDir(), false, []string{"-p"})
	if err == nil || strings.Contains(err.Error(), "not found") || !strings.Contains(err.Error(), "podman cannot check image") {
		t.Fatalf("want a podman failure, got: %v", err)
	}
}

// The preflight only gates podman: runs with the sandbox off or in bwrap never ask podman.
func TestSandboxPreflightSkipsWithoutPodman(t *testing.T) {
	oldMode, oldOff := sandboxMode, sandboxOff
	t.Cleanup(func() { sandboxMode, sandboxOff = oldMode, oldOff })
	t.Setenv("PATH", t.TempDir())
	sandboxMode, sandboxOff = "podman", true
	if err := sandboxPreflight(); err != nil {
		t.Errorf("sandbox off: %v", err)
	}
	sandboxMode, sandboxOff = "bwrap", false
	if err := sandboxPreflight(); err != nil {
		t.Errorf("bwrap: %v", err)
	}
}

// The diagnose step passes the whole scratch directory as its prompt files: podman must mount it
// at /sm, where the auditor's prompt says it is, and find the prompt file in its transcripts.
func TestPodmanMountsScratchForAuditor(t *testing.T) {
	scratch := t.TempDir()
	prompt := filepath.Join(scratch, "transcripts", "pi-diagnose-1", "prompt.md")
	got := strings.Join(mustPodman(t, t.TempDir(), scratch, false, []string{"@" + prompt}), " ")
	for _, w := range []string{scratch + ":/sm:ro", "@/sm/transcripts/pi-diagnose-1/prompt.md"} {
		if !strings.Contains(got, w) {
			t.Errorf("missing %q in:\n%s", w, got)
		}
	}
}
