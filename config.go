package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Profile is trusted operator configuration, never discovered in a target checkout.
type Profile struct {
	Provider       string              `json:"provider"`
	Models         map[string]string   `json:"models,omitempty"`
	Checks         []string            `json:"checks"`
	BaseChecks     []string            `json:"base_checks,omitempty"`           // what prepare runs on the untouched base; empty means checks
	TestReports    []string            `json:"required_test_reports,omitempty"` // TRX globs under fresh SM_CHECK_RESULTS
	Format         string              `json:"format,omitempty"`
	SessionLimit   float64             `json:"session_limit"`
	RunLimit       float64             `json:"run_limit"`
	TimeoutMinutes float64             `json:"timeout_minutes"`
	Commands       map[string][]string `json:"commands,omitempty"`
	NotesDir       string              `json:"notes_dir,omitempty"`
	AddDirs        []string            `json:"add_dirs,omitempty"`
	Links          []string            `json:"links,omitempty"` // gitignored repo files symlinked into each worktree, e.g. a ROM dump
	Legacy         bool                `json:"legacy_rust_ghidra,omitempty"`
	Base           string              `json:"base"`
	Jira           JiraConfig          `json:"jira,omitempty"`
	Management     ManagementConfig    `json:"management,omitempty"`
	Transcripts    bool                `json:"transcripts"`
	Concurrency    int                 `json:"concurrency,omitempty"` // queued issues `serve` runs at once; 0 or 1 is one at a time
}

var settings = defaultProfile("pi")
var managementCommand bool

const maxConcurrency = 16

func finiteNonnegative(n float64) bool {
	return !math.IsNaN(n) && !math.IsInf(n, 0) && n >= 0
}

func defaultProfile(provider string) Profile {
	return Profile{Provider: provider, SessionLimit: 2, RunLimit: 20, TimeoutMinutes: 60, Base: "origin/main",
		Models: map[string]string{}, Commands: map[string][]string{},
		Transcripts: true} // on until runs stop erroring; turn off with "transcripts": false or SM_TRANSCRIPTS=0
}

func decodeStrict(b []byte, out any) error {
	unique := json.NewDecoder(bytes.NewReader(b))
	if _, err := uniqueJSON(unique); err != nil {
		return err
	}
	if _, err := unique.Token(); err != io.EOF {
		return fmt.Errorf("expected exactly one JSON value")
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("expected exactly one JSON value")
	}
	return nil
}

func decodeProfile(b []byte, provider string) (Profile, error) {
	var header struct {
		Provider string `json:"provider"`
	}
	if err := json.Unmarshal(b, &header); err != nil {
		return Profile{}, err
	}
	if provider == "" {
		provider = header.Provider
	}
	if provider == "" {
		provider = "pi"
	}
	p := defaultProfile(provider)
	err := decodeStrict(b, &p)
	return p, err
}

func (p *Profile) applyLimitEnv() error {
	if v, ok := os.LookupEnv("SM_CONCURRENCY"); ok {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("SM_CONCURRENCY must be an integer")
		}
		p.Concurrency = n
	}
	for _, entry := range []struct {
		name string
		dst  *float64
	}{{"SM_SESSION_LIMIT", &p.SessionLimit}, {"SM_RUN_LIMIT", &p.RunLimit}} {
		if v, ok := os.LookupEnv(entry.name); ok {
			n, err := strconv.ParseFloat(v, 64)
			if err != nil {
				return fmt.Errorf("%s must be numeric", entry.name)
			}
			*entry.dst = n
		}
	}
	for _, entry := range []struct {
		name string
		dst  *float64
	}{{"SM_BUDGET", &p.SessionLimit}, {"SM_RUN_BUDGET", &p.RunLimit}} {
		if v, ok := os.LookupEnv(entry.name); ok {
			n, err := strconv.ParseFloat(v, 64)
			if err != nil {
				return fmt.Errorf("%s must be numeric", entry.name)
			}
			*entry.dst = n
		}
	}
	return nil
}

func loadProfile(path string) (Profile, error) {
	p := defaultProfile("pi")
	if snapshot := os.Getenv("SM_RESOLVED_CONFIG"); snapshot != "" {
		var err error
		p, err = decodeProfile([]byte(snapshot), "")
		if err != nil {
			return p, fmt.Errorf("resolved configuration: %w", err)
		}
		return p, p.validateFor(managementCommand)
	}
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return p, err
		}
		p, err = decodeProfile(b, os.Getenv("SM_PROVIDER"))
		if err != nil {
			return p, fmt.Errorf("profile: %w", err)
		}
		abs, err := filepath.Abs(path)
		if err != nil {
			return p, err
		}
		for i, d := range p.AddDirs {
			p.AddDirs[i] = profilePath(filepath.Dir(abs), d)
		}
		if p.NotesDir != "" {
			p.NotesDir = profilePath(filepath.Dir(abs), p.NotesDir)
		}
	} else if v := os.Getenv("SM_PROVIDER"); v != "" {
		p = defaultProfile(v)
	}
	if v := os.Getenv("SM_PROVIDER"); v != "" {
		p.Provider = v
	}
	if v, ok := os.LookupEnv("SM_TIMEOUT"); ok {
		n, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return p, fmt.Errorf("SM_TIMEOUT must be numeric")
		}
		p.TimeoutMinutes = n
	}
	if err := p.applyLimitEnv(); err != nil {
		return p, err
	}
	if err := p.applyTranscriptEnv(); err != nil {
		return p, err
	}
	if v, ok := os.LookupEnv("SM_CHECKS"); ok {
		p.Checks = strings.Split(v, "\n")
		if _, explicit := os.LookupEnv("SM_FMT"); !explicit {
			p.Format = ""
		}
	}
	if v, ok := os.LookupEnv("SM_FMT"); ok {
		p.Format = v
	}
	if v := os.Getenv("SM_BASE"); v != "" {
		p.Base = v
	}
	if v := os.Getenv("SM_NOTES_DIR"); v != "" {
		var err error
		p.NotesDir, err = filepath.Abs(v)
		if err != nil {
			return p, err
		}
	}
	if v := os.Getenv("SM_ADD_DIRS"); v != "" {
		for _, d := range strings.Split(v, ",") {
			abs, err := filepath.Abs(strings.TrimSpace(d))
			if err != nil {
				return p, err
			}
			p.AddDirs = append(p.AddDirs, abs)
		}
	}
	if p.Models == nil {
		p.Models = map[string]string{}
	}
	for _, st := range steps {
		if v := os.Getenv("SM_MODEL_" + strings.ToUpper(st.Name)); st.Role != "" && v != "" {
			p.Models[st.Name] = v
		}
	}
	if err := p.validateFor(managementCommand); err != nil {
		return p, err
	}
	return p, p.validateFor(managementCommand)
}

func (p *Profile) applyTranscriptEnv() error {
	if v, ok := os.LookupEnv("SM_TRANSCRIPTS"); ok {
		enabled, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("SM_TRANSCRIPTS must be a boolean")
		}
		p.Transcripts = enabled
	}
	return nil
}

func profilePath(base, p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(base, p)
}

func agentState(s string) bool {
	for _, st := range steps {
		if st.Name == s && st.Role != "" {
			return true
		}
	}
	return false
}

func (p Profile) validate() error {
	return p.validateFor(false)
}

func (p Profile) validateFor(managementOnly bool) error {
	if p.Provider != "pi" {
		return fmt.Errorf("provider must be pi")
	}
	for _, n := range []float64{p.SessionLimit, p.RunLimit, p.TimeoutMinutes} {
		if !finiteNonnegative(n) {
			return fmt.Errorf("limits and timeout must be finite and nonnegative")
		}
	}
	if p.Concurrency < 0 || p.Concurrency > maxConcurrency {
		return fmt.Errorf("concurrency must be between 0 and %d", maxConcurrency)
	}
	if p.SessionLimit == 0 || p.TimeoutMinutes == 0 {
		return fmt.Errorf("session_limit and timeout_minutes must be positive")
	}
	if p.TimeoutMinutes > float64(1<<63-1)/float64(time.Minute) {
		return fmt.Errorf("timeout_minutes exceeds the supported duration")
	}
	if p.NotesDir != "" {
		st, err := os.Stat(p.NotesDir)
		if err != nil {
			return fmt.Errorf("notes_dir: %w", err)
		}
		if !st.IsDir() {
			return fmt.Errorf("notes_dir must be a directory")
		}
	}
	for _, path := range p.AddDirs {
		st, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("add_dirs: %w", err)
		}
		if !st.IsDir() {
			return fmt.Errorf("add_dirs entries must be directories")
		}
	}
	for _, f := range p.Links {
		// IsLocal, not IsAbs: on Windows "/etc/passwd" and "C:x" are not absolute but still leave the repo.
		if f = filepath.Clean(f); f == "." || !filepath.IsLocal(f) {
			return fmt.Errorf("links must be paths inside the repository")
		}
	}
	if len(p.Checks) == 0 && !managementOnly {
		return fmt.Errorf("configure at least one check with --config or SM_CHECKS")
	}
	for _, cmd := range append(append([]string{}, p.Checks...), p.BaseChecks...) {
		if strings.TrimSpace(cmd) == "" {
			return fmt.Errorf("check commands must not be empty")
		}
	}
	for _, pattern := range p.TestReports {
		if !filepath.IsLocal(pattern) || strings.Contains(pattern, "..") || !strings.HasSuffix(pattern, ".trx") {
			return fmt.Errorf("required_test_reports must be relative TRX globs under SM_CHECK_RESULTS")
		}
		if _, err := filepath.Match(pattern, ""); err != nil {
			return fmt.Errorf("invalid test-report glob: %w", err)
		}
	}
	for state, model := range p.Models {
		if !agentState(state) || !modelRe.MatchString(model) {
			return fmt.Errorf("invalid model for %q", state)
		}
	}
	for state, commands := range p.Commands {
		if !agentState(state) {
			return fmt.Errorf("invalid command role %q", state)
		}
		for _, cmd := range commands {
			if !commandPrefix.MatchString(cmd) || neverSuggest(cmd) {
				return fmt.Errorf("invalid or program-owned command %q", cmd)
			}
		}
	}
	if !validBase(p.Base) {
		return fmt.Errorf("base must be origin/<branch> or a local branch name")
	}
	if err := p.Jira.validate(); err != nil {
		return err
	}
	return p.Management.validate(p.Jira)
}

func (p Profile) unit() string {
	return "USD"
}

// model is the step's pi model: SM_MODEL_<STATE> or the profile's models, else the step's default.
func (p Profile) model(st Step) string {
	if m := p.Models[st.Name]; m != "" {
		return m
	}
	if st.Model != "" {
		return st.Model
	}
	return piWorkModel
}
