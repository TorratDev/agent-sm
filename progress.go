package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// progress is the live state of one agent session, written to <scratch>/status.json (at most
// once a second) so a long step can be watched without reading logs.
type progress struct {
	c        *Ctx
	state    State
	unit     string
	start    time.Time
	last     time.Time
	calls    int
	lastTool string
	lastCmd  string
	feed     []feedItem
}

// toolInput is the part of a tool call's input worth showing.
type toolInput struct {
	Command  string `json:"command"`
	FilePath string `json:"file_path"`
	Path     string `json:"path"`
	Pattern  string `json:"pattern"`
}

type feedItem struct {
	T    string `json:"t"`
	Kind string `json:"kind"` // tool, say, error
	Text string `json:"text"`
}

const feedKeep = 40

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

func (p *progress) add(kind, text string) {
	if text = oneLine(text, 160); text == "" {
		return
	}
	p.feed = append(p.feed, feedItem{T: time.Now().Format("15:04:05"), Kind: kind, Text: text})
	if len(p.feed) > feedKeep {
		p.feed = p.feed[1:]
	}
	p.flush(false)
}

func (p *progress) tool(name string, in toolInput) {
	p.calls++
	p.lastTool, p.lastCmd = name, ""
	if w := cmdWords(in.Command); len(w) > 0 {
		p.lastCmd = strings.Join(w, " | ")
	}
	what := in.FilePath
	switch {
	case name == "Bash" || name == "bash" || name == "shell":
		what = in.Command
	case in.Pattern != "":
		what = in.Pattern + " " + in.Path
	case what == "":
		what = in.Path
	}
	p.add("tool", name+" "+what)
}

func (p *progress) flush(force bool) {
	if !force && time.Since(p.last) < time.Second {
		return
	}
	p.last = time.Now()
	b, _ := json.MarshalIndent(map[string]any{
		"issue": p.c.Issue, "state": p.state.String(), "unit": p.unit,
		"unit_index": p.c.UnitIdx + 1, "units": len(p.c.Units),
		"elapsed_s": int(time.Since(p.start).Seconds()), "tool_calls": p.calls,
		"last_tool": p.lastTool, "last_command": p.lastCmd, "feed": p.feed,
		"provider": settings.Provider, "usage_unit": settings.unit(), "usage_before_this_step": p.c.Cost,
		"tool_telemetry_available": settings.Provider != "copilot", "updated": p.last.Format(time.RFC3339),
	}, "", "  ")
	os.WriteFile(filepath.Join(p.c.Scratch, "status.json"), b, 0o644)
}
