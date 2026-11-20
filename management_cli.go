package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const workPlanSchema = `{"type":"object","properties":{"id":{"type":"string"},"items":{"type":"array","items":{"type":"object","properties":{"id":{"type":"string"},"title":{"type":"string"},"body":{"type":"string"},"kind":{"enum":["epic","story","task"]},"number":{"type":"integer"},"jira_key":{"type":"string"},"parent":{"type":"string"},"depends_on":{"type":"array","items":{"type":"string"}},"remove_dependencies":{"type":"array","items":{"type":"string"}},"base":{"type":"string"},"assignee":{"type":"string"},"github_type":{"type":"string"},"remove_parent":{"type":"boolean"},"labels":{"type":"array","items":{"type":"string"}},"remove_labels":{"type":"array","items":{"type":"string"}}},"required":["id","title","body","kind"],"additionalProperties":false}}},"required":["id","items"],"additionalProperties":false}`

func decodeWorkPlan(b []byte) (WorkPlan, error) {
	var plan WorkPlan
	if err := validateResult(b, workPlanSchema, &plan); err != nil {
		return plan, err
	}
	return plan, plan.validate()
}

func urlSegment(s string) string { return url.PathEscape(s) }

func issuesCLI(ctx context.Context, args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: issues preview REPO PLAN.json | apply REPO DIGEST | cancel REPO DIGEST | status REPO")
	}
	m, err := newManager(ctx, args[1], settings)
	if err != nil {
		return err
	}
	switch {
	case args[0] == "preview" && len(args) == 3:
		f, err := os.Open(args[2])
		if err != nil {
			return err
		}
		defer f.Close()
		b, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
		if err != nil || len(b) > 1<<20 {
			return fmt.Errorf("cannot read work plan (limit 1 MiB)")
		}
		plan, err := decodeWorkPlan(b)
		if err != nil {
			return err
		}
		preview, err := m.preview(ctx, plan)
		if err != nil {
			return err
		}
		b, err = json.MarshalIndent(preview, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(b))
		return nil
	case args[0] == "apply" && len(args) == 3:
		if err := m.apply(ctx, args[2]); err != nil {
			return err
		}
	case args[0] == "cancel" && len(args) == 3:
		if err := m.cancel(ctx, args[2]); err != nil {
			return err
		}
	case args[0] == "status" && len(args) == 2:
	default:
		return fmt.Errorf("usage: issues preview REPO PLAN.json | apply REPO DIGEST | cancel REPO DIGEST | status REPO")
	}
	b, err := m.snapshot(ctx)
	if err == nil {
		fmt.Println(string(b))
	}
	return err
}

func manageRun(c *Ctx, active bool, pr int, progress string, reconcile bool) error {
	if settings.Management.Mode != "service" {
		return nil
	}
	m, err := newManager(runCtx, c.Repo, settings)
	if err == nil {
		err = m.register(runCtx, c.Issue, active, pr, progress)
	}
	if err == nil && reconcile {
		err = m.reconcile(runCtx, c.Issue, true)
	}
	if err != nil {
		c.ManagementError = err.Error()
		logEvent(map[string]any{"event": "management-pending", "issue": c.Issue, "err": err.Error()})
	}
	return err
}

func pendingFollowUp(c *Ctx, exit Exit) error {
	plan := c.Proposal
	if plan == nil {
		reason := "work does not fit one PR"
		if exit == GaveUp {
			reason = "fix limit reached"
		}
		plan = &WorkPlan{ID: "follow-up-" + strconv.Itoa(c.Issue), Items: []WorkItem{{
			ID: "follow-up", Kind: "story", Title: c.Title + " (follow-up)",
			Body: fmt.Sprintf("Source: %s\n\nDone so far:\n%s\n\nRemaining work:\n%s\n%s\n\nReason: %s", c.IssueURL, c.Summary, c.Feedback, c.Evidence, reason),
		}}}
	}
	if err := plan.validate(); err != nil {
		return err
	}
	b, err := json.MarshalIndent(plan, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(c.Scratch, "issue-plan.json")
	if err := os.WriteFile(path, b, 0600); err != nil {
		return err
	}
	logEvent(map[string]any{"event": "issue-proposal-pending-approval", "path": path, "note": "preview and approve; no tracker changes performed"})
	return nil
}

func managementContext(c *Ctx) error {
	m, err := newManager(runCtx, c.Repo, settings)
	if err != nil {
		return err
	}
	return m.issueContext(runCtx, c)
}

func (m *manager) issueContext(ctx context.Context, c *Ctx) error {
	var managedComment githubComment
	if err := m.locked(ctx, func() error {
		if op := m.state.Ops["progress/github/"+strconv.Itoa(c.Issue)]; op != nil && op.State == "done" {
			return json.Unmarshal(op.Result, &managedComment)
		}
		return nil
	}); err != nil {
		return err
	}
	comments, err := ghPages[githubComment](ctx, m, m.endpoint(c.Issue, "/comments"))
	if err != nil {
		return fmt.Errorf("issue comments unavailable: %w", err)
	}
	var text strings.Builder
	for _, comment := range comments {
		if comment.ID <= 0 || comment.User.Login == "" {
			return fmt.Errorf("incomplete GitHub issue comment")
		}
		if comment.ID == managedComment.ID && comment.User.Login == managedComment.User.Login {
			continue
		}
		if text.Len()+len(comment.Body)+len(c.Body)+2 > 1<<20 {
			return fmt.Errorf("issue context exceeds 1 MiB; curate or split the issue before starting an agent")
		}
		text.WriteString("\n\n")
		text.WriteString(comment.Body)
	}
	c.Comments = text.String()
	return nil
}
