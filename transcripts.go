package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

type transcriptOutput struct {
	*os.File
	cancel context.CancelFunc
	err    error
}

func (w *transcriptOutput) Write(p []byte) (int, error) {
	n, err := w.File.Write(p)
	if err != nil {
		w.err = err
		w.cancel()
	}
	return n, err
}

type agentTranscript struct {
	dir, provider, sessionID, repo string
	stdout, stderr                 *transcriptOutput
}

func newTranscript(c *Ctx, state State) (*agentTranscript, error) {
	root := filepath.Join(c.Scratch, "transcripts")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(root, settings.Provider+"-"+state.String()+"-")
	if err != nil {
		return nil, err
	}
	stdout, err := os.OpenFile(filepath.Join(dir, "stdout.log"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	stderr, err := os.OpenFile(filepath.Join(dir, "stderr.log"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, errors.Join(err, stdout.Close())
	}
	t := &agentTranscript{dir: dir, provider: settings.Provider, repo: c.Repo,
		stdout: &transcriptOutput{File: stdout}, stderr: &transcriptOutput{File: stderr}}
	logEvent(map[string]any{"event": "transcript", "path": dir, "state": state.String(), "issue": c.Issue})
	return t, nil
}

func transcriptJSON(path string, value any) error {
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}

func (t *agentTranscript) request(args []string, state State, cancel context.CancelFunc) error {
	t.stdout.cancel, t.stderr.cancel = cancel, cancel
	return transcriptJSON(filepath.Join(t.dir, "request.json"), map[string]any{
		"provider": t.provider, "args": args, "cwd": t.repo,
		"state": state.String(), "schema": steps[state].Schema, "started": time.Now().UTC(),
	})
}

func (t *agentTranscript) finish(result Out) error {
	err := errors.Join(t.stdout.err, t.stderr.err, t.stdout.Close(), t.stderr.Close())
	var messagesErr error
	outcome := map[string]any{"outcome": result.String(), "finished": time.Now().UTC()}
	if messagesErr != nil {
		outcome["messages_error"] = messagesErr.Error()
	}
	if err != nil {
		outcome["capture_error"] = err.Error()
	}
	return errors.Join(err, transcriptJSON(filepath.Join(t.dir, "result.json"), outcome))
}
