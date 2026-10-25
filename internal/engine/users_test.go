package engine

import (
	"strings"
	"testing"
	"time"
)

func userSnapshot(t *testing.T) *Snapshot {
	return snapshot(t, map[string]string{
		"groups": `[
			{"id": "g1", "name": "admins", "issued": "api", "peers": []},
			{"id": "g2", "name": "vpn_access", "issued": "jwt", "peers": []},
			{"id": "g0", "name": "All", "issued": "api", "peers": []}
		]`,
		"users": `[
			{"id": "u-owner", "email": "Owner@example.com", "role": "owner", "auto_groups": ["g1"], "is_blocked": false, "is_service_user": false, "is_current": false},
			{"id": "u-a", "email": "a@example.com", "role": "user", "auto_groups": ["g2"], "is_blocked": false, "is_service_user": false, "is_current": false},
			{"id": "u-me", "email": "me@example.com", "role": "admin", "auto_groups": [], "is_blocked": false, "is_service_user": false, "is_current": true},
			{"id": "u-new", "email": "new@example.com", "role": "user", "auto_groups": [], "is_blocked": true, "pending_approval": true, "is_service_user": false},
			{"id": "u-svc", "name": "ci", "email": "", "role": "auditor", "auto_groups": [], "is_service_user": true}
		]`,
	})
}

func TestUserGroupsAreSetByIDWithNames(t *testing.T) {
	s := userSnapshot(t)
	p := mustPlan(t, s, desired(t, `{"users": [{"id": "u-a", "auto_groups": ["vpn_access", "admins"]}]}`))
	op := opFor(p, "users", "u-a")
	if op == nil || op.Action != "update" || len(op.Risky) != 0 {
		t.Fatalf("want one plain update, got %+v", p.Ops)
	}
	a := &applier{snap: s, created: map[string]map[string]string{}}
	body, err := a.body(KindByName("users"), op, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, sent := body["id"]; sent {
		t.Error("id must not be sent in the body")
	}
	if body["role"] != "user" || body["is_blocked"] != false {
		t.Errorf("undeclared role/is_blocked not kept: %v", body)
	}
	if got := strings.Join(listOf(body["auto_groups"]), ","); got != "g2,g1" {
		t.Errorf("auto_groups = %s, want IDs g2,g1", got)
	}
	// Service users are not covered: not listed as undeclared either.
	for _, u := range p.Unmanaged {
		if u.Kind == "users" && u.Key == "" {
			t.Errorf("service user listed: %+v", u)
		}
	}
}

func TestUserGroupRemovalIsRisky(t *testing.T) {
	p := mustPlan(t, userSnapshot(t), desired(t, `{"users": [{"id": "u-a", "auto_groups": []}]}`))
	if op := opFor(p, "users", "u-a"); op == nil || len(op.Risky) == 0 {
		t.Fatalf("group removal not risky: %+v", op)
	}
}

func TestUserChangesTheServerRefusesFailThePlan(t *testing.T) {
	s := userSnapshot(t)
	cases := map[string]string{
		"owner role change":        `{"users": [{"id": "u-owner", "role": "user"}]}`,
		"owner blocked":            `{"users": [{"id": "u-owner", "is_blocked": true}]}`,
		"assign owner":             `{"users": [{"id": "u-a", "role": "owner"}]}`,
		"own role change":          `{"users": [{"id": "u-me", "role": "user"}]}`,
		"All group":                `{"users": [{"id": "u-a", "auto_groups": ["All"]}]}`,
		"user who never logged in": `{"users": [{"id": "u-ghost"}]}`,
	}
	for name, src := range cases {
		if _, err := MakePlan(s, desired(t, src)); err == nil {
			t.Errorf("%s: want plan error", name)
		}
	}
	// The owner's groups can be managed (the server allows admins to), and a
	// matching role declaration is fine.
	p := mustPlan(t, s, desired(t, `{"users": [{"id": "u-owner", "role": "owner", "auto_groups": ["admins", "vpn_access"]}]}`))
	if op := opFor(p, "users", "u-owner"); op == nil || op.Action != "update" {
		t.Fatalf("want owner groups update, got %+v", p.Ops)
	}
}

func TestPendingUserIsApprovedThenUpdated(t *testing.T) {
	p := mustPlan(t, userSnapshot(t), desired(t, `{"users": [{"id": "u-new", "auto_groups": ["vpn_access"]}]}`))
	if len(p.Ops) != 2 || p.Ops[0].Action != "approve" || p.Ops[1].Action != "update" {
		t.Fatalf("want approve then update, got %+v", p.Ops)
	}
	if p.Ops[1].Body["is_blocked"] != false {
		t.Errorf("update after approval would block the user again: %v", p.Ops[1].Body)
	}
	// Undeclared pending users are only reported.
	p = mustPlan(t, userSnapshot(t), desired(t, `{"users": []}`))
	if len(p.Ops) != 0 {
		t.Errorf("undeclared user changed: %+v", p.Ops)
	}
}

func TestJWTGroupSyncToggleIsRisky(t *testing.T) {
	s := snapshot(t, map[string]string{"account": `[{"id": "acc", "settings": {"jwt_groups_enabled": true}}]`})
	p := mustPlan(t, s, desired(t, `{"account": {"settings": {"jwt_groups_enabled": false}}}`))
	if op := opFor(p, "account", ""); op == nil || len(op.Risky) == 0 {
		t.Fatalf("turning off the group sync is not risky: %+v", op)
	}
}

func tokenSnapshot(t *testing.T, expires string) *Snapshot {
	return snapshot(t, map[string]string{
		"service_users": `[
			{"id": "s-me", "name": "nb-nix", "role": "admin", "auto_groups": [], "is_blocked": false, "is_service_user": true, "is_current": true},
			{"id": "s-ci", "name": "ci", "role": "auditor", "auto_groups": [], "is_blocked": false, "is_service_user": true, "is_current": false}
		]`,
		"tokens": `[
			{"_parent": "s-me", "_parent_name": "nb-nix", "id": "t-me", "name": "main", "expiration_date": "` + expires + `"},
			{"_parent": "s-ci", "_parent_name": "ci", "id": "t-ci", "name": "weekly", "expiration_date": "` + expires + `"},
			{"_parent": "s-ci", "_parent_name": "ci", "id": "t-old", "name": "old", "expiration_date": "` + expires + `"}
		]`,
	})
}

func TestTokenRenewsBeforeExpiry(t *testing.T) {
	soon := time.Now().Add(10 * 24 * time.Hour).UTC().Format(time.RFC3339)
	s := tokenSnapshot(t, soon)
	sink := `"sink": {"command": ["true"]}`
	p := mustPlan(t, s, desired(t, `{"tokens": [{"user": "ci", "name": "weekly", "renew_before_days": 30, `+sink+`}]}`))
	op := opFor(p, "tokens", "ci/weekly")
	if op == nil || op.Action != "replace" || len(op.Risky) == 0 || op.Parent != "s-ci" {
		t.Fatalf("want risky replace under s-ci, got %+v", p.Ops)
	}
	p = mustPlan(t, s, desired(t, `{"tokens": [{"user": "ci", "name": "weekly", "renew_before_days": 5, `+sink+`}]}`))
	if len(p.Ops) != 0 {
		t.Fatalf("token renewed too early: %+v", p.Ops)
	}
	if _, err := MakePlan(s, desired(t, `{"tokens": [{"user": "ci", "name": "weekly", "renew_before_days": 30}]}`)); err == nil {
		t.Error("renewal without a sink must fail the plan")
	}
}

func TestServiceUserAndTokenCreateInOneApply(t *testing.T) {
	s := tokenSnapshot(t, time.Now().Add(100*24*time.Hour).UTC().Format(time.RFC3339))
	p := mustPlan(t, s, desired(t, `{
		"service_users": [{"name": "deployer", "role": "admin"}],
		"tokens": [{"user": "deployer", "name": "main", "expires_in": 90, "sink": {"command": ["true"]}}]
	}`))
	if len(p.Ops) != 2 || p.Ops[0].Kind != "service_users" || p.Ops[1].Kind != "tokens" {
		t.Fatalf("want service user then token, got %+v", p.Ops)
	}
	body := KindByName("service_users").CreateBody(p.Ops[0].Body)
	if body["is_service_user"] != true || body["name"] != "deployer" {
		t.Errorf("service user create body: %v", body)
	}
	a := &applier{snap: s, created: map[string]map[string]string{}}
	tb, err := a.body(KindByName("tokens"), &p.Ops[1], true)
	if err != nil {
		t.Fatal(err)
	}
	if tb = KindByName("tokens").CreateBody(tb); len(tb) != 2 || tb["expires_in"] == nil {
		t.Errorf("token create body must be name and expires_in: %v", tb)
	}
}

func TestPruneNeverDeletesTheTokenNbNixUses(t *testing.T) {
	s := tokenSnapshot(t, time.Now().Add(100*24*time.Hour).UTC().Format(time.RFC3339))
	p := mustPlan(t, s, desired(t, `{"prune": {"tokens": true}, "service_users": [{"name": "nb-nix"}, {"name": "ci"}]}`))
	var deleted []string
	for _, op := range p.Ops {
		if op.Action == "delete" {
			deleted = append(deleted, op.Key)
		}
	}
	if strings.Join(deleted, ",") != "ci/old,ci/weekly" && strings.Join(deleted, ",") != "ci/weekly,ci/old" {
		t.Errorf("deleted %v, want only ci's tokens", deleted)
	}
}
