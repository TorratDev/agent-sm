package main

import (
	"context"
	"fmt"
	"strings"
)

func (m *manager) verifyPaired(ctx context.Context, p *Preview, ghs map[string]*issueSnapshot, jiras map[string]*jiraSnapshot) error {
	for _, i := range p.Plan.Items {
		gh, err := m.ghIssue(ctx, ghs[i.ID].Number)
		if err != nil {
			return err
		}
		jira, err := m.jiraIssue(ctx, jiras[i.ID].Key)
		if err != nil {
			return err
		}
		meta, err := issueMetadata(gh.Body)
		if err != nil || meta.JiraKey != jira.Key || strings.TrimPrefix(meta.Base, "origin/") != m.itemBase(i) ||
			!hasBacklink(jira.Fields.Description, gh.URL) || gh.Title != i.Title || jira.Fields.Summary != i.Title || jira.Fields.IssueType.Name != m.typeName(i) {
			return fmt.Errorf("%s: final counterpart/field verification failed", i.ID)
		}
		if i.Parent != "" || i.RemoveParent {
			actual, err := m.parent(ctx, gh.Number)
			if err != nil {
				return err
			}
			if i.RemoveParent {
				if len(actual) != 0 || jira.Fields.Parent != nil {
					return fmt.Errorf("%s: parent removal readback mismatch", i.ID)
				}
			} else {
				if len(actual) != 1 || actual[0].ID != ghs[i.Parent].ID {
					return fmt.Errorf("%s: final GitHub parent mismatch", i.ID)
				}
				parent := m.jiraParent(p.Plan, i, jiras)
				if parent != "" && (jira.Fields.Parent == nil || jira.Fields.Parent.Key != parent) {
					return fmt.Errorf("%s: final Jira parent mismatch", i.ID)
				}
			}
		}
		labels := map[string]bool{}
		for _, label := range gh.Labels {
			labels[label.Name] = true
		}
		for _, label := range i.Labels {
			if !labels[label] {
				return fmt.Errorf("%s: requested label missing", i.ID)
			}
		}
		for _, label := range i.RemoveLabels {
			if labels[label] {
				return fmt.Errorf("%s: removed label is still present", i.ID)
			}
		}
		deps, err := ghPages[issueSnapshot](ctx, m, m.endpoint(gh.Number, "/dependencies/blocked_by"))
		if err != nil {
			return err
		}
		for _, remove := range []bool{false, true} {
			refs := i.DependsOn
			if remove {
				refs = i.RemoveDependencies
			}
			for _, ref := range refs {
				ghFound, jiraFound := false, false
				for _, dep := range deps {
					ghFound = ghFound || dep.ID == ghs[ref].ID
				}
				for _, link := range jira.Fields.Links {
					jiraFound = jiraFound || (link.InwardIssue != nil && link.InwardIssue.Key == jiras[ref].Key && strings.EqualFold(link.Type.Outward, "blocks"))
				}
				if ghFound == remove || jiraFound == remove {
					return fmt.Errorf("%s: final dependency verification failed", i.ID)
				}
			}
		}
	}
	return nil
}
