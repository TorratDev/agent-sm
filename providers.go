package main

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

var commandPrefix = regexp.MustCompile(`^[a-zA-Z0-9_./~-]+(?: [a-zA-Z0-9_./~-]+)*$`)

func agent(c *Ctx, state State, prompt string, out any) (result Out) {
	var transcript *agentTranscript
	if settings.Transcripts {
		var err error
		// Transcripts are diagnostics, on by default: a capture failure is logged and never
		// changes the step's outcome, so a paid-for result is not thrown away.
		transcript, err = newTranscript(c, state)
		if err != nil {
			logEvent(map[string]any{"event": "transcript-error", "err": err.Error()})
			transcript = nil
		} else {
			defer func() {
				if err := transcript.finish(result); err != nil {
					logEvent(map[string]any{"event": "transcript-error", "path": transcript.dir, "err": err.Error()})
				}
			}()
		}
	}
	return piAgent(c, state, prompt, out, transcript)
}

func process(ctx context.Context, dir, name string, args ...string) *exec.Cmd {
	name, args = agentCommand(name, args)
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	setProcessGroup(cmd)
	cmd.Cancel = func() error { return killProcessGroup(cmd.Process.Pid, syscall.SIGKILL) }
	// A descendant holding a pipe must not keep an otherwise exited session alive.
	cmd.WaitDelay = time.Second
	return cmd
}

type boundedOutput struct {
	bytes.Buffer
	limit int
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	n := len(p)
	if b.Len()+n > b.limit {
		remaining := max(0, b.limit-b.Len())
		b.Buffer.Write(p[:remaining])
		return n, fmt.Errorf("output exceeds %d bytes", b.limit)
	}
	return b.Buffer.Write(p)
}

func cleanAgentEnv() []string {
	var env []string
	for _, entry := range os.Environ() {
		k, _, _ := strings.Cut(entry, "=")
		switch k {
		case "SM_RESOLVED_CONFIG", "JIRA_API_TOKEN", "JIRA_USER_EMAIL", "BASH_ENV", "ENV":
			continue
		}
		if settings.Management.Mode == "service" {
			switch k {
			case "GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN":
				continue
			}
		}
		env = append(env, entry)
	}
	return env
}

func checkCommand(dir, command string, env ...string) (string, error) {
	ctx, cancel := context.WithTimeout(runCtx, time.Duration(settings.TimeoutMinutes*float64(time.Minute)))
	defer cancel()
	cmd := process(ctx, dir, shellPath(), "-c", command)
	cmd.Env = append(cleanAgentEnv(), env...)
	output := &boundedOutput{limit: 4 << 20}
	cmd.Stdout, cmd.Stderr = output, output
	err := cmd.Run()
	ev := map[string]any{"event": "check-command", "command": command, "ok": err == nil}
	if err != nil {
		ev["err"] = err.Error()
	}
	logEvent(ev)
	return output.String(), err
}

func verifyTestReports(dir string, patterns []string) error {
	for _, pattern := range patterns {
		files, err := filepath.Glob(filepath.Join(dir, pattern))
		if err != nil || len(files) == 0 {
			return fmt.Errorf("required test report %q missing", pattern)
		}
		for _, path := range files {
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			var report struct {
				XMLName  xml.Name `xml:"TestRun"`
				Counters struct {
					Total    int `xml:"total,attr"`
					Executed int `xml:"executed,attr"`
					Passed   int `xml:"passed,attr"`
				} `xml:"ResultSummary>Counters"`
			}
			if err := xml.Unmarshal(b, &report); err != nil {
				return fmt.Errorf("invalid TRX %s: %w", path, err)
			}
			n := report.Counters
			if n.Executed == 0 || n.Executed != n.Total || n.Passed != n.Executed {
				return fmt.Errorf("required test report %s: total=%d executed=%d passed=%d", path, n.Total, n.Executed, n.Passed)
			}
		}
	}
	return nil
}
