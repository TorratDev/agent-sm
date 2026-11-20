package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

type githubComment struct {
	ID   int64                  `json:"id"`
	Body string                 `json:"body"`
	User struct{ Login string } `json:"user"`
}

type jiraComment struct {
	ID     string          `json:"id"`
	Body   json.RawMessage `json:"body"`
	Author struct {
		AccountID string `json:"accountId"`
	} `json:"author"`
}

func (m *manager) progressComments(ctx context.Context, item *ManagedItem, text string) error {
	runes := []rune(text)
	if len(runes) > 12000 {
		text = string(runes[:12000]) + "\n[Truncated; full evidence remains in run artifacts.]"
	}
	hash := contentDigest(text)
	if item.ProgressHash == hash {
		return nil
	}
	var actor struct{ Login string }
	if err := m.api.call(ctx, "github", "GET", "/user", nil, &actor); err != nil {
		return err
	}
	if actor.Login == "" {
		return fmt.Errorf("GitHub service comment identity unavailable")
	}
	marker := "<!-- agent-sm:progress:" + strconv.Itoa(item.Number) + " -->"
	wanted := text + "\n\n" + marker
	opID := "progress/github/" + strconv.Itoa(item.Number)
	if err := m.op(ctx, opID, true, func(uncertain bool) (any, error) {
		comments, err := ghPages[githubComment](ctx, m, m.endpoint(item.Number, "/comments"))
		if err != nil {
			return nil, err
		}
		var found *githubComment
		for _, comment := range comments {
			if strings.Contains(comment.Body, marker) && comment.User.Login == actor.Login {
				if found != nil {
					return nil, fmt.Errorf("duplicate service progress comments")
				}
				copy := comment
				found = &copy
			}
		}
		if found != nil {
			return found, nil
		}
		if uncertain {
			return nil, fmt.Errorf("GitHub progress comment outcome unknown; inspect before recovery")
		}
		var created githubComment
		if err := m.api.call(ctx, "github", "POST", m.endpoint(item.Number, "/comments"), map[string]string{"body": wanted}, &created); err != nil {
			return nil, err
		}
		if created.ID <= 0 || created.User.Login != actor.Login {
			return nil, fmt.Errorf("invalid GitHub comment identity")
		}
		return created, nil
	}); err != nil {
		return err
	}
	var gh githubComment
	if json.Unmarshal(m.state.Ops[opID].Result, &gh) != nil || gh.ID <= 0 {
		return fmt.Errorf("invalid GitHub comment journal")
	}
	path := "/repos/" + m.profile.Management.Repo + "/issues/comments/" + strconv.FormatInt(gh.ID, 10)
	var actual githubComment
	if err := m.api.call(ctx, "github", "GET", path, nil, &actual); err != nil {
		return err
	}
	if actual.User.Login != actor.Login || !strings.Contains(actual.Body, marker) {
		return fmt.Errorf("service progress comment ownership conflict")
	}
	if actual.Body != wanted {
		if err := m.api.call(ctx, "github", "PATCH", path, map[string]string{"body": wanted}, nil); err != nil {
			return err
		}
		if err := m.api.call(ctx, "github", "GET", path, nil, &actual); err != nil {
			return err
		}
		if actual.Body != wanted {
			return fmt.Errorf("GitHub comment readback mismatch")
		}
	}
	var jiraActor struct {
		AccountID string `json:"accountId"`
	}
	if err := m.api.call(ctx, "jira", "GET", "/myself", nil, &jiraActor); err != nil {
		return err
	}
	if jiraActor.AccountID == "" {
		return fmt.Errorf("Jira service identity unavailable")
	}
	marker = "[agent-sm:progress:" + strconv.Itoa(item.Number) + "]"
	desc := adf(text + "\n" + marker)
	opID = "progress/jira/" + strconv.Itoa(item.Number)
	if err := m.op(ctx, opID, true, func(uncertain bool) (any, error) {
		comments, err := jiraPages[jiraComment](ctx, m, "/issue/"+item.JiraKey+"/comment", "comments")
		if err != nil {
			return nil, err
		}
		var found *jiraComment
		for _, c := range comments {
			if c.Author.AccountID == jiraActor.AccountID && strings.Contains(string(c.Body), marker) {
				if found != nil {
					return nil, fmt.Errorf("duplicate Jira service progress comments")
				}
				copy := c
				found = &copy
			}
		}
		if found != nil {
			return found, nil
		}
		if uncertain {
			return nil, fmt.Errorf("Jira comment outcome unknown; inspect before recovery")
		}
		var created jiraComment
		if err := m.api.call(ctx, "jira", "POST", "/issue/"+item.JiraKey+"/comment", map[string]any{"body": desc}, &created); err != nil {
			return nil, err
		}
		if created.ID == "" || created.Author.AccountID != jiraActor.AccountID {
			return nil, fmt.Errorf("invalid Jira comment identity")
		}
		return created, nil
	}); err != nil {
		return err
	}
	var jc jiraComment
	if json.Unmarshal(m.state.Ops[opID].Result, &jc) != nil || jc.ID == "" {
		return fmt.Errorf("invalid Jira comment journal")
	}
	path = "/issue/" + item.JiraKey + "/comment/" + urlSegment(jc.ID)
	var actualJira jiraComment
	if err := m.api.call(ctx, "jira", "GET", path, nil, &actualJira); err != nil {
		return err
	}
	if actualJira.Author.AccountID != jiraActor.AccountID || !strings.Contains(string(actualJira.Body), marker) {
		return fmt.Errorf("Jira progress comment ownership conflict")
	}
	var observed any
	if json.Unmarshal(actualJira.Body, &observed) != nil {
		return fmt.Errorf("invalid Jira comment body")
	}
	if contentDigest(observed) != contentDigest(desc) {
		if err := m.api.call(ctx, "jira", "PUT", path, map[string]any{"body": desc}, nil); err != nil {
			return err
		}
		if err := m.api.call(ctx, "jira", "GET", path, nil, &actualJira); err != nil {
			return err
		}
		if json.Unmarshal(actualJira.Body, &observed) != nil || contentDigest(observed) != contentDigest(desc) {
			return fmt.Errorf("Jira comment readback mismatch")
		}
	}
	item.ProgressHash = hash
	return nil
}
