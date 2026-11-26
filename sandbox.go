package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// sandboxMode isolates every agent session: "bwrap" (bubblewrap, the Linux default) or
// "podman" (the Windows default). SM_SANDBOX picks one explicitly. Inside, the agent has a full
// shell; it sees the worktree, the toolchain and pi, but no home directory, no SSH agent and no
// GitHub credentials, so only agent-sm itself can push, open PRs or edit issues.
var sandboxMode = func() string {
	if v := os.Getenv("SM_SANDBOX"); v != "" {
		return v
	}
	if runtime.GOOS == "windows" {
		return "podman"
	}
	return "bwrap"
}()

// sandboxOff runs pi directly on the host. Only tests set it, to drive fake agents.
var sandboxOff bool

const defaultSandboxImage = "agent-sm-pi"

// sandboxed returns the command that runs `pi args` for one step. repo is the worktree, writable
// only when the step edits; files is the directory holding the prompt files that args name.
func sandboxed(ctx context.Context, repo, files string, writable bool, args []string) (*exec.Cmd, error) {
	if sandboxOff {
		cmd := process(ctx, repo, "pi", args...)
		cmd.Env = cleanAgentEnv()
		return cmd, nil
	}
	switch sandboxMode {
	case "bwrap":
		// The agent's /tmp and home live on disk: on tmpfs a downloaded JDK or a venv used RAM until the host ran out.
		root := filepath.Join(scratchRoot(), "sandbox-tmp")
		if err := os.MkdirAll(root, 0o700); err != nil {
			return nil, err
		}
		// shortcut: a crashed run leaves its directory here; sweep root at serve start if leftovers pile up.
		tmp, err := os.MkdirTemp(root, "")
		if err != nil {
			return nil, err
		}
		context.AfterFunc(ctx, func() { os.RemoveAll(tmp) }) // the caller cancels ctx once pi has exited
		b, err := bwrapArgs(repo, files, tmp, writable)
		if err != nil {
			os.RemoveAll(tmp)
			return nil, err
		}
		cmd := process(ctx, repo, "bwrap", append(append(b, "--", "pi"), args...)...)
		cmd.Env = sandboxEnv()
		return cmd, nil
	case "podman":
		if err := sandboxPreflight(); err != nil {
			return nil, err
		}
		image := sandboxImage()
		name := "agent-sm-" + rand.Text()
		p, err := podmanArgs(repo, files, writable, image, name, args)
		if err != nil {
			return nil, err
		}
		cmd := process(ctx, repo, "podman", p...)
		cmd.Env = sandboxEnv()
		kill := cmd.Cancel
		// Killing the podman client leaves the container running; remove it as well.
		cmd.Cancel = func() error {
			exec.Command("podman", "rm", "-f", name).Run()
			return kill()
		}
		return cmd, nil
	}
	return nil, fmt.Errorf("SM_SANDBOX must be bwrap or podman, not %q", sandboxMode)
}

func sandboxImage() string {
	if v := os.Getenv("SM_SANDBOX_IMAGE"); v != "" {
		return v
	}
	return defaultSandboxImage
}

// sandboxPreflight fails before any step runs when the podman sandbox cannot start: the podman
// machine is down, or the image is not built. Runs without podman pass.
func sandboxPreflight() error {
	if sandboxOff || sandboxMode != "podman" {
		return nil
	}
	image := sandboxImage()
	// `podman image exists` exits 1 only when the image is absent; any other failure
	// (for example the podman machine is stopped) must not be reported as a missing image.
	if out, err := exec.Command("podman", "image", "exists", image).CombinedOutput(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			return fmt.Errorf("podman image %s not found; build it with: podman build -t %s sandbox", image, image)
		}
		return fmt.Errorf("podman cannot check image %s (is the podman machine started?): %s", image, strings.TrimSpace(string(out)))
	}
	return nil
}

// bwrapOverlay reports whether the installed bwrap can mount overlays (0.11+).
var bwrapOverlay = sync.OnceValue(func() bool {
	out, _ := exec.Command("bwrap", "--help").CombinedOutput()
	return strings.Contains(string(out), "--overlay-src")
})

// sandboxEnv is the agent environment without anything that reaches GitHub or SSH.
func sandboxEnv() []string {
	var env []string
	for _, e := range cleanAgentEnv() {
		switch k, _, _ := strings.Cut(e, "="); k {
		case "GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN",
			"SSH_AUTH_SOCK", "GIT_ASKPASS", "SSH_ASKPASS":
			continue
		}
		env = append(env, e)
	}
	return env
}

// bwrapArgs mounts the host read-only with an empty home and /tmp (directories under tmp, on
// disk) and an empty /run, then
// brings back read-only only what an agent needs: the PATH directories and toolchains under
// home, pi's code and login, add_dirs and the prompt files. The worktree is the only host
// directory it can write. Files written anywhere else vanish with the sandbox.
func bwrapArgs(repo, files, tmp string, writable bool) ([]string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	tmpDir, homeDir := filepath.Join(tmp, "tmp"), filepath.Join(tmp, "home")
	if err := errors.Join(os.MkdirAll(tmpDir, 0o700), os.MkdirAll(homeDir, 0o700)); err != nil {
		return nil, err
	}
	args := []string{"--die-with-parent", "--new-session", "--unshare-all", "--share-net",
		"--ro-bind", "/", "/", "--dev", "/dev", "--proc", "/proc", "--bind", tmpDir, "/tmp", "--tmpfs", "/run",
		// systemd hosts point /etc/resolv.conf here; without it there is no DNS.
		"--ro-bind-try", "/run/systemd/resolve", "/run/systemd/resolve",
		"--bind", homeDir, home}
	ro := func(p string) { args = append(args, "--ro-bind-try", p, p) }
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir != "" && within(home, dir) {
			ro(dir)
		}
	}
	var goroot, gomodcache, gocache string
	if out, err := exec.Command("go", "env", "GOROOT", "GOMODCACHE", "GOCACHE").Output(); err == nil {
		if v := strings.Split(strings.TrimSpace(string(out)), "\n"); len(v) == 3 {
			goroot, gomodcache, gocache = v[0], v[1], v[2]
		}
	}
	if goroot != "" && within(home, goroot) {
		ro(goroot)
	}
	// Toolchain installs without credentials in them.
	ro(filepath.Join(home, ".rustup"))
	ro(filepath.Join(home, ".dotnet"))
	// Module, build and crate caches: the agent writes only agent-sm's own cache, never the host's,
	// so it cannot poison what the host's checks build with, and its downloads persist between
	// steps. With bwrap 0.11+ the host's cache is the read-only layer underneath, so what the host
	// already downloaded is not fetched again.
	cache := filepath.Join(scratchRoot(), "sandbox-cache")
	for _, c := range []struct{ name, dst string }{
		{"go-mod", gomodcache}, {"go-build", gocache},
		{"cargo-registry", filepath.Join(home, ".cargo", "registry")},
		{"cargo-git", filepath.Join(home, ".cargo", "git")},
		{"nuget", filepath.Join(home, ".nuget", "packages")},
	} {
		if c.dst == "" {
			continue // no Go toolchain
		}
		upper, work := filepath.Join(cache, c.name, "upper"), filepath.Join(cache, c.name, "work")
		if err := errors.Join(os.MkdirAll(upper, 0o700), os.MkdirAll(work, 0o700)); err != nil {
			return nil, err
		}
		if st, err := os.Stat(c.dst); err == nil && st.IsDir() && bwrapOverlay() {
			// shortcut: concurrent sessions share one upper directory, which overlayfs leaves undefined
			// (no crash; a session may miss another's fresh download); give each job its own upper if that bites.
			args = append(args, "--overlay-src", c.dst, "--overlay", upper, work, c.dst)
		} else {
			args = append(args, "--bind", upper, c.dst)
		}
	}
	// pi's managed install and login. Its own state directory stays writable (it takes locks there).
	// shortcut: assumes pi's installer layout (~/.pi/agent); an npm-global pi outside PATH dirs is not mounted.
	agentDir := filepath.Join(home, ".pi", "agent")
	for _, f := range []string{"bin", "install", "auth.json", "settings.json", "models-store.json"} {
		ro(filepath.Join(agentDir, f))
	}
	for _, d := range settings.AddDirs {
		ro(d)
	}
	// Linked files are symlinks into the main checkout, which the empty home would otherwise hide.
	for _, f := range linkNames(settings.Links) {
		if t, err := filepath.EvalSymlinks(filepath.Join(repo, f)); err == nil && !within(repo, t) {
			ro(t)
		}
	}
	ro(files)
	if common, err := gitDir(repo, "--git-common-dir"); err == nil && !within(repo, common) {
		ro(common) // a worktree's .git file points into the main checkout's .git
	}
	if writable {
		args = append(args, "--bind", repo, repo)
	} else {
		ro(repo)
	}
	return append(args, "--chdir", repo), nil
}

// podmanArgs runs pi in the agent-sm-pi image with the worktree at /work and the prompt files at
// /sm. Git there cannot follow the worktree's host path, so GIT_DIR points at the mounted .git.
func podmanArgs(repo, files string, writable bool, image, name string, args []string) ([]string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	mode := ":ro"
	if writable {
		mode = ""
	}
	p := []string{"run", "--rm", "-i", "--name", name,
		"-v", repo + ":/work" + mode, "-w", "/work", "-v", files + ":/sm:ro",
		"-v", filepath.Join(home, ".pi", "agent", "auth.json") + ":/root/.pi/agent/auth.json:ro",
		// The mounts belong to the host user, not the container's root.
		"-e", "GIT_CONFIG_COUNT=1", "-e", "GIT_CONFIG_KEY_0=safe.directory", "-e", "GIT_CONFIG_VALUE_0=*"}
	gd, gerr := gitDir(repo, "--git-dir")
	common, cerr := gitDir(repo, "--git-common-dir")
	if gerr == nil && cerr == nil {
		if rel, err := filepath.Rel(repo, gd); err == nil && filepath.IsLocal(rel) {
			p = append(p, "-e", "GIT_DIR=/work/"+filepath.ToSlash(rel))
		} else if rel, err := filepath.Rel(common, gd); err == nil && (rel == "." || filepath.IsLocal(rel)) {
			p = append(p, "-v", common+":/git:ro", "-e", "GIT_DIR=/git/"+filepath.ToSlash(rel))
		}
		p = append(p, "-e", "GIT_WORK_TREE=/work")
	}
	// add_dirs read-only: under home at the same place in the container's home, so ~/x stays ~/x;
	// anything else under /host (C:\x is /host/C/x).
	for _, d := range settings.AddDirs {
		v := filepath.VolumeName(d)
		dst := path.Join("/host", filepath.ToSlash(strings.TrimSuffix(v, ":")), filepath.ToSlash(d[len(v):]))
		if rel, err := filepath.Rel(home, d); err == nil && filepath.IsLocal(rel) {
			dst = "/root/" + filepath.ToSlash(rel)
		}
		p = append(p, "-v", d+":"+dst+":ro")
	}
	// Linked files: the worktree's symlinks point at host paths the container cannot follow, and a
	// hard link would let a writable step change the main checkout's copy, so mount each read-only.
	for _, f := range linkNames(settings.Links) {
		if src := filepath.Join(repo, f); filepath.IsLocal(f) {
			if st, err := os.Stat(src); err == nil && !st.IsDir() {
				p = append(p, "-v", src+":/work/"+filepath.ToSlash(filepath.Clean(f))+":ro")
			}
		}
	}
	p = append(p, image, "pi")
	for _, a := range args {
		path, at := strings.CutPrefix(a, "@")
		if rel, err := filepath.Rel(files, path); err == nil && filepath.IsLocal(rel) {
			a = "/sm/" + filepath.ToSlash(rel)
			if at {
				a = "@" + a
			}
		}
		p = append(p, a)
	}
	return p, nil
}

func gitDir(repo, flag string) (string, error) {
	out, err := sh(repo, "git", "rev-parse", "--path-format=absolute", flag)
	if err != nil {
		return "", fmt.Errorf("git rev-parse %s: %s", flag, strings.TrimSpace(out))
	}
	return filepath.Clean(strings.TrimSpace(out)), nil
}

// within reports whether path is base or inside it.
func within(base, path string) bool {
	rel, err := filepath.Rel(base, path)
	return err == nil && (rel == "." || filepath.IsLocal(rel))
}
