package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"
)

// Jev is TypeSafe's decision model: it answers a choice question with a label and its
// probability, served by OpenCode's System One endpoint. agent-sm asks it one thing:
// does an open issue already ask for the work it is about to file?
var jevURL = "https://opencode.ai/inference/systemone/v1/systemone" // a var so tests can use a fake server

// shortcut: a limited-time free model; once OpenCode retires it every check fails open, so switch to jev-1.14.
const jevModel = "jev-1.13-free"

// jevMinConfidence: below this, file the issue; a missed duplicate costs less than lost work.
const jevMinConfidence = 0.9

// duplicateIssue returns the URL of an open issue labelled label that already asks for the
// new issue's work, or "" to file it. It fails open: without OPENCODE_API_KEY, open issues
// or a confident answer from Jev, the issue is filed as before.
func duplicateIssue(repo, label, title, body string) string {
	key := os.Getenv("OPENCODE_API_KEY")
	if key == "" {
		return ""
	}
	out, err := sh(repo, "gh", "issue", "list", "--state", "open", "--label", label, "--limit", "254", "--json", "number,title,body,url")
	var open []openIssue
	if err != nil || json.Unmarshal([]byte(out), &open) != nil {
		return ""
	}
	return pickDuplicate(key, label, title, body, open)
}

type openIssue struct {
	Number           int
	Title, Body, URL string
}

// pickDuplicate asks Jev which of open (at most 254, so 255 options with "new") the new issue repeats.
func pickDuplicate(key, label, title, body string, open []openIssue) string {
	if len(open) == 0 {
		return ""
	}
	criteria := map[string]string{"new": "None of these: the new issue asks for different work."}
	urls := map[string]string{}
	for _, o := range open { // follow-ups share their parent's title, so the body start tells them apart
		k := strconv.Itoa(o.Number)
		criteria[k], urls[k] = o.Title+"\n"+ends(o.Body, 400, 0), o.URL
	}
	req := map[string]any{
		"model": jevModel,
		"state": "New issue: " + title + "\n\n" + ends(body, 6000, 2000),
		"questions": map[string]any{"duplicate": map[string]any{
			"type":         "choice",
			"instructions": "Which open issue already asks for the same work as the new issue? Answer new if none does.",
			"criteria":     criteria,
		}},
	}
	var res struct {
		Answers map[string]struct {
			Choice     string
			Confidence float64
		}
	}
	if err := jevAsk(key, req, &res); err != nil {
		logEvent(map[string]any{"event": "jev-error", "err": err.Error()})
		return ""
	}
	a := res.Answers["duplicate"]
	logEvent(map[string]any{"event": "jev-duplicate", "label": label, "choice": a.Choice, "confidence": a.Confidence})
	if a.Confidence < jevMinConfidence {
		return ""
	}
	return urls[a.Choice] // "" for "new" or an option Jev was not given
}

func jevAsk(key string, req, out any) error {
	b, err := json.Marshal(req)
	if err != nil {
		return err
	}
	r, err := http.NewRequestWithContext(runCtx, http.MethodPost, jevURL, bytes.NewReader(b))
	if err != nil {
		return err
	}
	r.Header.Set("Authorization", "Bearer "+key)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("User-Agent", "agent-sm")
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(r)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("jev: HTTP %d: %s", resp.StatusCode, tail(string(body), 300))
	}
	return json.Unmarshal(body, out)
}
