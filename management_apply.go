package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"
)

type trackerError struct{ Status int }

func (e trackerError) Error() string { return fmt.Sprintf("tracker HTTP %d", e.Status) }

func (m *manager) parent(ctx context.Context, n int) ([]issueSnapshot, error) {
	var s issueSnapshot
	err := m.api.call(ctx, "github", "GET", m.endpoint(n, "/parent"), nil, &s)
	var status trackerError
	if errors.As(err, &status) && status.Status == 404 {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if s.ID == 0 || s.Number == 0 {
		return nil, fmt.Errorf("invalid GitHub parent")
	}
	return []issueSnapshot{s}, nil
}

func (m *manager) expected(ctx context.Context, plan WorkPlan) (map[string]expectedItem, error) {
	result := map[string]expectedItem{}
	for _, i := range plan.Items {
		e := expectedItem{}
		if i.Number > 0 {
			gh, err := m.ghIssue(ctx, i.Number)
			if err != nil {
				return nil, err
			}
			e.GitHub = gh
			e.Parent, err = m.parent(ctx, i.Number)
			if err != nil {
				return nil, err
			}
			e.Deps, err = ghPages[issueSnapshot](ctx, m, m.endpoint(i.Number, "/dependencies/blocked_by"))
			if err != nil {
				return nil, err
			}
			meta, err := issueMetadata(gh.Body)
			if err != nil {
				return nil, err
			}
			key := i.JiraKey
			if meta.JiraKey != "" {
				if key != "" && key != meta.JiraKey {
					return nil, fmt.Errorf("%s: conflicting explicit Jira mapping", i.ID)
				}
				if meta.JiraURL != strings.TrimRight(m.profile.Jira.Site, "/")+"/browse/"+meta.JiraKey {
					return nil, fmt.Errorf("%s: Jira site mismatch", i.ID)
				}
				key = meta.JiraKey
			}
			if key != "" {
				e.Jira, err = m.jiraIssue(ctx, key)
				if err != nil {
					return nil, err
				}
			}
		}
		result[i.ID] = e
	}
	return result, nil
}

func (m *manager) preview(ctx context.Context, plan WorkPlan) (*Preview, error) {
	if err := plan.validate(); err != nil {
		return nil, err
	}
	var result *Preview
	err := m.locked(ctx, func() error {
		for _, prior := range m.state.Previews {
			if prior.Plan.ID == plan.ID && contentDigest(prior.Plan) != contentDigest(plan) {
				return fmt.Errorf("plan id already has a different proposal; use a new id and explicit existing issue numbers")
			}
			if prior.Plan.ID == plan.ID && prior.Started && !prior.Complete && !prior.Cancelled {
				if prior.Policy != m.policy() {
					return fmt.Errorf("approved batch policy changed; restore it or cancel recovery and preview a new plan with explicit existing identities")
				}
				result = prior
				return nil
			}
		}
		if _, err := m.preflight(ctx, plan); err != nil {
			return err
		}
		expected, err := m.expected(ctx, plan)
		if err != nil {
			return err
		}
		counterparts := map[string]bool{}
		for _, e := range expected {
			if e.Jira != nil {
				if counterparts[e.Jira.Key] {
					return fmt.Errorf("two GitHub issues map to the same Jira counterpart")
				}
				counterparts[e.Jira.Key] = true
			}
		}
		if err := m.validateRemoteGraph(ctx, plan, expected); err != nil {
			return err
		}
		p := &Preview{Scope: m.scope, Policy: m.policy(), Plan: plan, Expected: expected}
		for _, i := range orderedItems(plan) {
			action := "create paired GitHub/Jira issues"
			if i.Number != 0 {
				action = fmt.Sprintf("edit/adopt GitHub #%d and its Jira counterpart", i.Number)
			}
			p.Operations = append(p.Operations, fmt.Sprintf("%s: %s (%s); title=%q; full requested body and expected remote fields are shown in plan/expected", i.ID, action, i.Kind, i.Title))
			if i.Parent != "" {
				p.Operations = append(p.Operations, i.ID+": set GitHub parent="+i.Parent+" and legal Jira parent")
			}
			if i.RemoveParent {
				p.Operations = append(p.Operations, i.ID+": remove existing GitHub/Jira parent")
			}
			if len(i.Labels)+len(i.RemoveLabels) > 0 {
				p.Operations = append(p.Operations, fmt.Sprintf("%s: add GitHub labels %v; remove GitHub labels %v; preserve other labels", i.ID, i.Labels, i.RemoveLabels))
			}
			for _, ref := range i.DependsOn {
				p.Operations = append(p.Operations, i.ID+": blocked by "+ref+" in GitHub and Jira")
			}
			for _, ref := range i.RemoveDependencies {
				p.Operations = append(p.Operations, i.ID+": remove blocked-by "+ref+" in GitHub and Jira")
			}
			if i.Assignee != "" {
				p.Operations = append(p.Operations, i.ID+": assign "+i.Assignee+" using the configured Jira identity")
			}
		}
		p.Digest = contentDigest(p)
		if old := m.state.Previews[p.Digest]; old != nil {
			result = old
			return nil
		}
		m.state.Previews[p.Digest], result = p, p
		return m.save()
	})
	return result, err
}

func approvalDigest(p Preview) string {
	p.Digest, p.Started, p.Complete = "", false, false
	p.Cancelled = false
	p.Error, p.Attempts, p.Next = "", 0, time.Time{}
	return contentDigest(p)
}

func (m *manager) apply(ctx context.Context, digest string) error {
	if !digestPattern.MatchString(digest) {
		return fmt.Errorf("approval requires an exact preview digest")
	}
	return m.locked(ctx, func() (applyErr error) {
		p := m.state.Previews[digest]
		if p == nil || p.Digest != digest || approvalDigest(*p) != digest || p.Scope != m.scope || p.Policy != m.policy() {
			return fmt.Errorf("unknown, altered or stale preview; preview again")
		}
		if p.Cancelled {
			return fmt.Errorf("batch recovery was cancelled; use a new plan id and explicit existing identities")
		}
		if err := p.Plan.validate(); err != nil {
			return err
		}
		if p.Complete {
			return nil
		}
		defer func() {
			if applyErr != nil {
				p.Error, m.state.Error = applyErr.Error(), p.Plan.ID+": "+applyErr.Error()
				var limit retryError
				if errors.As(applyErr, &limit) {
					p.Next = time.Now().Add(limit.after)
				}
				if saveErr := m.save(); saveErr != nil {
					applyErr = errors.Join(applyErr, saveErr)
				}
			}
		}()
		for otherDigest, other := range m.state.Previews {
			if otherDigest != digest && other.Started && !other.Complete && !other.Cancelled {
				return fmt.Errorf("finish approved batch %s before starting another", other.Plan.ID)
			}
		}
		types, err := m.preflight(ctx, p.Plan)
		if err != nil {
			return err
		}
		if !p.Started {
			actual, err := m.expected(ctx, p.Plan)
			if err != nil {
				return err
			}
			if contentDigest(actual) != contentDigest(p.Expected) {
				return fmt.Errorf("remote state changed; preview again")
			}
			if err := m.validateRemoteGraph(ctx, p.Plan, actual); err != nil {
				return err
			}
			p.Started = true
			if err := m.save(); err != nil {
				return err
			}
		} else {
			resolved := p.Plan
			resolved.Items = append([]WorkItem{}, p.Plan.Items...)
			for index := range resolved.Items {
				i := &resolved.Items[index]
				if op := m.state.Ops[digest+"/"+i.ID+"/github-create"]; op != nil && op.State == "done" {
					var s issueSnapshot
					if json.Unmarshal(op.Result, &s) != nil {
						return fmt.Errorf("invalid creation journal")
					}
					i.Number = s.Number
				}
				if op := m.state.Ops[digest+"/"+i.ID+"/jira-create"]; op != nil && op.State == "done" {
					var s jiraSnapshot
					if json.Unmarshal(op.Result, &s) != nil {
						return fmt.Errorf("invalid creation journal")
					}
					i.JiraKey = s.Key
				}
			}
			actual, err := m.expected(ctx, resolved)
			if err != nil {
				return err
			}
			if err := m.validateRemoteGraph(ctx, resolved, actual); err != nil {
				return err
			}
		}
		ghs, jiras := map[string]*issueSnapshot{}, map[string]*jiraSnapshot{}
		for _, i := range orderedItems(p.Plan) {
			gh, err := m.ensureGitHub(ctx, p, i)
			if err != nil {
				return err
			}
			ghs[i.ID] = gh
		}
		for _, i := range orderedItems(p.Plan) {
			if i.Parent == "" && !i.RemoveParent {
				continue
			}
			parent, child := ghs[i.Parent], ghs[i.ID]
			err := m.op(ctx, digest+"/"+i.ID+"/github-parent", false, func(bool) (any, error) {
				current, err := m.parent(ctx, child.Number)
				if err != nil {
					return nil, err
				}
				if (i.RemoveParent && len(current) == 0) || (parent != nil && len(current) == 1 && current[0].ID == parent.ID) {
					return nil, nil
				}
				if !sameIssueIDs(current, p.Expected[i.ID].Parent) {
					return nil, fmt.Errorf("GitHub parent changed since preview")
				}
				if i.RemoveParent {
					if err := m.api.call(ctx, "github", "DELETE", m.endpoint(current[0].Number, "/sub_issue"), map[string]any{"sub_issue_id": child.ID}, nil); err != nil {
						return nil, err
					}
				} else {
					if err := m.api.call(ctx, "github", "POST", m.endpoint(parent.Number, "/sub_issues"), map[string]any{"sub_issue_id": child.ID, "replace_parent": true}, nil); err != nil {
						return nil, err
					}
				}
				actual, err := m.parent(ctx, child.Number)
				if err == nil && ((i.RemoveParent && len(actual) != 0) || (!i.RemoveParent && (len(actual) != 1 || actual[0].ID != parent.ID))) {
					err = fmt.Errorf("GitHub parent readback mismatch")
				}
				return nil, err
			})
			if err != nil {
				return err
			}
		}
		for _, i := range orderedItems(p.Plan) {
			parent := m.jiraParent(p.Plan, i, jiras)
			jira, err := m.ensureJira(ctx, p, i, ghs[i.ID], types[m.typeName(i)], parent)
			if err != nil {
				return err
			}
			jiras[i.ID] = jira
			if err := m.linkAndEdit(ctx, p, i, ghs[i.ID], jiras[i.ID]); err != nil {
				return err
			}
		}
		for _, i := range p.Plan.Items {
			for _, remove := range []bool{false, true} {
				refs := i.DependsOn
				if remove {
					refs = i.RemoveDependencies
				}
				for _, ref := range refs {
					if err := m.mirrorDependency(ctx, digest+"/"+i.ID+"/"+ref+"/"+strconv.FormatBool(remove), ghs[i.ID], ghs[ref], jiras[i.ID], jiras[ref], remove); err != nil {
						return err
					}
				}
			}
		}
		for _, i := range p.Plan.Items {
			n := ghs[i.ID].Number
			managed := m.state.Items[strconv.Itoa(n)]
			if managed == nil {
				managed = &ManagedItem{Number: n}
				m.state.Items[strconv.Itoa(n)] = managed
			}
			managed.JiraKey, managed.Base, managed.Status, managed.Error = jiras[i.ID].Key, m.itemBase(i), "aligned", ""
			managed.Delivered = false
		}
		if err := m.verifyPaired(ctx, p, ghs, jiras); err != nil {
			return err
		}
		p.Complete, p.Error, m.state.Error = true, "", ""
		return m.save()
	})
}

func sameIssueIDs(a, b []issueSnapshot) bool {
	if len(a) != len(b) {
		return false
	}
	for _, x := range a {
		found := false
		for _, y := range b {
			found = found || x.ID == y.ID
		}
		if !found {
			return false
		}
	}
	return true
}

func (m *manager) itemBase(i WorkItem) string {
	base := i.Base
	if base == "" {
		base = m.profile.Base
	}
	return strings.TrimPrefix(base, "origin/")
}

func itemMarker(p WorkPlan, i WorkItem) string { return "agent-sm:" + p.ID + ":" + i.ID }

func (m *manager) ensureGitHub(ctx context.Context, p *Preview, i WorkItem) (*issueSnapshot, error) {
	if i.Number > 0 {
		return m.ghIssue(ctx, i.Number)
	}
	opID := p.Digest + "/" + i.ID + "/github-create"
	err := m.op(ctx, opID, true, func(uncertain bool) (any, error) {
		marker := "<!-- " + itemMarker(p.Plan, i) + " -->"
		all, err := ghPages[issueSnapshot](ctx, m, "/repos/"+m.profile.Management.Repo+"/issues?state=all")
		if err != nil {
			return nil, err
		}
		var matches []issueSnapshot
		for _, s := range all {
			if strings.Contains(s.Body, marker) {
				matches = append(matches, s)
			}
		}
		if len(matches) > 1 {
			return nil, fmt.Errorf("ambiguous GitHub creation marker")
		}
		if len(matches) == 1 {
			if matches[0].Title != i.Title || matches[0].Body != i.Body+"\n\n"+marker {
				return nil, fmt.Errorf("existing GitHub marker has changed content; preview an explicit existing issue")
			}
			return matches[0], nil
		}
		if uncertain {
			return nil, fmt.Errorf("GitHub create outcome unknown; inspect marker %s before recovery, never blindly recreate", marker)
		}
		input := map[string]any{"title": i.Title, "body": i.Body + "\n\n" + marker}
		if i.GitHubType != "" {
			input["type"] = i.GitHubType
		}
		if len(i.Labels) > 0 {
			input["labels"] = i.Labels
		}
		var created issueSnapshot
		if err := m.api.call(ctx, "github", "POST", "/repos/"+m.profile.Management.Repo+"/issues", input, &created); err != nil {
			return nil, err
		}
		actual, err := m.ghIssue(ctx, created.Number)
		if err == nil && (actual.Title != i.Title || !strings.Contains(actual.Body, marker)) {
			err = fmt.Errorf("GitHub create readback mismatch")
		}
		return actual, err
	})
	if err != nil {
		return nil, err
	}
	var s issueSnapshot
	if err := json.Unmarshal(m.state.Ops[opID].Result, &s); err != nil {
		return nil, err
	}
	return m.ghIssue(ctx, s.Number)
}

func (m *manager) jiraParent(plan WorkPlan, i WorkItem, resolved map[string]*jiraSnapshot) string {
	if i.Kind == "epic" || i.RemoveParent {
		return ""
	}
	if i.Kind == "task" {
		return resolved[i.Parent].Key
	}
	parent := i.Parent
	for parent != "" {
		for _, candidate := range plan.Items {
			if candidate.ID == parent {
				if candidate.Kind == "epic" {
					return resolved[candidate.ID].Key
				}
				parent = candidate.Parent
				break
			}
		}
	}
	return m.profile.Jira.Epic
}

func adf(text string) map[string]any {
	var nodes []any
	for _, line := range strings.Split(text, "\n") {
		paragraph := map[string]any{"type": "paragraph"}
		if line != "" {
			paragraph["content"] = []any{map[string]any{"type": "text", "text": line}}
		}
		nodes = append(nodes, paragraph)
	}
	return map[string]any{"type": "doc", "version": 1, "content": nodes}
}

func appendADF(raw json.RawMessage, text string) (map[string]any, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return adf(text), nil
	}
	var doc map[string]any
	if json.Unmarshal(raw, &doc) != nil || doc["type"] != "doc" {
		return nil, fmt.Errorf("unsupported Jira description; refuse destructive conversion")
	}
	nodes, ok := doc["content"].([]any)
	if !ok {
		return nil, fmt.Errorf("invalid Jira ADF")
	}
	doc["content"] = append(nodes, adf(text)["content"].([]any)...)
	return doc, nil
}

func (m *manager) findJira(ctx context.Context, ghURL, marker, typeID, parent string) (*jiraSnapshot, error) {
	jql := `project = "` + m.profile.Jira.Project + `" AND text ~ "` + strings.ReplaceAll(ghURL, `"`, `\"`) + `"`
	token := ""
	var matches []*jiraSnapshot
	for page := 0; page < 1000; page++ {
		var response struct {
			Issues []jiraSnapshot `json:"issues"`
			IsLast bool           `json:"isLast"`
			Next   string         `json:"nextPageToken"`
		}
		path := "/search/jql?jql=" + url.QueryEscape(jql) + "&fields=description,project,issuetype,parent&maxResults=100"
		if token != "" {
			path += "&nextPageToken=" + url.QueryEscape(token)
		}
		if err := m.api.call(ctx, "jira", "GET", path, nil, &response); err != nil {
			return nil, err
		}
		for _, s := range response.Issues {
			if hasBacklink(s.Fields.Description, ghURL) {
				if s.Fields.Project.Key != m.profile.Jira.Project || s.Fields.IssueType.ID != typeID || (parent != "" && (s.Fields.Parent == nil || s.Fields.Parent.Key != parent)) {
					return nil, fmt.Errorf("Jira counterpart exists with conflicting type/parent")
				}
				copy := s
				matches = append(matches, &copy)
			}
		}
		if response.IsLast {
			if len(matches) > 1 {
				return nil, fmt.Errorf("ambiguous Jira counterpart")
			}
			if len(matches) == 1 {
				return matches[0], nil
			}
			return nil, nil
		}
		if response.Next == "" || response.Next == token {
			return nil, fmt.Errorf("incomplete Jira search pagination")
		}
		token = response.Next
	}
	return nil, fmt.Errorf("Jira search pagination limit reached")
}

func (m *manager) ensureJira(ctx context.Context, p *Preview, i WorkItem, gh *issueSnapshot, typeID, parent string) (*jiraSnapshot, error) {
	if existing := p.Expected[i.ID].Jira; existing != nil {
		return m.jiraIssue(ctx, existing.Key)
	}
	id := p.Digest + "/" + i.ID + "/jira-create"
	err := m.op(ctx, id, true, func(uncertain bool) (any, error) {
		found, err := m.findJira(ctx, gh.URL, itemMarker(p.Plan, i), typeID, parent)
		if err != nil {
			return nil, err
		}
		if found != nil {
			actual, err := m.jiraIssue(ctx, found.Key)
			if err != nil {
				return nil, err
			}
			var description any
			if json.Unmarshal(actual.Fields.Description, &description) != nil ||
				actual.Fields.Summary != i.Title ||
				contentDigest(description) != contentDigest(adf(i.Body+"\n\nGitHub: "+gh.URL+"\n"+itemMarker(p.Plan, i))) {
				return nil, fmt.Errorf("existing Jira counterpart needs an explicit jira_key in a fresh preview")
			}
			return actual, nil
		}
		if uncertain {
			return nil, fmt.Errorf("Jira create outcome unknown; reconcile exact GitHub backlink before recovery")
		}
		fields := map[string]any{"project": map[string]string{"key": m.profile.Jira.Project}, "issuetype": map[string]string{"id": typeID},
			"summary": i.Title, "description": adf(i.Body + "\n\nGitHub: " + gh.URL + "\n" + itemMarker(p.Plan, i))}
		for key, value := range m.profile.Jira.CreateFields[m.typeName(i)] {
			fields[key] = value
		}
		if parent != "" {
			fields["parent"] = map[string]string{"key": parent}
		}
		if i.Assignee != "" {
			fields["assignee"] = map[string]string{"accountId": m.profile.Management.Identities[i.Assignee]}
		}
		var created struct{ Key string }
		if err := m.api.call(ctx, "jira", "POST", "/issue", map[string]any{"fields": fields}, &created); err != nil {
			return nil, err
		}
		s, err := m.jiraIssue(ctx, created.Key)
		if err == nil && (s.Fields.Summary != i.Title || s.Fields.IssueType.ID != typeID || !hasBacklink(s.Fields.Description, gh.URL) ||
			(parent != "" && (s.Fields.Parent == nil || s.Fields.Parent.Key != parent))) {
			err = fmt.Errorf("Jira create readback mismatch")
		}
		return s, err
	})
	if err != nil {
		return nil, err
	}
	var s jiraSnapshot
	if err := json.Unmarshal(m.state.Ops[id].Result, &s); err != nil {
		return nil, err
	}
	return m.jiraIssue(ctx, s.Key)
}

func githubBody(body, jiraURL, base, marker string) string {
	var lines []string
	inJira, inserted := false, false
	baseIndex, annotatedBase := -1, ""
	metadata := []string{"- **Jira issue**: " + jiraURL, "- **Base branch**: `" + base + "`"}
	for _, line := range strings.Split(body, "\n") {
		clean := strings.TrimSpace(line)
		if strings.HasPrefix(clean, "<!-- agent-sm:") {
			continue
		}
		if strings.HasPrefix(clean, "## ") {
			inJira = strings.EqualFold(clean, "## Jira")
			if inJira && !inserted {
				lines = append(lines, line)
				lines = append(lines, metadata...)
				baseIndex = len(lines) - 1
				inserted = true
				continue
			}
		}
		key, value, _ := strings.Cut(strings.ReplaceAll(strings.TrimLeft(clean, "- "), "**", ""), ":")
		key = strings.ToLower(strings.TrimSpace(key))
		if key == "base branch" {
			if baseNote.MatchString(strings.TrimSpace(value)) {
				prefix, rest, _ := strings.Cut(line, "`")
				_, suffix, _ := strings.Cut(rest, "`")
				annotatedBase = prefix + "`" + base + "`" + suffix
			}
			continue
		}
		if inJira && key == "jira issue" {
			continue
		}
		lines = append(lines, line)
	}
	if !inserted {
		lines = append(lines, "", "## Jira")
		lines = append(lines, metadata...)
		baseIndex = len(lines) - 1
	}
	if annotatedBase != "" {
		lines[baseIndex] = annotatedBase
	}
	return strings.TrimSpace(strings.Join(lines, "\n")) + "\n\n<!-- " + marker + " -->"
}

func (m *manager) linkAndEdit(ctx context.Context, p *Preview, i WorkItem, gh *issueSnapshot, jira *jiraSnapshot) error {
	id := p.Digest + "/" + i.ID
	oldGH, oldJira := p.Expected[i.ID].GitHub, p.Expected[i.ID].Jira
	if oldGH == nil {
		var created issueSnapshot
		op := m.state.Ops[id+"/github-create"]
		if op == nil || json.Unmarshal(op.Result, &created) != nil {
			return fmt.Errorf("missing GitHub creation evidence")
		}
		oldGH = &created
	}
	if oldJira == nil {
		var created jiraSnapshot
		op := m.state.Ops[id+"/jira-create"]
		if op == nil || json.Unmarshal(op.Result, &created) != nil {
			return fmt.Errorf("missing Jira creation evidence")
		}
		oldJira = &created
	}
	err := m.op(ctx, id+"/github-edit", false, func(bool) (any, error) {
		actual, err := m.ghIssue(ctx, gh.Number)
		if err != nil {
			return nil, err
		}
		body := i.Body
		if body == "" {
			body = gh.Body
		}
		wanted := githubBody(body, strings.TrimRight(m.profile.Jira.Site, "/")+"/browse/"+jira.Key, m.itemBase(i), itemMarker(p.Plan, i))
		input := map[string]any{}
		if actual.Title != i.Title {
			if actual.Title != oldGH.Title {
				return nil, fmt.Errorf("GitHub title changed since preview")
			}
			input["title"] = i.Title
		}
		if actual.Body != wanted {
			if actual.Body != gh.Body {
				return nil, fmt.Errorf("GitHub body changed since preview")
			}
			if actual.Body != oldGH.Body {
				return nil, fmt.Errorf("GitHub body changed after partial apply")
			}
			input["body"] = wanted
		}
		if i.GitHubType != "" && (actual.Type == nil || actual.Type.Name != i.GitHubType) {
			if !reflect.DeepEqual(oldGH.Type, actual.Type) {
				return nil, fmt.Errorf("GitHub type changed since preview")
			}
			input["type"] = i.GitHubType
		}
		if len(i.Labels)+len(i.RemoveLabels) > 0 {
			set := map[string]bool{}
			for _, label := range actual.Labels {
				set[label.Name] = true
			}
			changed := false
			for _, label := range i.Labels {
				changed = changed || !set[label]
				set[label] = true
			}
			for _, label := range i.RemoveLabels {
				changed = changed || set[label]
				delete(set, label)
			}
			if changed {
				labels := []string{}
				for label := range set {
					labels = append(labels, label)
				}
				sort.Strings(labels)
				input["labels"] = labels
			}
		}
		if len(input) > 0 {
			if err := m.api.call(ctx, "github", "PATCH", m.endpoint(gh.Number, ""), input, nil); err != nil {
				return nil, err
			}
		}
		verified, err := m.ghIssue(ctx, gh.Number)
		if err == nil && (verified.Title != i.Title || verified.Body != wanted || (i.GitHubType != "" && (verified.Type == nil || verified.Type.Name != i.GitHubType))) {
			err = fmt.Errorf("GitHub edit readback mismatch")
		}
		return nil, err
	})
	if err != nil {
		return err
	}
	return m.op(ctx, id+"/jira-edit", false, func(bool) (any, error) {
		actual, err := m.jiraIssue(ctx, jira.Key)
		if err != nil {
			return nil, err
		}
		fields := map[string]any{}
		old := oldJira
		if actual.Fields.Summary != i.Title {
			if old != nil && actual.Fields.Summary != old.Fields.Summary {
				return nil, fmt.Errorf("Jira summary changed since preview")
			}

			fields["summary"] = i.Title
		}
		var desc map[string]any
		if i.Body != "" {
			desc = adf(i.Body + "\n\nGitHub: " + gh.URL + "\n" + itemMarker(p.Plan, i))
		} else if !hasBacklink(actual.Fields.Description, gh.URL) {
			desc, err = appendADF(actual.Fields.Description, "GitHub: "+gh.URL)
			if err != nil {
				return nil, err
			}
		}
		if desc != nil {
			var observed any
			if json.Unmarshal(actual.Fields.Description, &observed) != nil {
				return nil, fmt.Errorf("invalid Jira description")
			}
			if contentDigest(desc) != contentDigest(observed) {
				if old != nil && contentDigest(actual.Fields.Description) != contentDigest(old.Fields.Description) {
					return nil, fmt.Errorf("Jira description changed since preview")
				}
				fields["description"] = desc
			}
		}
		if m.typeName(i) != actual.Fields.IssueType.Name {
			if old != nil && actual.Fields.IssueType.ID != old.Fields.IssueType.ID {
				return nil, fmt.Errorf("Jira type changed since preview")
			}
			fields["issuetype"] = map[string]string{"name": m.typeName(i)}
		}
		parent := ""
		if i.Parent != "" || i.Number == 0 {
			resolved := map[string]*jiraSnapshot{}
			for _, other := range p.Plan.Items {
				if e := p.Expected[other.ID].Jira; e != nil {
					resolved[other.ID] = e
				} else if op := m.state.Ops[p.Digest+"/"+other.ID+"/jira-create"]; op != nil && op.State == "done" {
					var s jiraSnapshot
					if json.Unmarshal(op.Result, &s) != nil {
						return nil, fmt.Errorf("invalid Jira creation journal")
					}
					resolved[other.ID] = &s
				}
			}
			parent = m.jiraParent(p.Plan, i, resolved)
		}
		if parent != "" && (actual.Fields.Parent == nil || actual.Fields.Parent.Key != parent) {
			if old != nil && !reflect.DeepEqual(actual.Fields.Parent, old.Fields.Parent) {
				return nil, fmt.Errorf("Jira parent changed since preview")
			}
			fields["parent"] = map[string]string{"key": parent}
		}
		if i.RemoveParent && actual.Fields.Parent != nil {
			if !reflect.DeepEqual(actual.Fields.Parent, old.Fields.Parent) {
				return nil, fmt.Errorf("Jira parent changed since preview")
			}
			fields["parent"] = nil
		}
		if len(fields) > 0 {
			if err := m.jiraEdit(ctx, jira.Key, fields); err != nil {
				return nil, err
			}
		}
		if i.Assignee != "" {
			return nil, m.assign(ctx, gh.Number, jira.Key, i.Assignee)
		}
		return nil, nil
	})
}

func (m *manager) blocksType(ctx context.Context) (string, error) {
	var response struct {
		Types []struct{ ID, Name, Inward, Outward string } `json:"issueLinkTypes"`
	}
	if err := m.api.call(ctx, "jira", "GET", "/issueLinkType", nil, &response); err != nil {
		return "", err
	}
	id := ""
	for _, t := range response.Types {
		if strings.EqualFold(t.Outward, "blocks") && strings.EqualFold(t.Inward, "is blocked by") {
			if id != "" {
				return "", fmt.Errorf("ambiguous Jira Blocks link type")
			}
			id = t.ID
		}
	}
	if id == "" {
		return "", fmt.Errorf("directional Jira Blocks link type unavailable")
	}
	return id, nil
}

func (m *manager) mirrorDependency(ctx context.Context, id string, dependent, blocker *issueSnapshot, dependentJira, blockerJira *jiraSnapshot, remove bool) error {
	if err := m.op(ctx, id+"/github", false, func(bool) (any, error) {
		actual, err := ghPages[issueSnapshot](ctx, m, m.endpoint(dependent.Number, "/dependencies/blocked_by"))
		if err != nil {
			return nil, err
		}
		exists := false
		for _, s := range actual {
			exists = exists || s.ID == blocker.ID
		}
		if exists == !remove {
			return nil, nil
		}
		path, method := m.endpoint(dependent.Number, "/dependencies/blocked_by"), "POST"
		var input any = map[string]int64{"issue_id": blocker.ID}
		if remove {
			path, method, input = path+"/"+strconv.FormatInt(blocker.ID, 10), "DELETE", nil
		}
		if err := m.api.call(ctx, "github", method, path, input, nil); err != nil {
			return nil, err
		}
		verified, err := ghPages[issueSnapshot](ctx, m, m.endpoint(dependent.Number, "/dependencies/blocked_by"))
		exists = false
		for _, s := range verified {
			exists = exists || s.ID == blocker.ID
		}
		if err == nil && exists == remove {
			err = fmt.Errorf("GitHub dependency readback mismatch")
		}
		return nil, err
	}); err != nil {
		return err
	}
	return m.op(ctx, id+"/jira", false, func(bool) (any, error) {
		typeID, err := m.blocksType(ctx)
		if err != nil {
			return nil, err
		}
		find := func() (string, error) {
			s, err := m.jiraIssue(ctx, dependentJira.Key)
			if err != nil {
				return "", err
			}
			linkID := ""
			for _, link := range s.Fields.Links {
				if link.Type.ID == typeID && link.InwardIssue != nil && link.InwardIssue.Key == blockerJira.Key {
					if linkID != "" {
						return "", fmt.Errorf("duplicate Jira dependency links")
					}
					linkID = link.ID
				}
			}
			return linkID, nil
		}
		linkID, err := find()
		if err != nil || (linkID != "") == !remove {
			return nil, err
		}
		if remove {
			err = m.api.call(ctx, "jira", "DELETE", "/issueLink/"+url.PathEscape(linkID), nil, nil)
		} else {
			err = m.api.call(ctx, "jira", "POST", "/issueLink", map[string]any{"type": map[string]string{"id": typeID},
				"inwardIssue": map[string]string{"key": blockerJira.Key}, "outwardIssue": map[string]string{"key": dependentJira.Key}}, nil)
		}
		if err != nil {
			return nil, err
		}
		linkID, err = find()
		if err == nil && (linkID != "") == remove {
			err = fmt.Errorf("Jira dependency readback mismatch")
		}
		return nil, err
	})
}
