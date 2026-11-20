package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

type remoteTrackers struct {
	repo    string
	profile Profile
	mu      sync.Mutex
	cache   map[string]cachedTrackerRead
}

type cachedTrackerRead struct {
	etag string
	body []byte
}

type retryError struct {
	status int
	after  time.Duration
}

func (e retryError) Error() string {
	return fmt.Sprintf("tracker HTTP %d; retry after %s", e.status, e.after)
}

func retryDelay(header string) time.Duration {
	if n, err := strconv.Atoi(header); err == nil && n > 0 {
		return min(time.Duration(n), 86400) * time.Second
	}
	if t, err := http.ParseTime(header); err == nil {
		return max(time.Duration(0), min(time.Until(t), 24*time.Hour))
	}
	return time.Minute
}

func (t *remoteTrackers) call(ctx context.Context, system, method, path string, input, output any) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cacheKey := system + path
	t.mu.Lock()
	cached := t.cache[cacheKey]
	if method != "GET" {
		t.cache = nil
	}
	t.mu.Unlock()
	decode := func(status int, headers http.Header, b []byte) error {
		if status == http.StatusNotModified {
			if cached.etag == "" {
				return fmt.Errorf("unexpected tracker 304 without cached evidence")
			}
			b = cached.body
		} else if status < 200 || status >= 300 {
			if status == 429 || (status == 403 && headers.Get("Retry-After") != "") {
				return retryError{status, retryDelay(headers.Get("Retry-After"))}
			}
			return trackerError{status}
		}
		if method == "GET" && headers.Get("ETag") != "" && status != 304 {
			t.mu.Lock()
			if t.cache == nil {
				t.cache = map[string]cachedTrackerRead{}
			}
			t.cache[cacheKey] = cachedTrackerRead{headers.Get("ETag"), b}
			t.mu.Unlock()
		}
		if output != nil {
			if err := json.Unmarshal(b, output); err != nil {
				return fmt.Errorf("invalid %s response: %w", system, err)
			}
		}
		return nil
	}
	var body []byte
	var err error
	if input != nil {
		body, err = json.Marshal(input)
		if err != nil {
			return err
		}
	}
	if system == "github" {
		args := []string{"api", "--include", "--hostname", t.profile.Management.Host, "--method", method, path}
		if method == "GET" && cached.etag != "" {
			args = append(args, "--header", "If-None-Match: "+cached.etag)
		}
		if input != nil {
			args = append(args, "--input", "-")
		}
		cmd := process(ctx, t.repo, "gh", args...)
		cmd.Env = serviceEnv()
		cmd.Stdin = bytes.NewReader(body)
		stdout, stderr := &boundedOutput{limit: 8 << 20}, &boundedOutput{limit: 64 << 10}
		cmd.Stdout, cmd.Stderr = stdout, stderr
		runErr := cmd.Run()
		response, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(stdout.Bytes())), nil)
		if err != nil {
			return fmt.Errorf("GitHub %s failed or returned invalid headers (%v); check gh permissions/network", method, runErr)
		}
		defer response.Body.Close()
		b, err := io.ReadAll(io.LimitReader(response.Body, (8<<20)+1))
		if err != nil || len(b) > 8<<20 {
			return fmt.Errorf("incomplete/oversized GitHub response")
		}
		if runErr != nil && response.StatusCode >= 200 && response.StatusCode < 300 {
			return fmt.Errorf("GitHub command failed after response: %w", runErr)
		}
		return decode(response.StatusCode, response.Header, b)
	}
	if system != "jira" {
		return fmt.Errorf("unknown tracker")
	}
	if os.Getenv("JIRA_USER_EMAIL") == "" || os.Getenv("JIRA_API_TOKEN") == "" {
		return fmt.Errorf("JIRA_USER_EMAIL/JIRA_API_TOKEN are required")
	}
	base := t.profile.Jira.APIBase
	if base == "" {
		base = t.profile.Jira.Site
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(base, "/")+"/rest/api/3"+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.SetBasicAuth(os.Getenv("JIRA_USER_EMAIL"), os.Getenv("JIRA_API_TOKEN"))
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	if method == "GET" && cached.etag != "" {
		req.Header.Set("If-None-Match", cached.etag)
	}
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return fmt.Errorf("Jira redirects are not followed")
	}}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("Jira %s failed; verify network/endpoint/credentials", method)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, (8<<20)+1))
	if err != nil || len(b) > 8<<20 {
		return fmt.Errorf("incomplete/oversized Jira response")
	}
	return decode(resp.StatusCode, resp.Header, b)
}

func ghPages[T any](ctx context.Context, m *manager, path string) ([]T, error) {
	var result []T
	totalBytes := 0
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	for page := 1; page <= 1000; page++ {
		var raw json.RawMessage
		if err := m.api.call(ctx, "github", "GET", path+sep+"per_page=100&page="+strconv.Itoa(page), nil, &raw); err != nil {
			return nil, err
		}
		totalBytes += len(raw)
		if totalBytes > 16<<20 {
			return nil, fmt.Errorf("GitHub collection exceeds 16 MiB; evidence incomplete")
		}
		var values []T
		if !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("[")) || json.Unmarshal(raw, &values) != nil {
			return nil, fmt.Errorf("invalid GitHub collection; evidence unavailable")
		}
		result = append(result, values...)
		if len(values) < 100 {
			return result, nil
		}
	}
	return nil, fmt.Errorf("GitHub pagination limit reached; evidence incomplete")
}

type jiraField struct {
	FieldID    string `json:"fieldId"`
	Key        string `json:"key"`
	Name       string `json:"name"`
	Required   bool   `json:"required"`
	HasDefault bool   `json:"hasDefaultValue"`
}

func jiraPages[T any](ctx context.Context, m *manager, path, field string) ([]T, error) {
	var result []T
	for start := 0; start < 100000; {
		var page map[string]json.RawMessage
		if err := m.api.call(ctx, "jira", "GET", path+"?maxResults=100&startAt="+strconv.Itoa(start), nil, &page); err != nil {
			return nil, err
		}
		raw := page[field]
		if raw == nil {
			raw = page["values"]
		}
		var values []T
		if !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("[")) || json.Unmarshal(raw, &values) != nil {
			return nil, fmt.Errorf("invalid Jira metadata page")
		}
		result = append(result, values...)
		var total int
		var last bool
		if raw := page["total"]; raw != nil {
			if json.Unmarshal(raw, &total) != nil || total < 0 {
				return nil, fmt.Errorf("invalid Jira pagination total")
			}
		}
		if raw := page["isLast"]; raw != nil && json.Unmarshal(raw, &last) != nil {
			return nil, fmt.Errorf("invalid Jira pagination state")
		}
		start += len(values)
		if (page["total"] != nil && start >= total) || last || (page["total"] == nil && len(values) < 100) {
			return result, nil
		}
		if len(values) == 0 {
			return nil, fmt.Errorf("incomplete Jira metadata")
		}
	}
	return nil, fmt.Errorf("Jira pagination limit reached")
}

func (m *manager) typeName(i WorkItem) string {
	switch i.Kind {
	case "epic":
		return m.profile.Jira.EpicType
	case "story":
		return m.profile.Jira.StoryType
	default:
		return m.profile.Jira.TaskType
	}
}

type jiraType struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Subtask bool   `json:"subtask"`
}

func (m *manager) preflight(ctx context.Context, plan WorkPlan) (map[string]string, error) {
	var repo struct {
		HasIssues   bool                                         `json:"has_issues"`
		Permissions struct{ Admin, Push, Triage, Maintain bool } `json:"permissions"`
	}
	if err := m.api.call(ctx, "github", "GET", "/repos/"+m.profile.Management.Repo, nil, &repo); err != nil {
		return nil, err
	}
	if !repo.HasIssues || !(repo.Permissions.Admin || repo.Permissions.Push || repo.Permissions.Triage || repo.Permissions.Maintain) {
		return nil, fmt.Errorf("GitHub issues and a write-capable service identity are required")
	}
	for _, w := range m.profile.Management.Workflows {
		var workflow struct{ State string }
		if err := m.api.call(ctx, "github", "GET", "/repos/"+m.profile.Management.Repo+"/actions/workflows/"+url.PathEscape(w), nil, &workflow); err != nil {
			return nil, err
		}
		if !strings.HasPrefix(workflow.State, "disabled_") {
			return nil, fmt.Errorf("competing workflow %s must be disabled", w)
		}
	}
	permissions := "BROWSE_PROJECTS,CREATE_ISSUES,EDIT_ISSUES,ASSIGN_ISSUES,TRANSITION_ISSUES,ADD_COMMENTS,LINK_ISSUES"
	var perms struct {
		Permissions map[string]struct {
			Have bool `json:"havePermission"`
		} `json:"permissions"`
	}
	if err := m.api.call(ctx, "jira", "GET", "/mypermissions?projectKey="+m.profile.Jira.Project+"&permissions="+permissions, nil, &perms); err != nil {
		return nil, err
	}
	for _, name := range strings.Split(permissions, ",") {
		if !perms.Permissions[name].Have {
			return nil, fmt.Errorf("missing Jira permission %s", name)
		}
	}
	types, err := jiraPages[jiraType](ctx, m, "/issue/createmeta/"+m.profile.Jira.Project+"/issuetypes", "issueTypes")
	if err != nil {
		return nil, err
	}
	ids := map[string]string{}
	for _, t := range types {
		if t.Name == m.profile.Jira.TaskType && !t.Subtask {
			return nil, fmt.Errorf("configured task_type must be a Jira Sub-task type")
		}
		if (t.Name == m.profile.Jira.StoryType || t.Name == m.profile.Jira.EpicType) && t.Subtask {
			return nil, fmt.Errorf("configured Story/Epic cannot be a subtask type")
		}
		if ids[t.Name] != "" {
			return nil, fmt.Errorf("ambiguous Jira type %q", t.Name)
		}
		ids[t.Name] = t.ID
	}
	if m.profile.Jira.Epic != "" {
		epic, err := m.jiraIssue(ctx, m.profile.Jira.Epic)
		if err != nil {
			return nil, err
		}
		if epic.Fields.IssueType.Name != m.profile.Jira.EpicType {
			return nil, fmt.Errorf("configured Epic has the wrong type")
		}
	}
	checkedTypes, checkedPeople := map[string][]jiraField{}, map[string]bool{}
	for _, i := range plan.Items {
		name := m.typeName(i)
		if ids[name] == "" {
			return nil, fmt.Errorf("Jira type %q is unavailable", name)
		}
		fields, checked := checkedTypes[name]
		if !checked {
			fields, err := jiraPages[jiraField](ctx, m, "/issue/createmeta/"+m.profile.Jira.Project+"/issuetypes/"+ids[name], "fields")
			if err != nil {
				return nil, err
			}
			checkedTypes[name] = fields
		}
		fields = checkedTypes[name]
		if len(fields) == 0 {
			return nil, fmt.Errorf("empty Jira create metadata for %s", name)
		}
		creating := i.Number == 0
		if i.Number > 0 && i.JiraKey == "" {
			gh, err := m.ghIssue(ctx, i.Number)
			if err != nil {
				return nil, err
			}
			meta, err := issueMetadata(gh.Body)
			if err != nil {
				return nil, err
			}
			creating = meta.JiraKey == ""
		}
		if creating {
			hasParent := i.Kind == "task" || (i.Kind == "story" && m.profile.Jira.Epic != "")
			for parent := i.Parent; parent != ""; {
				for _, candidate := range plan.Items {
					if candidate.ID == parent {
						hasParent = hasParent || candidate.Kind == "epic"
						parent = candidate.Parent
						break
					}
				}
			}
			provided := map[string]bool{"summary": true, "description": true, "project": true, "issuetype": true, "parent": hasParent, "assignee": i.Assignee != ""}
			available := map[string]bool{}
			for _, f := range fields {
				key := f.FieldID
				if key == "" {
					key = f.Key
				}
				available[key] = true
				if f.Required && !f.HasDefault && !provided[key] && !hasCreateValue(m.profile.Jira.CreateFields[name][key]) {
					return nil, fmt.Errorf("Jira type %s requires %s; supply its explicit parent/assignee or create_fields.%s.%s", name, key, name, key)
				}
			}
			for field := range m.profile.Jira.CreateFields[name] {
				if !available[field] {
					return nil, fmt.Errorf("configured create field %s is unavailable for %s", field, name)
				}
			}
		}
		for _, label := range i.Labels {
			if err := m.api.call(ctx, "github", "GET", "/repos/"+m.profile.Management.Repo+"/labels/"+url.PathEscape(label), nil, nil); err != nil {
				return nil, fmt.Errorf("GitHub label %q is unavailable: %w", label, err)
			}
		}
		if len(i.DependsOn)+len(i.RemoveDependencies) > 0 {
			if _, err := m.blocksType(ctx); err != nil {
				return nil, err
			}
		}
		login := i.Assignee
		if login != "" && !checkedPeople[login] {
			if err := m.checkIdentity(ctx, login); err != nil {
				return nil, err
			}
			checkedPeople[login] = true
		}
		if i.GitHubType != "" {
			var types []struct{ Name string }
			if err := m.api.call(ctx, "github", "GET", "/repos/"+m.profile.Management.Repo+"/issue-types", nil, &types); err != nil {
				return nil, err
			}
			found := false
			for _, t := range types {
				found = found || t.Name == i.GitHubType
			}
			if !found {
				return nil, fmt.Errorf("unavailable GitHub issue type %q", i.GitHubType)
			}
		}
	}
	return ids, nil
}

func hasCreateValue(raw json.RawMessage) bool {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return false
	}
	switch v := value.(type) {
	case nil:
		return false
	case string:
		return strings.TrimSpace(v) != ""
	case []any:
		return len(v) > 0
	case map[string]any:
		return len(v) > 0
	}
	return true
}

func (m *manager) checkIdentity(ctx context.Context, login string) error {
	account := m.profile.Management.Identities[login]
	if account == "" {
		return fmt.Errorf("no approved Jira identity for %s", login)
	}
	if err := m.api.call(ctx, "github", "GET", "/repos/"+m.profile.Management.Repo+"/assignees/"+url.PathEscape(login), nil, nil); err != nil {
		return err
	}
	var users []struct {
		AccountID string `json:"accountId"`
		Active    bool   `json:"active"`
	}
	if err := m.api.call(ctx, "jira", "GET", "/user/assignable/search?project="+m.profile.Jira.Project+"&accountId="+url.QueryEscape(account), nil, &users); err != nil {
		return err
	}
	if len(users) != 1 || users[0].AccountID != account || !users[0].Active {
		return fmt.Errorf("mapped Jira account is not uniquely active/assignable")
	}
	return nil
}

func (m *manager) jiraEdit(ctx context.Context, key string, fields map[string]any) error {
	var meta struct {
		Fields map[string]struct {
			Operations []string
			Schema     struct{ Type string } `json:"schema"`
		} `json:"fields"`
	}
	if err := m.api.call(ctx, "jira", "GET", "/issue/"+key+"/editmeta", nil, &meta); err != nil {
		return err
	}
	for name := range fields {
		found := false
		for _, op := range meta.Fields[name].Operations {
			found = found || op == "set"
		}
		if !found {
			return fmt.Errorf("Jira field %s is not editable on %s", name, key)
		}
		if name == m.profile.Jira.StartDateField && meta.Fields[name].Schema.Type != "date" {
			return fmt.Errorf("configured Start-date field is not a Jira date field")
		}
	}
	if field := m.profile.Jira.StartDateField; fields[field] != nil {
		var current struct{ Fields map[string]json.RawMessage }
		if err := m.api.call(ctx, "jira", "GET", "/issue/"+key+"?fields="+field, nil, &current); err != nil {
			return err
		}
		raw, ok := current.Fields[field]
		if !ok {
			return fmt.Errorf("Start-date field unavailable")
		}
		if string(raw) != "null" && string(raw) != `""` {
			delete(fields, field)
		}
	}
	if len(fields) == 0 {
		return nil
	}
	if err := m.api.call(ctx, "jira", "PUT", "/issue/"+key, map[string]any{"fields": fields}, nil); err != nil {
		return err
	}
	var actual struct {
		Fields map[string]json.RawMessage `json:"fields"`
	}
	if err := m.api.call(ctx, "jira", "GET", "/issue/"+key, nil, &actual); err != nil {
		return err
	}
	for name, desired := range fields {
		var observed any
		if json.Unmarshal(actual.Fields[name], &observed) != nil {
			return fmt.Errorf("Jira field %s missing from readback", name)
		}
		switch name {
		case "assignee":
			var person struct {
				AccountID string `json:"accountId"`
			}
			json.Unmarshal(actual.Fields[name], &person)
			b, _ := json.Marshal(desired)
			var wanted struct {
				AccountID string `json:"accountId"`
			}
			json.Unmarshal(b, &wanted)
			if person.AccountID != wanted.AccountID {
				return fmt.Errorf("Jira assignee readback mismatch")
			}
		case "parent", "issuetype":
			if desired == nil {
				if observed != nil {
					return fmt.Errorf("Jira %s removal readback mismatch", name)
				}
				continue
			}
			b, _ := json.Marshal(desired)
			var wanted map[string]any
			json.Unmarshal(b, &wanted)
			got, ok := observed.(map[string]any)
			if !ok {
				return fmt.Errorf("invalid Jira %s readback", name)
			}
			for k, v := range wanted {
				if got[k] != v {
					return fmt.Errorf("Jira %s readback mismatch", name)
				}
			}
		default:
			if contentDigest(observed) != contentDigest(desired) {
				return fmt.Errorf("Jira field %s readback mismatch", name)
			}
		}
	}
	return nil
}
