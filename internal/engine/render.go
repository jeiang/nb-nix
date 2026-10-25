package engine

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

var actionMark = map[string]string{"create": "+", "update": "~", "replace": "-/+", "delete": "-", "approve": "*"}

// Render writes a readable plan. Secret file paths are shown, never values.
func (p *Plan) Render(w io.Writer) {
	for _, op := range p.Ops {
		fmt.Fprintf(w, "%s %s %s %s\n", actionMark[op.Action], op.Action, op.Kind, op.Title())
		switch op.Action {
		case "create":
			b, _ := json.MarshalIndent(op.Body, "      ", "  ")
			fmt.Fprintf(w, "      %s\n", b)
		case "update", "replace":
			for _, c := range op.Changes {
				fmt.Fprintf(w, "      %s: %s -> %s\n", c.Path, compact(c.Old), compact(c.New))
			}
		}
		for _, r := range op.Risky {
			fmt.Fprintf(w, "      ! %s\n", r)
		}
	}
	if len(p.Unmanaged) > 0 {
		byKind := map[string][]string{}
		for _, u := range p.Unmanaged {
			label := (&Op{Key: u.Key, Label: u.Label}).Title()
			if u.Protected {
				label += " (protected)"
			}
			byKind[u.Kind] = append(byKind[u.Kind], label)
		}
		kinds := make([]string, 0, len(byKind))
		for k := range byKind {
			kinds = append(kinds, k)
		}
		sort.Strings(kinds)
		fmt.Fprintln(w, "\nNot declared (left alone unless pruned):")
		for _, k := range kinds {
			fmt.Fprintf(w, "  %s: %s\n", k, strings.Join(byKind[k], ", "))
		}
	}
	c := p.Counts()
	fmt.Fprintf(w, "\nPlan: %d to create, %d to update, %d to replace, %d to delete, %d to approve.\n",
		c["create"], c["update"], c["replace"], c["delete"], c["approve"])
	if r := p.Risky(); len(r) > 0 {
		fmt.Fprintf(w, "%d risky change(s); apply needs --allow-risky.\n", len(r))
	}
}

func compact(v any) string {
	if v == nil {
		return "(unset)"
	}
	b, _ := json.Marshal(v)
	return string(b)
}
