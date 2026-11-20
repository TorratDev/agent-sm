package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

func (m *manager) ownership(ctx context.Context) error {
	for _, w := range m.profile.Management.Workflows {
		var s struct{ State string }
		if err := m.api.call(ctx, "github", "GET", "/repos/"+m.profile.Management.Repo+"/actions/workflows/"+url.PathEscape(w), nil, &s); err != nil {
			return err
		}
		if !strings.HasPrefix(s.State, "disabled_") {
			return fmt.Errorf("competing workflow %s is active; synchronization blocked", w)
		}
	}
	return nil
}

func (m *manager) register(ctx context.Context, n int, active bool, pr int, progress string) error {
	return m.locked(ctx, func() error {
		if m.pendingFor(n) {
			return fmt.Errorf("issue has an incomplete approved structural change")
		}
		gh, err := m.ghIssue(ctx, n)
		if err != nil {
			return err
		}
		meta, err := issueMetadata(gh.Body)
		if err != nil || meta.JiraKey == "" || meta.Base == "" || meta.JiraURL != strings.TrimRight(m.profile.Jira.Site, "/")+"/browse/"+meta.JiraKey {
			return fmt.Errorf("service management requires verified Jira counterpart and Base branch metadata")
		}
		jira, err := m.jiraIssue(ctx, meta.JiraKey)
		if err != nil {
			return err
		}
		if !hasBacklink(jira.Fields.Description, gh.URL) {
			return fmt.Errorf("Jira counterpart has no exact GitHub backlink")
		}
		if active && gh.State != "open" {
			return fmt.Errorf("closed issue cannot start implementation")
		}
		key := strconv.Itoa(n)
		item := m.state.Items[key]
		if item == nil {
			item = &ManagedItem{Number: n, JiraKey: meta.JiraKey, Base: strings.TrimPrefix(meta.Base, "origin/")}
			m.state.Items[key] = item
		}
		if item.JiraKey != meta.JiraKey || item.Base != strings.TrimPrefix(meta.Base, "origin/") {
			return fmt.Errorf("managed issue mapping/base changed; approved structural reconciliation required")
		}
		item.Active = item.Active || active
		if active {
			item.AssignmentPaused = false
		}
		if active || pr > 0 {
			item.Delivered = false
		}
		if progress != "" {
			item.Progress = progress
		}
		if pr > 0 {
			found := false
			for _, existing := range item.PRs {
				found = found || existing == pr
			}
			if !found {
				item.PRs = append(item.PRs, pr)
			}
		}
		item.Next = time.Time{}
		return m.save()
	})
}

func (m *manager) assign(ctx context.Context, n int, key, login string) error {
	if err := m.checkIdentity(ctx, login); err != nil {
		return err
	}
	gh, err := m.ghIssue(ctx, n)
	if err != nil {
		return err
	}
	found := false
	for _, p := range gh.Assignees {
		found = found || p.Login == login
	}
	if !found {
		if err := m.api.call(ctx, "github", "POST", m.endpoint(n, "/assignees"), map[string]any{"assignees": []string{login}}, nil); err != nil {
			return err
		}
		verified, err := m.ghIssue(ctx, n)
		if err != nil {
			return err
		}
		for _, p := range verified.Assignees {
			found = found || p.Login == login
		}
		if !found {
			return fmt.Errorf("GitHub assignment readback mismatch")
		}
	}
	jira, err := m.jiraIssue(ctx, key)
	if err != nil {
		return err
	}
	account := m.profile.Management.Identities[login]
	if jira.Fields.Assignee == nil || jira.Fields.Assignee.AccountID != account {
		return m.jiraEdit(ctx, key, map[string]any{"assignee": map[string]string{"accountId": account}})
	}
	return nil
}

type timelineEvent struct {
	Event    string                 `json:"event"`
	Created  string                 `json:"created_at"`
	Assignee struct{ Login string } `json:"assignee"`
	Source   struct {
		Issue struct {
			Number int             `json:"number"`
			URL    string          `json:"html_url"`
			PR     json.RawMessage `json:"pull_request"`
		} `json:"issue"`
	} `json:"source"`
}

func (m *manager) closingPR(ctx context.Context, n int) (linkedPR, error) {
	parts := strings.Split(m.profile.Management.Repo, "/")
	var result linkedPR
	cursor := ""
	for page := 0; page < 1000; page++ {
		query := `query($owner:String!,$repo:String!,$n:Int!,$cursor:String){repository(owner:$owner,name:$repo){pullRequest(number:$n){title body state isDraft baseRefName url mergedAt closingIssuesReferences(first:100,after:$cursor){nodes{url number} pageInfo{hasNextPage endCursor}}}}}`
		input := map[string]any{"query": query, "variables": map[string]any{"owner": parts[0], "repo": parts[1], "n": n, "cursor": nil}}
		if cursor != "" {
			input["variables"].(map[string]any)["cursor"] = cursor
		}
		var response struct {
			Errors []json.RawMessage `json:"errors"`
			Data   struct {
				Repository struct {
					PR *struct {
						Title, Body, State, BaseRefName, URL, MergedAt string
						IsDraft                                        bool
						Closing                                        struct {
							Nodes []struct {
								URL    string
								Number int
							}
							Page struct {
								HasNext bool   `json:"hasNextPage"`
								Cursor  string `json:"endCursor"`
							} `json:"pageInfo"`
						} `json:"closingIssuesReferences"`
					} `json:"pullRequest"`
				} `json:"repository"`
			} `json:"data"`
		}
		if err := m.api.call(ctx, "github", "POST", "/graphql", input, &response); err != nil {
			return result, err
		}
		pr := response.Data.Repository.PR
		if len(response.Errors) > 0 || pr == nil || pr.URL != "https://"+m.profile.Management.Host+"/"+m.profile.Management.Repo+"/pull/"+strconv.Itoa(n) {
			return result, fmt.Errorf("incomplete GitHub PR/closing-reference evidence")
		}
		result.Title, result.Body, result.State, result.BaseRefName, result.URL, result.IsDraft, result.MergedAt = pr.Title, pr.Body, pr.State, pr.BaseRefName, pr.URL, pr.IsDraft, pr.MergedAt
		result.ClosingIssuesReferences = append(result.ClosingIssuesReferences, pr.Closing.Nodes...)
		if !pr.Closing.Page.HasNext {
			return result, nil
		}
		if pr.Closing.Page.Cursor == "" || pr.Closing.Page.Cursor == cursor {
			return result, fmt.Errorf("incomplete closing-reference pagination")
		}
		cursor = pr.Closing.Page.Cursor
	}
	return result, fmt.Errorf("closing-reference pagination limit reached")
}

func (m *manager) transition(ctx context.Context, s *jiraSnapshot, target string) error {
	if s.Fields.Status.Name == target {
		return nil
	}
	current, err := m.jiraIssue(ctx, s.Key)
	if err != nil {
		return err
	}
	s = current
	actual := s.Fields.Status.Name
	if actual == target {
		return nil
	}
	rank := func(status string) int {
		switch status {
		case m.profile.Jira.InProgress:
			return 1
		case m.profile.Jira.InReview:
			return 2
		case m.profile.Jira.Delivered:
			return 3
		}
		for _, initial := range m.profile.Jira.InitialStatuses {
			if initial == status {
				return 0
			}
		}
		return -1
	}
	if rank(actual) < 0 || rank(target) < 0 {
		return fmt.Errorf("unrecognized Jira status %q; configured workflow policy required", actual)
	}
	if rank(actual) > rank(target) {
		return nil
	}
	var response struct {
		Transitions []struct {
			ID     string                `json:"id"`
			To     struct{ Name string } `json:"to"`
			Fields map[string]struct {
				Required   bool
				HasDefault bool `json:"hasDefaultValue"`
			} `json:"fields"`
		} `json:"transitions"`
	}
	if err := m.api.call(ctx, "jira", "GET", "/issue/"+s.Key+"/transitions?expand=transitions.fields", nil, &response); err != nil {
		return err
	}
	id := ""
	for _, t := range response.Transitions {
		if t.To.Name == target {
			if id != "" {
				return fmt.Errorf("ambiguous Jira transition to %s", target)
			}
			for field, f := range t.Fields {
				if f.Required && !f.HasDefault {
					return fmt.Errorf("Jira transition requires field %s; human intervention required", field)
				}
			}
			id = t.ID
		}
	}
	if id == "" {
		return fmt.Errorf("no direct Jira transition from %s to %s; no implicit workflow traversal", actual, target)
	}
	if err := m.api.call(ctx, "jira", "POST", "/issue/"+s.Key+"/transitions", map[string]any{"transition": map[string]string{"id": id}}, nil); err != nil {
		return err
	}
	verified, err := m.jiraIssue(ctx, s.Key)
	if err == nil && verified.Fields.Status.Name != target {
		err = fmt.Errorf("Jira transition readback mismatch")
	}
	return err
}

func (m *manager) syncOne(ctx context.Context, item *ManagedItem) error {
	if m.pendingFor(item.Number) {
		return fmt.Errorf("approved structural changes are incomplete; lifecycle synchronization paused")
	}
	gh, err := m.ghIssue(ctx, item.Number)
	if err != nil {
		return err
	}
	meta, err := issueMetadata(gh.Body)
	if err != nil || meta.JiraKey != item.JiraKey || strings.TrimPrefix(meta.Base, "origin/") != item.Base || meta.JiraURL != strings.TrimRight(m.profile.Jira.Site, "/")+"/browse/"+item.JiraKey {
		return fmt.Errorf("managed counterpart/base drift; preview structural changes")
	}
	jira, err := m.jiraIssue(ctx, item.JiraKey)
	if err != nil {
		return err
	}
	if !hasBacklink(jira.Fields.Description, gh.URL) {
		return fmt.Errorf("Jira backlink conflict")
	}
	login := m.profile.Management.GitHubAssignee
	ghAssigned := false
	for _, person := range gh.Assignees {
		ghAssigned = ghAssigned || person.Login == login
	}
	assigned := !item.AssignmentPaused && gh.State == "open" && (item.Active || len(gh.Assignees) > 0)
	if !item.Active && assigned && len(gh.Assignees) > 0 {
		configuredPresent := false
		for _, person := range gh.Assignees {
			configuredPresent = configuredPresent || person.Login == login
		}
		if !configuredPresent {
			if len(gh.Assignees) != 1 || m.profile.Management.Identities[gh.Assignees[0].Login] == "" {
				return fmt.Errorf("GitHub assignment has no unique approved Jira identity")
			}
			login = gh.Assignees[0].Login
			ghAssigned = true
		}
	}
	if assigned && login != "" {
		account := m.profile.Management.Identities[login]
		if jira.Fields.Assignee != nil && jira.Fields.Assignee.AccountID != account {
			return fmt.Errorf("Jira assignee differs from service policy; explicit approval required")
		}
		if !ghAssigned || jira.Fields.Assignee == nil {
			if err := m.assign(ctx, item.Number, item.JiraKey, login); err != nil {
				return err
			}
		}
	}
	events, err := ghPages[timelineEvent](ctx, m, m.endpoint(item.Number, "/timeline"))
	if err != nil {
		return err
	}
	prIDs := map[int]bool{}
	for _, n := range item.PRs {
		prIDs[n] = true
	}
	reopened := ""
	start := ""
	for _, e := range events {
		if e.Event == "cross-referenced" && len(e.Source.Issue.PR) > 0 &&
			e.Source.Issue.URL == "https://"+m.profile.Management.Host+"/"+m.profile.Management.Repo+"/pull/"+strconv.Itoa(e.Source.Issue.Number) {
			prIDs[e.Source.Issue.Number] = true
		}
		if e.Event == "reopened" && e.Created > reopened {
			reopened = e.Created
		}
		if e.Event == "assigned" && e.Assignee.Login == login {
			t, parseErr := time.Parse(time.RFC3339, e.Created)
			if parseErr != nil {
				return fmt.Errorf("invalid assignment-event timestamp")
			}
			date := t.UTC().Format("2006-01-02")
			if start == "" || date < start {
				start = date
			}
		}
	}
	var prs []linkedPR
	for n := range prIDs {
		pr, err := m.closingPR(ctx, n)
		if err != nil {
			return err
		}
		if pr.State == "MERGED" && reopened != "" && pr.MergedAt < reopened {
			for _, ref := range pr.ClosingIssuesReferences {
				if ref.URL == gh.URL {
					return fmt.Errorf("issue reopened after verified delivery; human decision required")
				}
			}
		}
		prs = append(prs, pr)
	}
	target, err := jiraPhase(gh.URL, item.JiraKey, item.Base, assigned, prs, m.profile.Jira)
	if err != nil {
		return err
	}
	dateError := error(nil)
	if !item.AssignmentPaused && (item.Active || ghAssigned) && login != "" && m.profile.Jira.StartDateField != "" {
		var current struct {
			Fields map[string]json.RawMessage `json:"fields"`
		}
		if err := m.api.call(ctx, "jira", "GET", "/issue/"+item.JiraKey+"?fields="+m.profile.Jira.StartDateField, nil, &current); err != nil {
			return err
		}
		raw, ok := current.Fields[m.profile.Jira.StartDateField]
		if !ok {
			return fmt.Errorf("configured Start-date field is not visible")
		}
		if string(raw) == "null" || string(raw) == `""` {
			if start == "" {
				dateError = fmt.Errorf("Start date pending: verified assignment event unavailable")
			} else if err := m.jiraEdit(ctx, item.JiraKey, map[string]any{m.profile.Jira.StartDateField: start}); err != nil {
				return err
			}
		}
	}
	if target != "" {
		if err := m.transition(ctx, jira, target); err != nil {
			return err
		}
	}
	if target == m.profile.Jira.Delivered && gh.State != "closed" {
		if err := m.api.call(ctx, "github", "PATCH", m.endpoint(item.Number, ""), map[string]string{"state": "closed", "state_reason": "completed"}, nil); err != nil {
			return err
		}
		verified, err := m.ghIssue(ctx, item.Number)
		if err != nil {
			return err
		}
		if verified.State != "closed" {
			return fmt.Errorf("GitHub completion readback mismatch")
		}
	}
	text := "agent-sm progress\n" + item.Progress
	if target != "" {
		text += "\nEvidence-derived phase: " + target + " (later Jira states are preserved)"
	}
	if dateError != nil {
		text += "\n" + dateError.Error()
	}
	if err := m.progressComments(ctx, item, text); err != nil {
		return err
	}
	if target == m.profile.Jira.Delivered && dateError == nil {
		item.Active = false
		item.Delivered = true
	}
	return dateError
}

func (m *manager) reconcile(ctx context.Context, only int, force bool) error {
	return m.locked(ctx, func() error {
		if err := m.ownership(ctx); err != nil {
			m.state.Error = err.Error()
			if saveErr := m.save(); saveErr != nil {
				return saveErr
			}
			return err
		}
		var failures []string
		for _, item := range m.state.Items {
			if only > 0 && item.Number != only {
				continue
			}
			if !force && (item.Delivered || time.Now().Before(item.Next)) {
				continue
			}
			err := m.syncOne(ctx, item)
			if err != nil {
				item.Status, item.Error = "pending/conflict", err.Error()
				item.Attempts++
				delay := time.Minute * time.Duration(1<<min(item.Attempts, 8))
				var limit retryError
				if errors.As(err, &limit) {
					delay = max(delay, limit.after)
				}
				item.Next = time.Now().Add(delay)
				failures = append(failures, fmt.Sprintf("#%d: %v", item.Number, err))
			} else {
				item.Status, item.Error, item.Attempts = "aligned", "", 0
				item.Next = time.Now().Add(time.Minute)
			}
			if err := m.save(); err != nil {
				return err
			}
		}
		var pending []string
		for _, item := range m.state.Items {
			if item.Error != "" {
				pending = append(pending, fmt.Sprintf("#%d: %s", item.Number, item.Error))
			}
		}
		for _, p := range m.state.Previews {
			if p.Started && !p.Complete && !p.Cancelled {
				message := p.Error
				if message == "" {
					message = "approved structural batch incomplete"
				}
				pending = append(pending, p.Plan.ID+": "+message)
			}
		}
		m.state.Error = strings.Join(pending, "; ")
		if err := m.save(); err != nil {
			return err
		}
		if len(failures) > 0 || (only == 0 && len(pending) > 0) {
			return fmt.Errorf("%s", m.state.Error)
		}
		return nil
	})
}

func (m *manager) watch(ctx context.Context) {
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		if err := m.resume(ctx); err != nil && ctx.Err() == nil {
			logEvent(map[string]any{"event": "management-recovery-pending", "err": err.Error()})
		}
		if err := m.reconcile(ctx, 0, false); err != nil && ctx.Err() == nil {
			logEvent(map[string]any{"event": "management-pending", "err": err.Error()})
		}

		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (m *manager) snapshot(ctx context.Context) ([]byte, error) {
	var b []byte
	err := m.locked(ctx, func() error {
		var err error
		b, err = json.Marshal(map[string]any{"enabled": true, "state": m.state})
		return err
	})
	return b, err
}

func (m *manager) pendingFor(n int) bool {
	for _, p := range m.state.Previews {
		if p.Started && !p.Complete && !p.Cancelled {
			for _, i := range p.Plan.Items {
				if i.Number == n {
					return true
				}
				if op := m.state.Ops[p.Digest+"/"+i.ID+"/github-create"]; op != nil && op.State == "done" {
					var s issueSnapshot
					if json.Unmarshal(op.Result, &s) != nil || s.Number == n {
						return true
					}
				}
			}
		}
	}
	return false
}

func (m *manager) resume(ctx context.Context) error {
	var digests []string
	if err := m.locked(ctx, func() error {
		for digest, p := range m.state.Previews {
			if p.Started && !p.Complete && !p.Cancelled && !time.Now().Before(p.Next) {
				digests = append(digests, digest)
			}
		}
		return nil
	}); err != nil {
		return err
	}
	var failures []string
	for _, digest := range digests {
		if err := m.apply(ctx, digest); err != nil {
			failures = append(failures, err.Error())
			if saveErr := m.locked(ctx, func() error {
				p := m.state.Previews[digest]
				p.Error, p.Attempts = err.Error(), p.Attempts+1
				delay := time.Minute * time.Duration(1<<min(p.Attempts, 8))
				var limit retryError
				if errors.As(err, &limit) {
					delay = max(delay, limit.after)
				}
				p.Next = time.Now().Add(delay)
				return m.save()
			}); saveErr != nil {
				return saveErr
			}
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("%s", strings.Join(failures, "; "))
	}
	return nil
}

func (m *manager) deactivate(ctx context.Context, n int, outcome string) error {
	return m.locked(ctx, func() error {
		if item := m.state.Items[strconv.Itoa(n)]; item != nil {
			item.Active = false
			item.AssignmentPaused = true
			item.Progress = "Coding outcome: " + outcome
		}
		return m.save()
	})
}
