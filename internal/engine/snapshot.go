package engine

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/jeiang/nb-nix/internal/nbapi"
)

// LiveObj is one object read from the server.
type LiveObj struct {
	ID         string
	ParentID   string
	ParentName string
	Raw        Doc // response as returned
	// Hash covers the named document only, so fields the server changes on
	// its own (last_seen, last_used, connection state) do not stale a plan.
	Hash  string
	Named Doc // request shape with IDs replaced by names
	Key   string
}

// Snapshot is the live state of every managed kind.
type Snapshot struct {
	AccountID string
	Objects   map[string][]*LiveObj
	// Unread lists, per child kind, the parents whose children the token may
	// not read (an auditor cannot list other service users' tokens).
	Unread  map[string][]string
	idName  map[string]map[string]string
	nameIDs map[string]map[string][]string
}

// Fetch reads every managed kind from the server.
func Fetch(ctx context.Context, c *nbapi.Client) (*Snapshot, error) {
	s := &Snapshot{Objects: map[string][]*LiveObj{}, Unread: map[string][]string{}}
	for _, k := range Kinds {
		objs, err := fetchKind(ctx, c, s, k)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", k.Name, err)
		}
		s.Objects[k.Name] = objs
	}
	s.index()
	for _, k := range Kinds {
		for _, o := range s.Objects[k.Name] {
			if err := s.name(k, o); err != nil {
				return nil, fmt.Errorf("%s %s: %w", k.Name, o.ID, err)
			}
		}
	}
	return s, nil
}

func fetchKind(ctx context.Context, c *nbapi.Client, s *Snapshot, k *Kind) ([]*LiveObj, error) {
	switch {
	case k.Name == "account":
		var accounts []Doc
		if err := c.Do(ctx, "GET", "/accounts", nil, &accounts); err != nil {
			return nil, err
		}
		if len(accounts) != 1 {
			return nil, fmt.Errorf("expected one account, got %d", len(accounts))
		}
		s.AccountID = str(accounts[0]["id"])
		return []*LiveObj{newLive(s.AccountID, "", "", accounts[0])}, nil
	case k.Singleton:
		var d Doc
		if err := c.Do(ctx, "GET", k.Collection(""), nil, &d); err != nil {
			return nil, err
		}
		return []*LiveObj{newLive("", "", "", d)}, nil
	case k.Parent != "":
		var out []*LiveObj
		for _, p := range s.Objects[k.Parent] {
			var docs []Doc
			if err := c.Do(ctx, "GET", k.Collection(p.ID), nil, &docs); err != nil {
				var he *nbapi.HTTPError
				if errors.As(err, &he) && he.Status == http.StatusForbidden {
					s.Unread[k.Name] = append(s.Unread[k.Name], str(p.Raw["name"]))
					continue
				}
				return nil, err
			}
			for _, d := range docs {
				out = append(out, newLive(str(d["id"]), p.ID, str(p.Raw["name"]), d))
			}
		}
		return out, nil
	default:
		var docs []Doc
		if err := c.Do(ctx, "GET", k.Collection(""), nil, &docs); err != nil {
			return nil, err
		}
		out := make([]*LiveObj, 0, len(docs))
		for _, d := range docs {
			if k.Skip != nil && k.Skip(d) {
				continue
			}
			out = append(out, newLive(str(d["id"]), "", "", d))
		}
		return out, nil
	}
}

func newLive(id, parentID, parentName string, raw Doc) *LiveObj {
	return &LiveObj{ID: id, ParentID: parentID, ParentName: parentName, Raw: raw}
}

func (s *Snapshot) index() {
	s.idName = map[string]map[string]string{}
	s.nameIDs = map[string]map[string][]string{}
	for _, k := range Kinds {
		ids := map[string]string{}
		names := map[string][]string{}
		for _, o := range s.Objects[k.Name] {
			n := str(o.Raw["name"])
			if o.ID == "" || n == "" {
				continue
			}
			ids[o.ID] = n
			names[n] = append(names[n], o.ID)
		}
		s.idName[k.Name] = ids
		s.nameIDs[k.Name] = names
	}
}

// name builds the request-shaped, name-referencing document for o.
func (s *Snapshot) name(k *Kind, o *LiveObj) error {
	d := deepCopy(o.Raw).(map[string]any)
	if k.Transform != nil {
		d = k.Transform(d)
	}
	projected, _ := project(d, k.ReqType).(map[string]any)
	if projected == nil {
		projected = Doc{}
	}
	named, err := mapRefs(k, projected, func(kind, id string) (string, error) {
		if id == "" {
			return "", nil
		}
		if n, ok := s.idName[kind][id]; ok {
			return n, nil
		}
		return "id:" + id, nil
	})
	if err != nil {
		return err
	}
	if k.Parent != "" {
		named[k.ParentField] = o.ParentName
	}
	for _, key := range k.Keep {
		named[key] = o.Raw[key]
	}
	o.Named = named
	o.Hash = hashOf(named)
	o.Key = k.Key(named)
	return nil
}

// mapRefs rewrites every reference in d with fn(targetKind, value).
func mapRefs(k *Kind, d Doc, fn func(kind, v string) (string, error)) (Doc, error) {
	var cur any = d
	for _, r := range k.Refs {
		var err error
		cur, err = walkStrings(cur, parsePath(r.Path), nil, func(v string, parent map[string]any) (string, error) {
			kind := r.Kind
			if r.KindFrom != nil {
				kind = r.KindFrom(parent)
			}
			if kind == "" {
				return v, nil
			}
			return fn(kind, v)
		})
		if err != nil {
			return nil, err
		}
	}
	return cur.(map[string]any), nil
}

// canon sorts every unordered list (reference lists) in d.
func canon(k *Kind, d Doc) Doc {
	var cur any = d
	for _, r := range k.Refs {
		segs := parsePath(r.Path)
		if len(segs) > 0 && segs[len(segs)-1] == "[]" {
			cur = sortAt(cur, segs)
		}
	}
	m, _ := cur.(map[string]any)
	return m
}
