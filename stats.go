package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

const thinEvidence = 10 // fewer sessions than this per state: unused is not yet proof

// report summarizes the stats JSONL per state: cost, tokens, and which listed tools
// and MCP servers were never called (candidates to trim from that state's allow list).
func report(jsonl []byte) string {
	byState := map[string][]Stats{}
	for _, line := range strings.Split(string(jsonl), "\n") {
		var st Stats
		if json.Unmarshal([]byte(line), &st) == nil && st.State != "" {
			key := st.State
			if st.Provider != "" {
				key = st.Provider + "/" + key
			}
			byState[key] = append(byState[key], st)
		}
	}
	var states []string
	for s := range byState {
		states = append(states, s)
	}
	sort.Strings(states)

	var b strings.Builder
	for _, state := range states {
		list := byState[state]
		var cost float64
		var create, read, out, turns int
		listed, called := map[string]int{}, map[string]int{}
		mcpLoaded := map[string]int{}
		denied := map[string]int{}
		bash := map[string]int{}
		var sample []string
		models := map[string]int{}
		killed, unknown := 0, 0
		credits := list[0].Provider == "copilot"
		for _, st := range list {
			models[st.Model]++
			if st.Killed {
				killed++
			}
			if st.UsageUnknown || st.Killed {
				unknown++
			} else if credits && st.Credits != nil {
				cost += *st.Credits
			} else if credits {
				unknown++
			} else {
				cost += st.CostUSD
			}
			create += st.CacheCreation
			read += st.CacheRead
			out += st.OutputTokens
			turns += st.Turns
			for _, t := range st.ToolsListed {
				listed[t]++
			}
			for t, n := range st.ToolCalls {
				called[t] += n
			}
			for k, c := range st.BashCmds {
				bash[k] += c
			}
			for tl, c := range st.DeniedTools {
				denied[tl] += c
			}
			if len(sample) < 3 {
				sample = append(sample, st.DeniedSample...)
			}
			for _, m := range st.MCP {
				mcpLoaded[m]++
			}
		}
		n := len(list)
		if credits {
			fmt.Fprintf(&b, "%s: %d sessions, %.3f known credits, %d unknown-usage sessions; tool/turn telemetry unavailable\n",
				state, n, cost, unknown)
		} else {
			fmt.Fprintf(&b, "%s: %d sessions, $%.3f known total, %d unknown-usage sessions, avg cache-create %d, cache-read %d, output %d, turns %.1f\n",
				state, n, cost, unknown, create/n, read/n, out/n, float64(turns)/float64(n))
		}
		var ml []string
		for m, k := range models {
			if m == "" {
				m = "unknown"
			}
			ml = append(ml, fmt.Sprintf("%s x%d", m, k))
		}
		sort.Strings(ml)
		fmt.Fprintf(&b, "  models: %s\n", strings.Join(ml, ", "))
		if credits {
			continue
		}
		if killed > 0 {
			fmt.Fprintf(&b, "  note: %d killed sessions reported no cost; the total above leaves them out\n", killed)
		}
		if n < thinEvidence {
			fmt.Fprintf(&b, "  note: only %d sessions, unused is not proof yet (want %d+)\n", n, thinEvidence)
		}

		var unused, used []string
		for t := range listed {
			if t == "StructuredOutput" {
				continue
			}
			if called[t] == 0 {
				unused = append(unused, t)
			} else {
				used = append(used, fmt.Sprintf("%s x%d", t, called[t]))
			}
		}
		sort.Strings(unused)
		sort.Strings(used)
		fmt.Fprintf(&b, "  tools listed %d, used: %s\n", len(listed), strings.Join(used, ", "))
		var bl []string
		for k, c := range bash {
			bl = append(bl, fmt.Sprintf("%s x%d", k, c))
		}
		sort.Strings(bl)
		fmt.Fprintf(&b, "  bash commands: %s\n", strings.Join(bl, ", "))
		fmt.Fprintf(&b, "  ALLOW suggestion: %s\n", suggestAllow(strings.TrimPrefix(state, "claude/"), called, bash))
		fmt.Fprintf(&b, "  TRIM tools (listed, never called) %d: %s\n", len(unused), strings.Join(unused, ", "))

		if len(denied) > 0 {
			var dn []string
			for tl, c := range denied {
				dn = append(dn, fmt.Sprintf("%s x%d", tl, c))
			}
			sort.Strings(dn)
			fmt.Fprintf(&b, "  DENIED (wanted, not allowed; add to allow list or fix the prompt): %s\n", strings.Join(dn, ", "))
			for _, sm := range sample {
				fmt.Fprintf(&b, "    e.g. %s\n", sm)
			}
		}

		var mcpUnused []string
		for m := range mcpLoaded {
			if !mcpCalled(m, called) {
				mcpUnused = append(mcpUnused, m)
			}
		}
		sort.Strings(mcpUnused)
		fmt.Fprintf(&b, "  MCP servers loaded %d, TRIM (no calls): %s\n", len(mcpLoaded), strings.Join(mcpUnused, ", "))
	}
	return b.String()
}

// mcpCalled: tool names look like mcp__<server>__<tool>, with the server name
// normalized (non-alphanumerics become "_"), e.g. "claude.ai Notion" -> claude_ai_Notion.
func mcpCalled(server string, called map[string]int) bool {
	norm := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			return r
		}
		return '_'
	}, server)
	norm = strings.TrimPrefix(norm, "plugin_") // plugin servers are named plugin_<plugin>_<server>
	for t := range called {
		if strings.HasPrefix(t, "mcp__") && strings.Contains(t, norm) {
			return true
		}
	}
	return false
}

// allowJSON is the per-state allow lists suggested by the stats, as SM_ALLOW_FILE expects.
func allowJSON(jsonl []byte) string {
	called, bash := map[string]map[string]int{}, map[string]map[string]int{}
	for _, line := range strings.Split(string(jsonl), "\n") {
		var st Stats
		if json.Unmarshal([]byte(line), &st) != nil || st.State == "" {
			continue
		}
		if st.Provider != "" && st.Provider != "claude" {
			continue // Copilot command policy is configured explicitly, not inferred.
		}
		if called[st.State] == nil {
			called[st.State], bash[st.State] = map[string]int{}, map[string]int{}
		}
		for t, n := range st.ToolCalls {
			called[st.State][t] += n
		}
		for k, n := range st.BashCmds {
			bash[st.State][k] += n
		}
	}
	out := map[string]string{}
	for s := range called {
		out[s] = suggestAllow(s, called[s], bash[s])
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	return string(b)
}
