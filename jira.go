package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type JiraConfig struct {
	Mode            string                                `json:"mode,omitempty"` // off (empty), or actions: read-only reconciliation
	Site            string                                `json:"site,omitempty"`
	APIBase         string                                `json:"api_base,omitempty"`
	InProgress      string                                `json:"in_progress,omitempty"`
	InReview        string                                `json:"in_review,omitempty"`
	Delivered       string                                `json:"delivered,omitempty"`
	Project         string                                `json:"project,omitempty"`
	Epic            string                                `json:"epic,omitempty"`
	StoryType       string                                `json:"story_type,omitempty"`
	TaskType        string                                `json:"task_type,omitempty"`
	EpicType        string                                `json:"epic_type,omitempty"`
	StartDateField  string                                `json:"start_date_field,omitempty"`
	CreateFields    map[string]map[string]json.RawMessage `json:"create_fields,omitempty"`
	InitialStatuses []string                              `json:"initial_statuses,omitempty"`
}

func (j JiraConfig) validate() error {
	if j.Mode == "" {
		return nil
	}
	if j.Mode != "actions" && j.Mode != "service" {
		return fmt.Errorf("jira.mode must be empty, actions or service")
	}
	for i, s := range []string{j.Site, j.APIBase} {
		if s == "" && j.Site != "" {
			continue
		}
		u, err := url.Parse(s)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return fmt.Errorf("Jira site/API base must be an explicit HTTPS URL without credentials")
		}
		if i == 0 && u.Path != "" && u.Path != "/" {
			return fmt.Errorf("jira.site must be a browser origin, without an API path")
		}
	}
	if j.InProgress == "" || j.InReview == "" || j.Delivered == "" {
		return fmt.Errorf("configure the Jira workflow's exact in_progress, in_review and delivered statuses")
	}
	if j.Mode == "service" {
		if j.InProgress == j.InReview || j.InProgress == j.Delivered || j.InReview == j.Delivered ||
			j.StoryType == j.TaskType || j.StoryType == j.EpicType || j.TaskType == j.EpicType {
			return fmt.Errorf("service lifecycle statuses and issue types must be distinct")
		}
		for _, initial := range j.InitialStatuses {
			if strings.TrimSpace(initial) == "" || initial == j.InProgress || initial == j.InReview || initial == j.Delivered {
				return fmt.Errorf("initial_statuses must name earlier, non-managed workflow states")
			}
		}
		if !regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`).MatchString(j.Project) || j.StoryType == "" || j.TaskType == "" || j.EpicType == "" {
			return fmt.Errorf("service Jira requires project and exact story_type, task_type and epic_type")
		}
		if j.Epic != "" && !jiraKeyPattern.MatchString(j.Epic) {
			return fmt.Errorf("invalid configured Jira Epic")
		}
		if j.StartDateField != "" && !regexp.MustCompile(`^customfield_[0-9]+$`).MatchString(j.StartDateField) {
			return fmt.Errorf("start_date_field must be a discovered customfield ID")
		}
		for kind, fields := range j.CreateFields {
			if kind != j.StoryType && kind != j.TaskType && kind != j.EpicType {
				return fmt.Errorf("create_fields must be keyed by configured issue-type names")
			}
			for key := range fields {
				if !regexp.MustCompile(`^customfield_[0-9]+$`).MatchString(key) {
					return fmt.Errorf("create_fields may only supply operator-owned custom fields")
				}
			}
		}
	}
	return nil
}

var (
	jiraURL  = regexp.MustCompile(`^https://[a-zA-Z0-9._:-]+/browse/([A-Z][A-Z0-9]*-[1-9][0-9]*)$`)
	httpURL  = regexp.MustCompile(`https?://[^\s<>\])]+`)
	baseName = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_./-]*$`)
	baseNote = regexp.MustCompile("^`([^`]+)`\\s+\\([^`]*\\)\\.?$")
)

func validBase(s string) bool {
	if !baseName.MatchString(s) || strings.Contains(s, "..") || strings.Contains(s, "//") || strings.HasSuffix(s, "/") {
		return false
	}
	for _, part := range strings.Split(s, "/") {
		if strings.HasPrefix(part, ".") || strings.HasPrefix(part, "-") || strings.HasSuffix(part, ".lock") || strings.HasSuffix(part, ".") {
			return false
		}
	}
	return true
}

type issueMeta struct {
	Base    string
	JiraURL string
	JiraKey string
}

func issueMetadata(body string) (issueMeta, error) {
	var meta issueMeta
	inJira := false
	baseSeen, jiraSeen := false, false
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "## ") {
			inJira = strings.EqualFold(strings.TrimSpace(strings.TrimPrefix(line, "## ")), "Jira")
			continue
		}
		clean := strings.ReplaceAll(strings.TrimLeft(line, "- "), "**", "")
		key, value, ok := strings.Cut(clean, ":")
		if !ok {
			continue
		}
		rawValue := strings.TrimSpace(value)
		value = strings.Trim(rawValue, "`")
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "base branch":
			if match := baseNote.FindStringSubmatch(rawValue); match != nil {
				value = match[1]
			}
			if baseSeen || !validBase(value) {
				return meta, fmt.Errorf("missing, invalid or duplicate Base branch metadata")
			}
			baseSeen = true
			meta.Base = value
			if !strings.HasPrefix(value, "origin/") {
				meta.Base = "origin/" + value
			}
		case "jira issue":
			if !inJira {
				continue
			}
			urls := httpURL.FindAllString(value, -1)
			if jiraSeen || len(urls) != 1 {
				return meta, fmt.Errorf("ambiguous Jira counterpart; use one Jira issue URL under ## Jira")
			}
			match := jiraURL.FindStringSubmatch(urls[0])
			if match == nil {
				return meta, fmt.Errorf("Jira issue must be a canonical HTTPS /browse/KEY URL")
			}
			jiraSeen = true
			meta.JiraURL, meta.JiraKey = match[0], match[1]
		}
	}
	return meta, nil
}

type JiraResult struct {
	Status string `json:"status"`
	Key    string `json:"key,omitempty"`
	Actual string `json:"actual,omitempty"`
	Target string `json:"target,omitempty"`
	Reason string `json:"reason,omitempty"`
}

type linkedPR struct {
	Title                   string
	Body                    string
	State                   string
	IsDraft                 bool
	BaseRefName             string
	URL                     string
	MergedAt                string
	ClosingIssuesReferences []struct {
		URL    string
		Number int
	}
}

func jiraPhase(issueURL, key, base string, assigned bool, prs []linkedPR, j JiraConfig) (string, error) {
	target, rank := "", 0
	if assigned {
		target, rank = j.InProgress, 1
	}
	for _, pr := range prs {
		closes := false
		for _, ref := range pr.ClosingIssuesReferences {
			if ref.URL == issueURL {
				closes = true
			}
		}
		if !closes {
			continue // partial/related delivery is not lifecycle evidence
		}
		if pr.State != "MERGED" && (pr.State != "OPEN" || pr.IsDraft) {
			continue
		}
		if pr.BaseRefName != strings.TrimPrefix(base, "origin/") {
			return "", fmt.Errorf("closing PR %s targets a different base", pr.URL)
		}
		if !jiraTitleKey(pr.Title, key) {
			return "", fmt.Errorf("closing PR %s is missing the exact Jira title token", pr.URL)
		}
		if pr.State == "MERGED" {
			target, rank = j.Delivered, 3
		} else if rank < 2 {
			target, rank = j.InReview, 2
		}
	}
	return target, nil
}

func jiraTitleKey(title, key string) bool {
	token := regexp.MustCompile(`(^|[^A-Za-z0-9_-])` + regexp.QuoteMeta(key) + `([^A-Za-z0-9_-]|$)`)
	return token.MatchString(title)
}

func reconcileJira(c *Ctx) JiraResult {
	j := settings.Jira
	if j.Mode != "actions" && j.Mode != "service" {
		return JiraResult{Status: "skipped-unavailable", Reason: "Jira reconciliation is not configured"}
	}
	result := JiraResult{Status: "blocked"}
	out, err := sh(c.Repo, "gh", "issue", "view", strconv.Itoa(c.Issue), "--json", "url,body,assignees,state")
	var issue struct {
		URL, Body, State string
		Assignees        []json.RawMessage
	}
	if err != nil || json.Unmarshal([]byte(out), &issue) != nil {
		result.Status, result.Reason = "skipped-unavailable", "GitHub issue evidence unavailable"
		return result
	}
	meta, err := issueMetadata(issue.Body)
	if err != nil || meta.JiraKey == "" || meta.Base == "" {
		result.Reason = "one explicit Jira counterpart and Base branch are required"
		return result
	}
	result.Key = meta.JiraKey
	if meta.JiraURL != strings.TrimRight(j.Site, "/")+"/browse/"+meta.JiraKey {
		result.Status, result.Reason = "conflict", "Jira counterpart site does not match configured site"
		return result
	}
	out, err = sh(c.Repo, "gh", "pr", "list", "--state", "all", "--limit", "100", "--search",
		fmt.Sprintf("#%d in:body", c.Issue), "--json", "title,body,state,isDraft,baseRefName,url,closingIssuesReferences")
	var prs []linkedPR
	if err != nil || json.Unmarshal([]byte(out), &prs) != nil {
		result.Status, result.Reason = "skipped-unavailable", "GitHub PR evidence unavailable"
		return result
	}
	if len(prs) >= 100 {
		result.Reason = "PR evidence truncated; narrow or page the reconciliation query"
		return result
	}
	result.Target, err = jiraPhase(issue.URL, meta.JiraKey, meta.Base, len(issue.Assignees) > 0, prs, j)
	if err != nil {
		result.Status, result.Reason = "conflict", err.Error()
		return result
	}
	if result.Target == "" {
		result.Reason = "no assignment, non-draft closing PR, or verified merge establishes a lifecycle target"
		return result
	}
	if os.Getenv("JIRA_USER_EMAIL") == "" || os.Getenv("JIRA_API_TOKEN") == "" {
		result.Status, result.Reason = "skipped-unavailable", "JIRA_USER_EMAIL/JIRA_API_TOKEN not configured for standalone reconciliation"
		return result
	}
	base := j.APIBase
	if base == "" {
		base = j.Site
	}
	req, err := http.NewRequestWithContext(runCtx, http.MethodGet,
		strings.TrimRight(base, "/")+"/rest/api/3/issue/"+url.PathEscape(meta.JiraKey)+"?fields=status,description,project", nil)
	if err != nil {
		result.Reason = "invalid configured Jira API endpoint"
		return result
	}
	req.SetBasicAuth(os.Getenv("JIRA_USER_EMAIL"), os.Getenv("JIRA_API_TOKEN"))
	req.Header.Set("Accept", "application/json")
	client := &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return fmt.Errorf("Jira redirects are not followed")
	}}
	resp, err := client.Do(req)
	if err != nil {
		result.Status, result.Reason = "skipped-unavailable", "Jira read failed; check endpoint, network and credentials"
		return result
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		result.Status, result.Reason = "skipped-unavailable", fmt.Sprintf("Jira read returned HTTP %d", resp.StatusCode)
		return result
	}
	var ticket struct {
		Key    string
		Fields struct {
			Status      struct{ Name string }
			Description json.RawMessage
			Project     struct{ Key string }
		}
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&ticket); err != nil || ticket.Key != meta.JiraKey {
		result.Reason = "invalid Jira response or mismatched counterpart"
		return result
	}
	if j.Mode == "service" && ticket.Fields.Project.Key != j.Project {
		result.Status, result.Reason = "conflict", "Jira project does not match service-management scope"
		return result
	}
	if !hasBacklink(ticket.Fields.Description, issue.URL) {
		result.Status, result.Reason = "conflict", "Jira description does not link back to this exact GitHub issue"
		return result
	}
	result.Actual = ticket.Fields.Status.Name
	if strings.EqualFold(strings.TrimSpace(result.Actual), strings.TrimSpace(result.Target)) {
		result.Status, result.Reason = "already aligned", "status verified; assignment/date and Action recovery remain owned by the target integration"
	} else {
		result.Status, result.Reason = "conflict", "status differs; verify the target Action and reconcile there, never move Jira backwards automatically"
	}
	if j.Mode == "service" {
		if result.Status == "already aligned" {
			result.Reason = "status verified without writes; assignment/date are checked during --apply; issues status shows the journal"
		} else {
			result.Reason = "status differs; explicit reconcile --apply or the running service can attempt policy-validated repair"
		}
	}
	return result
}

func hasBacklink(raw json.RawMessage, issueURL string) bool {
	var value any
	if json.Unmarshal(raw, &value) != nil || issueURL == "" {
		return false
	}
	re := regexp.MustCompile(`(?:^|[^A-Za-z0-9_/:.-])` + regexp.QuoteMeta(issueURL) + `(?:[^A-Za-z0-9_/?#-]|$)`)
	var contains func(any) bool
	contains = func(v any) bool {
		switch x := v.(type) {
		case string:
			return re.MatchString(x)
		case []any:
			for _, child := range x {
				if contains(child) {
					return true
				}
			}
		case map[string]any:
			for _, child := range x {
				if contains(child) {
					return true
				}
			}
		}
		return false
	}
	return contains(value)
}
