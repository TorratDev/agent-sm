package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

var localID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)
var jiraKeyPattern = regexp.MustCompile(`^[A-Z][A-Z0-9]*-[1-9][0-9]*$`)
var digestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

type ManagementConfig struct {
	Mode            string            `json:"mode,omitempty"`
	Host            string            `json:"host,omitempty"`
	Repo            string            `json:"repo,omitempty"`
	WritersDisabled bool              `json:"writers_disabled,omitempty"`
	Workflows       []string          `json:"workflows,omitempty"`
	Identities      map[string]string `json:"identities,omitempty"`
	GitHubAssignee  string            `json:"github_assignee,omitempty"`
}

func (m ManagementConfig) validate(j JiraConfig) error {
	if m.Mode == "" {
		if j.Mode == "service" {
			return fmt.Errorf("jira service mode requires management.mode=service")
		}
		return nil
	}
	if m.Mode != "service" || !m.WritersDisabled || j.Mode != "service" {
		return fmt.Errorf("service ownership requires management.mode=service, writers_disabled=true and jira.mode=service")
	}
	if !regexp.MustCompile(`^[a-zA-Z0-9.-]+$`).MatchString(m.Host) || !regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`).MatchString(m.Repo) {
		return fmt.Errorf("management requires an explicit GitHub host and owner/repo")
	}
	for _, w := range m.Workflows {
		if filepath.Base(w) != w || w == "." || strings.TrimSpace(w) == "" {
			return fmt.Errorf("workflow conflicts must be explicit workflow filenames or IDs")
		}
	}
	for login, account := range m.Identities {
		if !regexp.MustCompile(`^[A-Za-z0-9-]+$`).MatchString(login) || strings.TrimSpace(account) == "" {
			return fmt.Errorf("invalid GitHub/Jira identity mapping")
		}
	}
	if m.GitHubAssignee != "" && m.Identities[m.GitHubAssignee] == "" {
		return fmt.Errorf("github_assignee requires an explicit Jira identity mapping")
	}
	return nil
}

type WorkPlan struct {
	ID    string     `json:"id"`
	Items []WorkItem `json:"items"`
}

type WorkItem struct {
	ID                 string   `json:"id"`
	Title              string   `json:"title"`
	Body               string   `json:"body"`
	Kind               string   `json:"kind"`
	Number             int      `json:"number,omitempty"`
	JiraKey            string   `json:"jira_key,omitempty"`
	Parent             string   `json:"parent,omitempty"`
	DependsOn          []string `json:"depends_on,omitempty"`
	RemoveDependencies []string `json:"remove_dependencies,omitempty"`
	Base               string   `json:"base,omitempty"`
	Assignee           string   `json:"assignee,omitempty"`
	GitHubType         string   `json:"github_type,omitempty"`
	RemoveParent       bool     `json:"remove_parent,omitempty"`
	Labels             []string `json:"labels,omitempty"`
	RemoveLabels       []string `json:"remove_labels,omitempty"`
}

func (p WorkPlan) validate() error {
	if !localID.MatchString(p.ID) || len(p.Items) == 0 || len(p.Items) > 100 {
		return fmt.Errorf("plan requires a stable id and 1..100 items")
	}
	if b, err := json.Marshal(p); err != nil || len(b) > 1<<20 {
		return fmt.Errorf("work plan exceeds 1 MiB")
	}
	items, numbers, keys := map[string]WorkItem{}, map[int]bool{}, map[string]bool{}
	for _, i := range p.Items {
		if !localID.MatchString(i.ID) || items[i.ID].ID != "" || strings.TrimSpace(i.Title) == "" || len(i.Title) > 200 || len(i.Body) > 60000 {
			return fmt.Errorf("invalid/duplicate item id, title or body: %q", i.ID)
		}
		if i.Kind != "epic" && i.Kind != "story" && i.Kind != "task" {
			return fmt.Errorf("%s: kind must be epic, story or task", i.ID)
		}
		if i.Number < 0 || (i.Number > 0 && numbers[i.Number]) || (i.JiraKey != "" && (!jiraKeyPattern.MatchString(i.JiraKey) || keys[i.JiraKey])) {
			return fmt.Errorf("%s: invalid/duplicate remote reference", i.ID)
		}
		if i.JiraKey != "" && i.Number == 0 {
			return fmt.Errorf("%s: Jira adoption requires its existing GitHub counterpart", i.ID)
		}
		if i.Base != "" && !validBase(i.Base) {
			return fmt.Errorf("%s: invalid base", i.ID)
		}
		if i.RemoveParent && (i.Parent != "" || i.Kind == "task" || i.Number == 0) {
			return fmt.Errorf("%s: remove_parent requires an existing non-task with no new parent", i.ID)
		}
		labels := map[string]bool{}
		for _, label := range append(append([]string{}, i.Labels...), i.RemoveLabels...) {
			if strings.TrimSpace(label) == "" || len(label) > 50 || labels[label] {
				return fmt.Errorf("%s: empty, duplicate or conflicting label", i.ID)
			}
			labels[label] = true
		}
		if strings.Contains(i.Body, "<!-- agent-sm:") {
			return fmt.Errorf("%s: reserved service marker", i.ID)
		}
		items[i.ID], numbers[i.Number], keys[i.JiraKey] = i, true, true
	}
	for _, i := range p.Items {
		if i.Parent != "" {
			parent, ok := items[i.Parent]
			if !ok || i.Parent == i.ID || i.Kind == "epic" || parent.Kind == "task" || (i.Kind == "task" && parent.Kind != "story") {
				return fmt.Errorf("%s: invalid hierarchy", i.ID)
			}
		} else if i.Kind == "task" {
			return fmt.Errorf("%s: a task requires a Story parent", i.ID)
		}
		seen := map[string]bool{}
		for _, ref := range append(append([]string{}, i.DependsOn...), i.RemoveDependencies...) {
			if items[ref].ID == "" || ref == i.ID || seen[ref] {
				return fmt.Errorf("%s: missing/self/duplicate dependency %q", i.ID, ref)
			}
			seen[ref] = true
		}
	}
	for _, hierarchy := range []bool{false, true} {
		visiting, done := map[string]bool{}, map[string]bool{}
		var visit func(string) error
		visit = func(id string) error {
			if visiting[id] {
				return fmt.Errorf("cycle involving %s", id)
			}
			if done[id] {
				return nil
			}
			visiting[id] = true
			refs := items[id].DependsOn
			if hierarchy {
				refs = nil
				if items[id].Parent != "" {
					refs = []string{items[id].Parent}
				}
			}
			for _, ref := range refs {
				if err := visit(ref); err != nil {
					return err
				}
			}
			visiting[id], done[id] = false, true
			return nil
		}
		for id := range items {
			if err := visit(id); err != nil {
				return err
			}
		}
	}
	return nil
}

type issueSnapshot struct {
	ID          int64                    `json:"id"`
	Number      int                      `json:"number"`
	URL         string                   `json:"html_url"`
	Title       string                   `json:"title"`
	Body        string                   `json:"body"`
	State       string                   `json:"state"`
	Updated     string                   `json:"updated_at"`
	Assignees   []struct{ Login string } `json:"assignees"`
	Type        *struct{ Name string }   `json:"type"`
	Labels      []struct{ Name string }  `json:"labels"`
	PullRequest json.RawMessage          `json:"pull_request,omitempty"`
}

type jiraSnapshot struct {
	ID     string `json:"id"`
	Key    string `json:"key"`
	Fields struct {
		Summary     string               `json:"summary"`
		Description json.RawMessage      `json:"description"`
		Updated     string               `json:"updated"`
		Project     struct{ Key string } `json:"project"`
		IssueType   struct {
			ID      string `json:"id"`
			Name    string `json:"name"`
			Subtask bool   `json:"subtask"`
		} `json:"issuetype"`
		Parent   *struct{ Key string } `json:"parent"`
		Status   struct{ Name string } `json:"status"`
		Assignee *struct {
			AccountID string `json:"accountId"`
		} `json:"assignee"`
		Links []jiraLink `json:"issuelinks"`
	} `json:"fields"`
}

type jiraLink struct {
	ID           string                                     `json:"id"`
	Type         struct{ ID, Name, Inward, Outward string } `json:"type"`
	InwardIssue  *struct{ Key string }                      `json:"inwardIssue"`
	OutwardIssue *struct{ Key string }                      `json:"outwardIssue"`
}

type expectedItem struct {
	GitHub *issueSnapshot  `json:"github,omitempty"`
	Jira   *jiraSnapshot   `json:"jira,omitempty"`
	Parent []issueSnapshot `json:"parent,omitempty"`
	Deps   []issueSnapshot `json:"dependencies,omitempty"`
}

type Preview struct {
	Digest     string                  `json:"digest"`
	Scope      string                  `json:"scope"`
	Policy     string                  `json:"policy"`
	Plan       WorkPlan                `json:"plan"`
	Expected   map[string]expectedItem `json:"expected"`
	Operations []string                `json:"operations"`
	Started    bool                    `json:"started,omitempty"`
	Complete   bool                    `json:"complete,omitempty"`
	Cancelled  bool                    `json:"cancelled,omitempty"`
	Error      string                  `json:"error,omitempty"`
	Attempts   int                     `json:"attempts,omitempty"`
	Next       time.Time               `json:"next,omitempty"`
}

type managementOp struct {
	State  string          `json:"state"`
	Result json.RawMessage `json:"result,omitempty"`
}

type ManagedItem struct {
	Number           int       `json:"number"`
	JiraKey          string    `json:"jira_key"`
	Base             string    `json:"base"`
	Active           bool      `json:"active"`
	PRs              []int     `json:"prs,omitempty"`
	Status           string    `json:"status"`
	Error            string    `json:"error,omitempty"`
	Attempts         int       `json:"attempts,omitempty"`
	Next             time.Time `json:"next,omitempty"`
	Progress         string    `json:"progress,omitempty"`
	ProgressHash     string    `json:"progress_hash,omitempty"`
	AssignmentPaused bool      `json:"assignment_paused,omitempty"`
	Delivered        bool      `json:"delivered,omitempty"`
}

type managementState struct {
	Scope    string                   `json:"scope"`
	Previews map[string]*Preview      `json:"previews"`
	Items    map[string]*ManagedItem  `json:"items"`
	Ops      map[string]*managementOp `json:"operations"`
	Error    string                   `json:"error,omitempty"`
}

type trackerAPI interface {
	call(context.Context, string, string, string, any, any) error
}

type manager struct {
	repo, dir, scope string
	profile          Profile
	api              trackerAPI
	state            managementState
}

func contentDigest(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("non-JSON management state: %v", err))
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var canonical any
	if err := d.Decode(&canonical); err != nil {
		panic(err)
	}
	b, err = json.Marshal(canonical)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func newManager(ctx context.Context, repo string, p Profile) (*manager, error) {
	if err := p.Management.validate(p.Jira); err != nil {
		return nil, err
	}
	if p.Management.Mode != "service" {
		return nil, fmt.Errorf("service management is not enabled")
	}
	u, err := url.Parse("https://" + p.Management.Host + "/" + p.Management.Repo)
	if err != nil {
		return nil, err
	}
	readCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := process(readCtx, repo, "gh", "repo", "view", "--json", "url", "--jq", ".url")
	cmd.Env = serviceEnv()
	out, err := cmd.Output()
	if err != nil || strings.TrimRight(strings.TrimSpace(string(out)), "/") != u.String() {
		return nil, fmt.Errorf("checkout GitHub identity does not match configured management scope")
	}
	scope := u.String() + "|" + p.Jira.Site + "|" + p.Jira.Project
	m := &manager{repo: repo, scope: scope, profile: p,
		dir: filepath.Join(scratchRoot(), "management", contentDigest(u.String())),
		api: &remoteTrackers{repo: repo, profile: p}}
	return m, nil
}

func (m *manager) locked(ctx context.Context, f func() error) error {
	if err := os.MkdirAll(m.dir, 0700); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(m.dir, "lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	for {
		if err = lockFile(lock); err == nil {
			break
		}
		if !isLockHeld(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	defer unlockFile(lock)
	m.state = managementState{Scope: m.scope, Previews: map[string]*Preview{}, Items: map[string]*ManagedItem{}, Ops: map[string]*managementOp{}}
	b, err := os.ReadFile(filepath.Join(m.dir, "state.json"))
	if err == nil {
		if err = decodeStrict(b, &m.state); err != nil {
			return fmt.Errorf("invalid management journal: %w", err)
		}
		if m.state.Scope != m.scope || m.state.Previews == nil || m.state.Items == nil || m.state.Ops == nil {
			return fmt.Errorf("incomplete management journal")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	return f()
}

func (m *manager) save() error {
	// shortcut: rewrite the complete journal; compact history when management state becomes large.
	b, err := json.MarshalIndent(m.state, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(m.dir, ".state-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(f.Name(), filepath.Join(m.dir, "state.json")); err != nil {
		return err
	}
	return fsyncDir(m.dir)
}

func (m *manager) op(ctx context.Context, id string, create bool, run func(bool) (any, error)) error {
	old := m.state.Ops[id]
	if old != nil && old.State == "done" {
		return nil
	}
	attempted := old != nil
	m.state.Ops[id] = &managementOp{State: "pending"}
	if err := m.save(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	result, err := run(create && attempted)
	if err != nil {
		m.state.Error = id + ": " + err.Error()
		if saveErr := m.save(); saveErr != nil {
			return fmt.Errorf("%v; persist failure: %w", err, saveErr)
		}
		return fmt.Errorf("%s: %w", id, err)
	}
	b, err := json.Marshal(result)
	if err != nil {
		return err
	}
	m.state.Ops[id] = &managementOp{State: "done", Result: b}
	return m.save()
}

func (m *manager) endpoint(n int, suffix string) string {
	return fmt.Sprintf("/repos/%s/issues/%d%s", m.profile.Management.Repo, n, suffix)
}

func (m *manager) ghIssue(ctx context.Context, n int) (*issueSnapshot, error) {
	var s issueSnapshot
	err := m.api.call(ctx, "github", "GET", m.endpoint(n, ""), nil, &s)
	if err == nil && (s.Number != n || s.ID <= 0 || len(s.PullRequest) > 0 || (s.State != "open" && s.State != "closed") || s.URL != "https://"+m.profile.Management.Host+"/"+m.profile.Management.Repo+"/issues/"+strconv.Itoa(n)) {
		err = fmt.Errorf("GitHub issue identity mismatch")
	}
	return &s, err
}

func (m *manager) jiraIssue(ctx context.Context, key string) (*jiraSnapshot, error) {
	if !jiraKeyPattern.MatchString(key) {
		return nil, fmt.Errorf("invalid Jira key")
	}
	var s jiraSnapshot
	err := m.api.call(ctx, "jira", "GET", "/issue/"+key, nil, &s)
	if err == nil && (s.Key != key || s.Fields.Project.Key != m.profile.Jira.Project) {
		err = fmt.Errorf("Jira issue identity/project mismatch")
	}
	return &s, err
}

func (m *manager) policy() string {
	return contentDigest(struct {
		Management ManagementConfig
		Jira       JiraConfig
		Base       string
	}{m.profile.Management, m.profile.Jira, m.profile.Base})
}

func orderedItems(p WorkPlan) []WorkItem {
	items := append([]WorkItem{}, p.Items...)
	depth := func(i WorkItem) int {
		n := 0
		for i.Parent != "" {
			for _, parent := range p.Items {
				if parent.ID == i.Parent {
					i = parent
					break
				}
			}
			n++
		}
		return n
	}
	sort.SliceStable(items, func(a, b int) bool { return depth(items[a]) < depth(items[b]) })
	return items
}

func serviceEnv() []string {
	var env []string
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if key != "GH_REPO" && key != "GH_HOST" {
			env = append(env, entry)
		}
	}
	return env
}

func (m *manager) cancel(ctx context.Context, digest string) error {
	if !digestPattern.MatchString(digest) {
		return fmt.Errorf("cancel requires an exact preview digest")
	}
	return m.locked(ctx, func() error {
		p := m.state.Previews[digest]
		if p == nil || p.Complete {
			return fmt.Errorf("no incomplete batch with that digest")
		}
		p.Cancelled = true
		return m.save()
	})
}
