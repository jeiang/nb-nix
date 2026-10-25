package engine

import (
	"encoding/json"
	"strings"
	"testing"
)

// snapshot builds a Snapshot from raw API responses (JSON), as Fetch would.
func snapshot(t *testing.T, raws map[string]string) *Snapshot {
	t.Helper()
	s := &Snapshot{AccountID: "acc", Objects: map[string][]*LiveObj{}}
	for _, k := range Kinds {
		src, ok := raws[k.Name]
		if !ok {
			continue
		}
		var docs []Doc
		dec := json.NewDecoder(strings.NewReader(src))
		dec.UseNumber()
		if err := dec.Decode(&docs); err != nil {
			t.Fatalf("%s: %v", k.Name, err)
		}
		for _, d := range docs {
			parentID, parentName := "", ""
			if k.Parent != "" {
				parentID, parentName = str(d["_parent"]), str(d["_parent_name"])
				delete(d, "_parent")
				delete(d, "_parent_name")
			}
			if k.Skip != nil && k.Skip(d) {
				continue
			}
			s.Objects[k.Name] = append(s.Objects[k.Name], newLive(str(d["id"]), parentID, parentName, d))
		}
	}
	s.index()
	for _, k := range Kinds {
		for _, o := range s.Objects[k.Name] {
			if err := s.name(k, o); err != nil {
				t.Fatal(err)
			}
		}
	}
	return s
}

func desired(t *testing.T, src string) *Desired {
	t.Helper()
	d, err := ParseDesired([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func mustPlan(t *testing.T, s *Snapshot, d *Desired) *Plan {
	t.Helper()
	p, err := MakePlan(s, d)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

const liveBase = `{
	"groups": [
		{"id": "g1", "name": "admins", "issued": "api", "peers": [{"id": "p1", "name": "alpha"}, {"id": "p2", "name": "beta"}], "resources": []},
		{"id": "g2", "name": "servers", "issued": "api", "peers": [], "resources": [{"id": "r1", "type": "host"}]},
		{"id": "g0", "name": "All", "issued": "api", "peers": [], "resources": []},
		{"id": "g9", "name": "sso-team", "issued": "jwt", "peers": [], "resources": []}
	],
	"peers": [
		{"id": "p1", "name": "alpha", "ip": "100.64.0.1", "ssh_enabled": false, "login_expiration_enabled": true, "inactivity_expiration_enabled": false, "approval_required": false, "groups": []},
		{"id": "p2", "name": "beta", "ip": "100.64.0.2", "ssh_enabled": false, "login_expiration_enabled": true, "inactivity_expiration_enabled": false, "approval_required": false, "groups": []}
	],
	"policies": [
		{"id": "pol1", "name": "ssh", "description": "", "enabled": true, "source_posture_checks": [],
		 "rules": [{"id": "pol1", "name": "ssh", "enabled": true, "action": "accept", "bidirectional": false, "protocol": "tcp", "ports": ["22"],
		            "sources": [{"id": "g1", "name": "admins"}], "destinations": [{"id": "g2", "name": "servers"}, {"id": "g1", "name": "admins"}]}]},
		{"id": "pol2", "name": "Temporary access policy for peer gamma", "enabled": true, "rules": [{"id": "x", "name": "t", "enabled": true, "action": "accept", "bidirectional": false, "protocol": "all"}]}
	],
	"setup_keys": [
		{"id": "k1", "name": "servers", "type": "reusable", "auto_groups": ["g2"], "usage_limit": 0, "ephemeral": false, "allow_extra_dns_labels": false, "revoked": false, "key": "****", "valid": true}
	],
	"account": [
		{"id": "acc", "settings": {"dns_domain": "mesh.test", "network_range": "100.64.0.0/16", "peer_login_expiration_enabled": true, "peer_login_expiration": 86400,
		 "ipv6_enabled_groups": ["g0"], "metrics_push_enabled": true, "extra": {"peer_approval_enabled": true}}}
	]
}`

func liveSnapshot(t *testing.T) *Snapshot {
	var parts map[string]json.RawMessage
	if err := json.Unmarshal([]byte(liveBase), &parts); err != nil {
		t.Fatal(err)
	}
	raws := map[string]string{}
	for k, v := range parts {
		raws[k] = string(v)
	}
	return snapshot(t, raws)
}

func opFor(p *Plan, kind, key string) *Op {
	for i := range p.Ops {
		if p.Ops[i].Kind == kind && p.Ops[i].Key == key {
			return &p.Ops[i]
		}
	}
	return nil
}

func TestPlanMatchingStateHasNoChanges(t *testing.T) {
	s := liveSnapshot(t)
	// Reference lists in another order, and only some fields declared.
	p := mustPlan(t, s, desired(t, `{
		"groups": [{"name": "admins"}, {"name": "servers"}],
		"policies": [{"name": "ssh", "rules": [{"sources": ["admins"], "destinations": ["admins", "servers"], "ports": ["22"]}]}],
		"account": {"settings": {"dns_domain": "mesh.test", "ipv6_enabled_groups": ["All"]}}
	}`))
	if len(p.Ops) != 0 {
		t.Fatalf("want no ops, got %+v", p.Ops)
	}
}

func TestUpdateKeepsUndeclaredFieldsAndResolvesNames(t *testing.T) {
	s := liveSnapshot(t)
	p := mustPlan(t, s, desired(t, `{"policies": [{"name": "ssh", "rules": [{"ports": ["22", "2222"]}]}]}`))
	op := opFor(p, "policies", "ssh")
	if op == nil || op.Action != "update" {
		t.Fatalf("want update, got %+v", p.Ops)
	}
	if len(op.Changes) != 1 || op.Changes[0].Path != "rules[0].ports" {
		t.Fatalf("want one change at rules[0].ports, got %+v", op.Changes)
	}
	a := &applier{snap: s, created: map[string]map[string]string{}}
	body, err := a.body(KindByName("policies"), op, false)
	if err != nil {
		t.Fatal(err)
	}
	rule := body["rules"].([]any)[0].(map[string]any)
	if rule["id"] != "pol1" {
		t.Errorf("rule id not kept: %v", rule["id"])
	}
	if got := rule["sources"].([]any); len(got) != 1 || got[0] != "g1" {
		t.Errorf("sources not resolved to IDs: %v", got)
	}
	if rule["protocol"] != "tcp" {
		t.Errorf("undeclared protocol not kept: %v", rule["protocol"])
	}
}

func TestGroupUpdateKeepsLiveMembersAndResources(t *testing.T) {
	s := liveSnapshot(t)
	p := mustPlan(t, s, desired(t, `{"groups": [{"name": "servers", "peers": ["alpha"]}]}`))
	op := opFor(p, "groups", "servers")
	if op == nil {
		t.Fatal("no op")
	}
	a := &applier{snap: s, created: map[string]map[string]string{}}
	body, err := a.body(KindByName("groups"), op, false)
	if err != nil {
		t.Fatal(err)
	}
	res, _ := body["resources"].([]any)
	if len(res) != 1 {
		t.Errorf("group resources must be resent, got %v", body["resources"])
	}
	// Undeclared peers on another group stay untouched: no op for "admins".
	if opFor(p, "groups", "admins") != nil {
		t.Error("undeclared group changed")
	}
	// Removing members from a declared membership is risky.
	p = mustPlan(t, s, desired(t, `{"groups": [{"name": "admins", "peers": ["alpha"]}]}`))
	if op := opFor(p, "groups", "admins"); op == nil || len(op.Risky) == 0 {
		t.Errorf("member removal not flagged risky: %+v", op)
	}
}

func TestRiskyAccountChanges(t *testing.T) {
	s := liveSnapshot(t)
	p := mustPlan(t, s, desired(t, `{"account": {"settings": {"network_range": "100.65.0.0/16", "extra": {"peer_approval_enabled": false}}}}`))
	op := opFor(p, "account", "")
	if op == nil {
		t.Fatal("no account op")
	}
	joined := strings.Join(op.Risky, ";")
	for _, want := range []string{"network_range", "peer approval"} {
		if !strings.Contains(joined, want) {
			t.Errorf("risky reasons %q miss %q", joined, want)
		}
	}
	// The update body carries every live setting, not only declared ones.
	settings := op.Body["settings"].(map[string]any)
	if settings["metrics_push_enabled"] != true || settings["dns_domain"] != "mesh.test" {
		t.Errorf("live settings dropped from body: %v", settings)
	}
}

func TestSetupKeyImmutableChangeReplaces(t *testing.T) {
	s := liveSnapshot(t)
	p := mustPlan(t, s, desired(t, `{"setup_keys": [{"name": "servers", "type": "one-off", "auto_groups": ["servers"], "sink": {"command": ["true"]}}]}`))
	op := opFor(p, "setup_keys", "servers")
	if op == nil || op.Action != "replace" {
		t.Fatalf("want replace, got %+v", op)
	}
	p = mustPlan(t, s, desired(t, `{"setup_keys": [{"name": "servers", "auto_groups": ["servers", "admins"], "expires_in": 3600}]}`))
	op = opFor(p, "setup_keys", "servers")
	if op == nil || op.Action != "update" {
		t.Fatalf("want update, got %+v", op)
	}
	body := KindByName("setup_keys").UpdateBody(op.Body)
	if len(body) != 2 || body["revoked"] != false {
		t.Errorf("update body must only carry revoked and auto_groups: %v", body)
	}
}

func TestPruneDeletesOnlyUnprotectedUndeclared(t *testing.T) {
	s := liveSnapshot(t)
	p := mustPlan(t, s, desired(t, `{"prune": {"groups": true, "policies": true}, "groups": [{"name": "admins"}]}`))
	var deleted []string
	for _, op := range p.Ops {
		if op.Action == "delete" {
			deleted = append(deleted, op.Kind+"/"+op.Key)
		}
	}
	want := "groups/servers,policies/ssh"
	if strings.Join(deleted, ",") != "policies/ssh,groups/servers" && strings.Join(deleted, ",") != want {
		t.Errorf("deleted %v, want policies/ssh and groups/servers only", deleted)
	}
	// Policies (which reference groups) are deleted before groups.
	if len(deleted) == 2 && deleted[0] != "policies/ssh" {
		t.Errorf("delete order %v: policies must go before groups", deleted)
	}
	// Without prune nothing is deleted, but undeclared objects are reported.
	p = mustPlan(t, s, desired(t, `{"groups": [{"name": "admins"}]}`))
	for _, op := range p.Ops {
		if op.Action == "delete" {
			t.Errorf("unexpected delete %+v", op)
		}
	}
	if len(p.Unmanaged) == 0 {
		t.Error("undeclared objects not reported")
	}
}

func TestPlanRejectsInvalidDesiredState(t *testing.T) {
	s := liveSnapshot(t)
	cases := map[string]string{
		"unknown reference": `{"policies": [{"name": "x", "rules": [{"sources": ["nobody"]}]}]}`,
		"peer creation":     `{"peers": [{"name": "gamma"}]}`,
		"declared twice":    `{"groups": [{"name": "a"}, {"name": "a"}]}`,
		"setup key no sink": `{"setup_keys": [{"name": "new", "type": "reusable"}]}`,
		"unknown network":   `{"network_resources": [{"network": "nope", "name": "r", "address": "10.0.0.1/32"}]}`,
	}
	for name, src := range cases {
		if _, err := MakePlan(s, desired(t, src)); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func TestCreateReferencingNewObjectsResolvesAtApply(t *testing.T) {
	s := liveSnapshot(t)
	p := mustPlan(t, s, desired(t, `{
		"groups": [{"name": "web"}],
		"policies": [{"name": "web", "enabled": true, "rules": [{"name": "web", "sources": ["web"], "destinations": ["servers"], "protocol": "tcp", "action": "accept", "enabled": true}]}]
	}`))
	if len(p.Ops) != 2 || p.Ops[0].Kind != "groups" || p.Ops[1].Kind != "policies" {
		t.Fatalf("want group create before policy create, got %+v", p.Ops)
	}
	a := &applier{snap: s, created: map[string]map[string]string{"groups": {"web": "gNEW"}}}
	body, err := a.body(KindByName("policies"), &p.Ops[1], true)
	if err != nil {
		t.Fatal(err)
	}
	rule := body["rules"].([]any)[0].(map[string]any)
	if rule["sources"].([]any)[0] != "gNEW" || rule["destinations"].([]any)[0] != "g2" {
		t.Errorf("refs not resolved: %v", rule)
	}
}
