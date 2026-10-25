package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/jeiang/nb-nix/internal/nbapi"
)

// ApplyOptions control apply.
type ApplyOptions struct {
	AllowRisky bool
	// Rotate holds "kind/key" entries whose secrets are sent even when unchanged.
	Rotate map[string]bool
	Log    io.Writer
}

// Apply makes the changes in p. It refuses a plan whose objects changed on
// the server since the plan was made, and stops at the first failure.
func Apply(ctx context.Context, c *nbapi.Client, p *Plan, opts ApplyOptions) error {
	if r := p.Risky(); len(r) > 0 && !opts.AllowRisky {
		return fmt.Errorf("plan has risky changes; rerun with --allow-risky to make them:\n  %s", strings.Join(r, "\n  "))
	}
	snap, err := Fetch(ctx, c)
	if err != nil {
		return err
	}
	if err := checkStale(snap, p); err != nil {
		return err
	}
	a := &applier{c: c, snap: snap, opts: opts, created: map[string]map[string]string{}}
	for i := range p.Ops {
		op := &p.Ops[i]
		if err := a.do(ctx, op); err != nil {
			return fmt.Errorf("%s %s %s: %w (%d of %d changes made)", op.Action, op.Kind, op.Title(), err, i, len(p.Ops))
		}
		fmt.Fprintf(opts.Log, "%s %s %s\n", pastTense(op.Action), op.Kind, op.Title())
	}
	// Old tokens of the user this apply authenticates as go last: deleting one
	// earlier could revoke the credential the remaining requests use.
	for _, del := range a.deferred {
		if err := a.c.Do(ctx, "DELETE", del.path, nil, nil); err != nil {
			return fmt.Errorf("delete replaced %s: %w (all other changes made)", del.what, err)
		}
		fmt.Fprintf(opts.Log, "deleted replaced %s\n", del.what)
	}
	return nil
}

type deferredDelete struct{ path, what string }

func pastTense(action string) string {
	switch action {
	case "create":
		return "created"
	case "update":
		return "updated"
	case "replace":
		return "replaced"
	case "delete":
		return "deleted"
	case "approve":
		return "approved"
	}
	return action
}

func checkStale(snap *Snapshot, p *Plan) error {
	if snap.AccountID != p.AccountID {
		return fmt.Errorf("plan was made for account %s, server has %s", p.AccountID, snap.AccountID)
	}
	var stale []string
	for _, op := range p.Ops {
		k := KindByName(op.Kind)
		if k == nil {
			return fmt.Errorf("plan has unknown kind %q", op.Kind)
		}
		var found *LiveObj
		for _, o := range snap.Objects[op.Kind] {
			if op.Action == "create" && o.Key == op.Key {
				stale = append(stale, fmt.Sprintf("%s %s now exists", op.Kind, quote(op.Key)))
			}
			if (op.LiveID != "" && o.ID == op.LiveID) || k.Singleton {
				found = o
			}
		}
		if op.Action == "create" {
			continue
		}
		switch {
		case found == nil:
			stale = append(stale, fmt.Sprintf("%s %s no longer exists", op.Kind, quote(op.Key)))
		case found.Hash != op.Hash:
			stale = append(stale, fmt.Sprintf("%s %s changed on the server", op.Kind, quote(op.Key)))
		}
	}
	if len(stale) > 0 {
		return fmt.Errorf("plan is stale, make a new plan:\n  %s", strings.Join(stale, "\n  "))
	}
	return nil
}

type applier struct {
	c        *nbapi.Client
	snap     *Snapshot
	opts     ApplyOptions
	created  map[string]map[string]string // kind -> name -> id
	deferred []deferredDelete
}

func (a *applier) do(ctx context.Context, op *Op) error {
	k := KindByName(op.Kind)
	switch op.Action {
	case "delete":
		return a.c.Do(ctx, "DELETE", k.Item(op.Parent, op.LiveID), nil, nil)
	case "approve":
		return a.c.Do(ctx, "POST", k.Item(op.Parent, op.LiveID)+"/approve", nil, nil)
	case "create":
		_, err := a.create(ctx, k, op)
		return err
	case "replace":
		if _, err := a.create(ctx, k, op); err != nil {
			return err
		}
		if k.Parent != "" && a.parentIsCurrent(k, op.Parent) {
			a.deferred = append(a.deferred, deferredDelete{k.Item(op.Parent, op.LiveID), op.Kind + " " + quote(op.Key)})
			return nil
		}
		return a.c.Do(ctx, "DELETE", k.Item(op.Parent, op.LiveID), nil, nil)
	case "update":
		body, err := a.body(k, op, op.SendSecrets || a.opts.Rotate[op.Kind+"/"+op.Key])
		if err != nil {
			return err
		}
		if k.UpdateBody != nil {
			body = k.UpdateBody(body)
		}
		id := op.LiveID
		if k.Name == "account" {
			id = a.snap.AccountID
		}
		return a.c.Do(ctx, "PUT", k.Item(op.Parent, id), body, nil)
	}
	return fmt.Errorf("unknown action %q", op.Action)
}

// parentIsCurrent reports whether the parent object is the user the API token
// belongs to.
func (a *applier) parentIsCurrent(k *Kind, parentID string) bool {
	for _, o := range a.snap.Objects[k.Parent] {
		if o.ID == parentID {
			return isCurrent(o.Raw)
		}
	}
	return false
}

func (a *applier) create(ctx context.Context, k *Kind, op *Op) (string, error) {
	body, err := a.body(k, op, true)
	if err != nil {
		return "", err
	}
	if k.CreateBody != nil {
		body = k.CreateBody(body)
	}
	parent := ""
	if k.Parent != "" {
		parent, err = a.resolve(k.Parent, str(op.Body[k.ParentField]))
		if err != nil {
			return "", err
		}
	}
	var resp Doc
	if err := a.c.Do(ctx, "POST", k.Collection(parent), body, &resp); err != nil {
		return "", err
	}
	idPath := "id"
	if k.Sink != nil {
		idPath = k.Sink.ID
	}
	id := str(get(resp, idPath))
	if a.created[k.Name] == nil {
		a.created[k.Name] = map[string]string{}
	}
	if n := str(body["name"]); n != "" {
		a.created[k.Name][n] = id
	}
	if k.Sink != nil {
		if err := a.sinkSecret(ctx, k, op, parent, id, str(get(resp, k.Sink.Secret))); err != nil {
			return "", err
		}
	}
	return id, nil
}

// body turns the named document of op into a request body: names become IDs,
// desired-only keys (and create-only keys, except on create) go, and secrets
// are read from their files or left empty.
func (a *applier) body(k *Kind, op *Op, withSecrets bool) (Doc, error) {
	d := deepCopy(op.Body).(map[string]any)
	for _, key := range k.DesiredOnly {
		delete(d, key)
	}
	if op.Action == "update" {
		for _, key := range k.CreateOnly {
			delete(d, key)
		}
	}
	for _, s := range k.Secrets {
		var ferr error
		r, _ := walkObjects(d, parsePath(s.Container), func(m map[string]any) {
			file, hasFile := m[s.Key+"_file"].(string)
			delete(m, s.Key+"_file")
			if withSecrets && hasFile {
				b, err := os.ReadFile(file)
				if err != nil {
					ferr = err
					return
				}
				m[s.Key] = strings.TrimRight(string(b), "\r\n")
			} else {
				m[s.Key] = ""
			}
		})
		if ferr != nil {
			return nil, fmt.Errorf("read secret: %w", ferr)
		}
		d, _ = r.(map[string]any)
	}
	return mapRefs(k, d, func(kind, name string) (string, error) {
		return a.resolve(kind, name)
	})
}

func (a *applier) resolve(kind, name string) (string, error) {
	if name == "" {
		return "", nil
	}
	if id, ok := strings.CutPrefix(name, "id:"); ok {
		return id, nil
	}
	if id, ok := a.created[kind][name]; ok {
		return id, nil
	}
	ids := a.snap.nameIDs[kind][name]
	switch len(ids) {
	case 1:
		return ids[0], nil
	case 0:
		return "", fmt.Errorf("unknown %s %q", kind, name)
	default:
		return "", fmt.Errorf("ambiguous %s %q", kind, name)
	}
}

// sinkSecret hands a secret returned at creation to the object's sink command
// on stdin. If that fails, the new object is deleted (setup keys are revoked
// if deleting fails) so no unrecorded secret stays valid.
func (a *applier) sinkSecret(ctx context.Context, k *Kind, op *Op, parent, id, value string) error {
	err := runSink(ctx, op.Body["sink"], value)
	if err == nil {
		return nil
	}
	if delErr := a.c.Do(ctx, "DELETE", k.Item(parent, id), nil, nil); delErr != nil {
		var revErr error
		if k.Name == "setup_keys" {
			revErr = a.c.Do(ctx, "PUT", k.Item(parent, id), Doc{"revoked": true, "auto_groups": []any{}}, nil)
		}
		return errors.Join(fmt.Errorf("%s sink: %w", k.Name, err),
			fmt.Errorf("delete new %s %s: %w", k.Name, id, delErr), revErr)
	}
	return fmt.Errorf("%s sink failed, new object deleted: %w", k.Name, err)
}

func runSink(ctx context.Context, sink any, value string) error {
	m, _ := sink.(map[string]any)
	l, _ := m["command"].([]any)
	if len(l) == 0 {
		return errors.New("no sink command configured")
	}
	argv := make([]string, len(l))
	for i, e := range l {
		argv[i] = str(e)
	}
	input := []byte(value)
	if str(m["format"]) != "raw" {
		input, _ = json.Marshal(value)
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdin = bytes.NewReader(input)
	var stderr bytes.Buffer
	cmd.Stdout = &stderr
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w: %s", argv[0], err, strings.TrimSpace(stderr.String()))
	}
	return nil
}
