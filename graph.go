package main

// Edge is one transition of the state machine, derived from next() itself so the picture
// cannot drift from the code.
type Edge struct {
	From  string `json:"from"`
	Out   string `json:"out"`
	To    string `json:"to"`
	Exit  bool   `json:"exit"`
	Limit bool   `json:"limit,omitempty"` // only taken once a retry limit is reached
}

func edges() []Edge {
	seen := map[string]bool{}
	var es []Edge
	for _, visits := range []int{0, 99} { // 99 reveals the limit edges (gave-up, errored)
		var v [numStates]int
		for i := range v {
			v[i] = visits
		}
		for s := State(0); s < numStates; s++ {
			for _, o := range steps[s].Outs {
				n := next(s, o, v)
				e := Edge{From: s.String(), Out: o.String(), Limit: visits > 0}
				if n.Exit != "" {
					e.To, e.Exit = string(n.Exit), true
				} else {
					e.To = n.State.String()
				}
				if k := e.From + "|" + e.Out + "|" + e.To; !seen[k] {
					seen[k] = true
					es = append(es, e)
				}
			}
		}
	}
	return es
}
