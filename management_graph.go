package main

import (
	"context"
	"fmt"
	"net/url"
	"strings"
)

func (m *manager) validateRemoteGraph(ctx context.Context, plan WorkPlan, expected map[string]expectedItem) error {
	ghKeys, jiraKeys := map[string]string{}, map[string]string{}
	ghItems, jiraItems := map[string]WorkItem{}, map[string]WorkItem{}
	for _, i := range plan.Items {
		ghKey, jiraKey := "local:"+i.ID, "local:"+i.ID
		if s := expected[i.ID].GitHub; s != nil {
			ghKey = s.URL
		}
		if s := expected[i.ID].Jira; s != nil {
			jiraKey = s.Key
		}
		ghKeys[i.ID], jiraKeys[i.ID] = ghKey, jiraKey
		ghItems[ghKey], jiraItems[jiraKey] = i, i
	}
	for _, system := range []string{"github", "jira"} {
		keys, items := ghKeys, ghItems
		if system == "jira" {
			keys, items = jiraKeys, jiraItems
		}
		for _, hierarchy := range []bool{false, true} {
			visiting, done := map[string]bool{}, map[string]bool{}
			count := 0
			var visit func(string) error
			visit = func(key string) error {
				if visiting[key] {
					return fmt.Errorf("%s hierarchy/dependency cycle at %s", system, key)
				}
				if done[key] {
					return nil
				}
				count++
				if count > 1000 {
					return fmt.Errorf("remote graph exceeds validation bound; no writes performed")
				}
				visiting[key] = true
				var neighbors []string
				if !strings.HasPrefix(key, "local:") {
					if system == "github" {
						u, err := url.Parse(key)
						if err != nil {
							return fmt.Errorf("invalid GitHub graph reference")
						}
						parts := strings.Split(strings.Trim(u.Path, "/"), "/")
						if u.Scheme != "https" || u.User != nil || u.Host != m.profile.Management.Host || len(parts) != 4 || parts[2] != "issues" {
							return fmt.Errorf("unverifiable GitHub graph reference")
						}
						path := "/repos/" + parts[0] + "/" + parts[1] + "/issues/" + parts[3]
						if hierarchy {
							var parent issueSnapshot
							err = m.api.call(ctx, "github", "GET", path+"/parent", nil, &parent)
							if err != nil {
								if status, ok := err.(trackerError); !ok || status.Status != 404 {
									return err
								}
							} else if parent.URL == "" {
								return fmt.Errorf("invalid parent evidence")
							} else {
								neighbors = append(neighbors, parent.URL)
							}
						} else {
							deps, err := ghPages[issueSnapshot](ctx, m, path+"/dependencies/blocked_by")
							if err != nil {
								return err
							}
							for _, d := range deps {
								neighbors = append(neighbors, d.URL)
							}
						}
					} else {
						s, err := m.jiraIssue(ctx, key)
						if err != nil {
							return err
						}
						if hierarchy && s.Fields.Parent != nil {
							neighbors = append(neighbors, s.Fields.Parent.Key)
						} else if !hierarchy {
							for _, link := range s.Fields.Links {
								if link.InwardIssue != nil && strings.EqualFold(link.Type.Outward, "blocks") {
									neighbors = append(neighbors, link.InwardIssue.Key)
								}
							}
						}
					}
				}
				if i, ok := items[key]; ok {
					if hierarchy {
						if i.RemoveParent {
							neighbors = nil
						} else if i.Parent != "" {
							neighbors = []string{keys[i.Parent]}
							if system == "jira" && i.Kind == "story" {
								neighbors = nil
								parent := i.Parent
								for parent != "" {
									candidate := items[keys[parent]]
									if candidate.Kind == "epic" {
										neighbors = []string{keys[parent]}
										break
									}
									parent = candidate.Parent
								}
								if len(neighbors) == 0 && m.profile.Jira.Epic != "" {
									neighbors = []string{m.profile.Jira.Epic}
								}
							}
						}
					} else {
						removed := map[string]bool{}
						for _, ref := range i.RemoveDependencies {
							removed[keys[ref]] = true
						}
						var kept []string
						for _, n := range neighbors {
							if !removed[n] {
								kept = append(kept, n)
							}
						}
						neighbors = kept
						for _, ref := range i.DependsOn {
							neighbors = append(neighbors, keys[ref])
						}
					}
				}
				for _, n := range neighbors {
					if err := visit(n); err != nil {
						return err
					}
				}
				visiting[key], done[key] = false, true
				return nil
			}
			for key := range items {
				if err := visit(key); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
