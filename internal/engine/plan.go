package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Desired is the declared state, as produced by the Nix module.
type Desired struct {
	Server  string
	Prune   map[string]bool
	Objects map[string][]Doc
}

// ParseDesired reads the desired-state JSON.
func ParseDesired(data []byte) (*Desired, error) {
	var raw map[string]any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("parse desired state: %w", err)
	}
	d := &Desired{Prune: map[string]bool{}, Objects: map[string][]Doc{}}
	for key, v := range raw {
		switch key {
		case "server":
			d.Server = str(v)
			continue
		case "prune":
			m, _ := v.(map[string]any)
			for kind, b := range m {
				if KindByName(kind) == nil {
					return nil, fmt.Errorf("prune: unknown kind %q", kind)
				}
				d.Prune[kind], _ = b.(bool)
			}
			continue
		}
		k := KindByName(key)
		if k == nil {
			return nil, fmt.Errorf("unknown kind %q", key)
		}
		if k.Singleton {
			m, ok := v.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("%s: expected an object", key)
			}
			d.Objects[key] = []Doc{m}
			continue
		}
		l, ok := v.([]any)
		if !ok {
			return nil, fmt.Errorf("%s: expected a list", key)
		}
		for i, e := range l {
			m, ok := e.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("%s[%d]: expected an object", key, i)
			}
			d.Objects[key] = append(d.Objects[key], m)
		}
	}
	return d, nil
}

// Op is one change to make.
type Op struct {
	Kind    string   `json:"kind"`
	Key     string   `json:"key"`
	Action  string   `json:"action"` // create, update, replace, delete
	LiveID  string   `json:"live_id,omitempty"`
	Parent  string   `json:"parent_id,omitempty"`
	Hash    string   `json:"live_hash,omitempty"`
	Body    Doc      `json:"body,omitempty"` // named; may hold *_file secret paths
	Changes []Change `json:"changes,omitempty"`
	Risky   []string `json:"risky,omitempty"`
	// SendSecrets is set when an update must include write-only secrets
	// (an auth method is being turned on).
	SendSecrets bool `json:"send_secrets,omitempty"`
}

// Unmanaged is a live object with no declaration.
type Unmanaged struct {
	Kind      string `json:"kind"`
	Key       string `json:"key"`
	Protected bool   `json:"protected,omitempty"`
}

// Plan is the set of changes from live to desired state.
type Plan struct {
	Server    string      `json:"server"`
	AccountID string      `json:"account_id"`
	Version   string      `json:"netbird_version"`
	Ops       []Op        `json:"ops"`
	Unmanaged []Unmanaged `json:"unmanaged"`
}

// MakePlan compares desired state with a live snapshot.
func MakePlan(snap *Snapshot, want *Desired) (*Plan, error) {
	p := &Plan{AccountID: snap.AccountID}
	if err := checkRefs(snap, want); err != nil {
		return nil, err
	}
	var deletes []Op
	var errs []string
	for _, k := range Kinds {
		ops, dels, unmanaged, kindErrs := planKind(k, snap, want)
		p.Ops = append(p.Ops, ops...)
		deletes = append(dels, deletes...) // reverse kind order
		p.Unmanaged = append(p.Unmanaged, unmanaged...)
		errs = append(errs, kindErrs...)
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("invalid desired state:\n  %s", strings.Join(errs, "\n  "))
	}
	p.Ops = append(p.Ops, deletes...)
	return p, nil
}

func planKind(k *Kind, snap *Snapshot, want *Desired) (ops, deletes []Op, unmanaged []Unmanaged, errs []string) {
	live := map[string][]*LiveObj{}
	for _, o := range snap.Objects[k.Name] {
		live[o.Key] = append(live[o.Key], o)
	}
	declared := map[string]bool{}
	for _, d := range want.Objects[k.Name] {
		key := k.Key(d)
		if !k.Singleton && strings.TrimPrefix(key, "/") == "" {
			errs = append(errs, fmt.Sprintf("%s: object without a name", k.Name))
			continue
		}
		if declared[key] {
			errs = append(errs, fmt.Sprintf("%s %q: declared twice", k.Name, key))
			continue
		}
		declared[key] = true
		matches := live[key]
		switch {
		case len(matches) > 1:
			errs = append(errs, fmt.Sprintf("%s %q: %d live objects share this name; rename or delete the extras", k.Name, key, len(matches)))
		case len(matches) == 0 && k.NoCreate:
			errs = append(errs, fmt.Sprintf("%s %q: not found on the server (nb-nix cannot create %s)", k.Name, key, k.Name))
		case len(matches) == 0:
			ops = append(ops, Op{Kind: k.Name, Key: key, Action: "create", Body: d})
		default:
			o := matches[0]
			if pending, _ := o.Raw["pending_approval"].(bool); pending && k.Name == "users" {
				ops = append(ops, Op{Kind: k.Name, Key: key, Action: "approve", LiveID: o.ID, Hash: o.Hash})
				// Approval unblocks the user; do not block it again unless declared.
				d = overlay(Doc{"is_blocked": false}, d).(map[string]any)
			}
			op, ok, checkErrs := updateOp(k, o, d)
			for _, e := range checkErrs {
				errs = append(errs, fmt.Sprintf("%s %q: %s", k.Name, key, e))
			}
			if !ok && k.Renew != nil {
				if reason := k.Renew(o.Raw, d); reason != "" {
					op, ok = Op{
						Kind: k.Name, Key: key, Action: "replace", LiveID: o.ID, Parent: o.ParentID, Hash: o.Hash,
						Body: overlay(o.Named, d).(map[string]any), Risky: []string{"replaces " + k.Name + " " + quote(key) + ": " + reason},
					}, true
				}
			}
			if ok {
				ops = append(ops, op)
			}
		}
		if n := len(ops); n > 0 && ops[n-1].Key == key && k.Sink != nil &&
			(ops[n-1].Action == "create" || ops[n-1].Action == "replace") && d["sink"] == nil {
			errs = append(errs, fmt.Sprintf("%s %q: creating it needs a sink (the secret is shown only once)", k.Name, key))
		}
	}
	if k.Singleton || k.Quiet {
		return ops, nil, nil, errs
	}
	deletedParents := map[string]bool{}
	parents := map[string]Doc{}
	if k.Parent != "" {
		for _, o := range snap.Objects[k.Parent] {
			parents[o.ID] = o.Raw
			if want.Prune[k.Parent] && !declaredIn(want, k.Parent, o.Key) {
				deletedParents[o.ID] = true
			}
		}
	}
	keys := make([]string, 0, len(live))
	for key := range live {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if declared[key] {
			continue
		}
		for _, o := range live[key] {
			protected := k.NoDelete || (k.Protected != nil && k.Protected(o.Raw, parents[o.ParentID]))
			unmanaged = append(unmanaged, Unmanaged{Kind: k.Name, Key: key, Protected: protected})
			if !want.Prune[k.Name] || protected || deletedParents[o.ParentID] {
				continue
			}
			deletes = append(deletes, Op{
				Kind: k.Name, Key: key, Action: "delete", LiveID: o.ID, Parent: o.ParentID, Hash: o.Hash,
				Risky: []string{"deletes " + k.Name + " " + quote(key)},
			})
		}
	}
	return ops, deletes, unmanaged, errs
}

func declaredIn(want *Desired, kind, key string) bool {
	k := KindByName(kind)
	for _, d := range want.Objects[kind] {
		if k.Key(d) == key {
			return true
		}
	}
	return false
}

// compareView removes keys that do not take part in comparison: secrets,
// create-only keys and desired-only keys other than the parent reference.
func compareView(k *Kind, d Doc) Doc {
	out := deepCopy(d).(map[string]any)
	for _, key := range append(append([]string(nil), k.DesiredOnly...), k.CreateOnly...) {
		if key != k.ParentField {
			delete(out, key)
		}
	}
	for _, s := range k.Secrets {
		out = stripSecret(out, s)
	}
	return out
}

func stripSecret(d Doc, s Secret) Doc {
	r, _ := walkObjects(d, parsePath(s.Container), func(m map[string]any) {
		delete(m, s.Key)
		delete(m, s.Key+"_file")
	})
	out, _ := r.(map[string]any)
	return out
}

// walkObjects calls fn on (copies of) every object at path and returns the
// rewritten value.
func walkObjects(v any, segs []string, fn func(map[string]any)) (any, bool) {
	if len(segs) == 0 {
		m, ok := v.(map[string]any)
		if !ok {
			return v, false
		}
		c := make(map[string]any, len(m))
		for k, x := range m {
			c[k] = x
		}
		fn(c)
		return c, true
	}
	if segs[0] == "[]" {
		l, ok := v.([]any)
		if !ok {
			return v, false
		}
		out := make([]any, len(l))
		found := false
		for i, e := range l {
			var f bool
			out[i], f = walkObjects(e, segs[1:], fn)
			found = found || f
		}
		return out, found
	}
	m, ok := v.(map[string]any)
	if !ok {
		return v, false
	}
	e, ok := m[segs[0]]
	if !ok {
		return v, false
	}
	r, found := walkObjects(e, segs[1:], fn)
	out := make(map[string]any, len(m))
	for k, x := range m {
		out[k] = x
	}
	out[segs[0]] = r
	return out, found
}

// updateOp compares a live object with its declaration. It returns the update
// (if anything differs) and reasons the update cannot be made.
func updateOp(k *Kind, o *LiveObj, want Doc) (Op, bool, []string) {
	liveView := compareView(k, o.Named)
	merged := overlay(liveView, compareView(k, want)).(map[string]any)
	changes := diff("", canon(k, liveView), canon(k, merged))
	if len(changes) == 0 {
		return Op{}, false, nil
	}
	var errs []string
	if k.Check != nil {
		errs = k.Check(o.Raw, liveView, merged)
	}
	op := Op{
		Kind: k.Name, Key: o.Key, Action: "update", LiveID: o.ID, Parent: o.ParentID, Hash: o.Hash,
		Body: overlay(o.Named, want).(map[string]any), Changes: changes,
	}
	for _, c := range changes {
		for _, f := range k.Immutable {
			if c.Path == f {
				op.Action = "replace"
			}
		}
	}
	if op.Action == "replace" {
		op.Risky = append(op.Risky, "replaces "+k.Name+" "+quote(o.Key)+" (immutable field changed)")
	}
	if k.Risky != nil {
		op.Risky = append(op.Risky, k.Risky(liveView, merged)...)
	}
	for _, s := range k.Secrets {
		if needsSecret(o.Named, want, s) {
			op.SendSecrets = true
		}
	}
	return op, true, errs
}

// needsSecret reports whether an update must carry the secret: its auth method
// turns on, or (for lists such as header auths, matched by position) the list
// changes. Otherwise the server keeps the stored value when it gets "".
func needsSecret(live, want Doc, s Secret) bool {
	merged := overlay(live, want).(map[string]any)
	if path, ok := strings.CutSuffix(s.Container, "[]"); ok {
		if get(want, path) == nil {
			return false
		}
		return !equalJSON(get(stripSecret(live, s), path), get(stripSecret(merged, s), path))
	}
	return !boolAt(live, s.Container+".enabled") && boolAt(merged, s.Container+".enabled")
}

// checkRefs verifies that every name referenced by desired state exists live
// or is declared.
func checkRefs(snap *Snapshot, want *Desired) error {
	declaredNames := map[string]map[string]bool{}
	for _, k := range Kinds {
		names := map[string]bool{}
		for _, d := range want.Objects[k.Name] {
			if n := str(d["name"]); n != "" {
				names[n] = true
			}
		}
		declaredNames[k.Name] = names
	}
	var errs []string
	for _, k := range Kinds {
		for _, d := range want.Objects[k.Name] {
			_, _ = mapRefs(k, d, func(kind, name string) (string, error) {
				if name == "" || strings.HasPrefix(name, "id:") {
					return name, nil
				}
				if declaredNames[kind][name] {
					return name, nil
				}
				switch n := len(snap.nameIDs[kind][name]); {
				case n == 0:
					errs = append(errs, fmt.Sprintf("%s %q: references unknown %s %q", k.Name, k.Key(d), kind, name))
				case n > 1:
					errs = append(errs, fmt.Sprintf("%s %q: reference to %s %q is ambiguous (%d live objects share the name)", k.Name, k.Key(d), kind, name, n))
				}
				return name, nil
			})
			if k.Parent != "" {
				parent := str(d[k.ParentField])
				if !declaredNames[k.Parent][parent] && len(snap.nameIDs[k.Parent][parent]) != 1 {
					errs = append(errs, fmt.Sprintf("%s %q: unknown %s %q", k.Name, k.Key(d), k.ParentField, parent))
				}
			}
		}
	}
	if len(errs) > 0 {
		sort.Strings(errs)
		return fmt.Errorf("invalid references:\n  %s", strings.Join(errs, "\n  "))
	}
	return nil
}

func quote(s string) string { return fmt.Sprintf("%q", s) }

// Risky lists the risky reasons in the plan.
func (p *Plan) Risky() []string {
	var out []string
	for _, op := range p.Ops {
		for _, r := range op.Risky {
			out = append(out, fmt.Sprintf("%s %s: %s", op.Kind, quote(op.Key), r))
		}
	}
	return out
}

// Counts returns the number of ops per action.
func (p *Plan) Counts() map[string]int {
	c := map[string]int{}
	for _, op := range p.Ops {
		c[op.Action]++
	}
	return c
}
