package engine

import (
	"strings"
	"testing"
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

func TestUserGroupsAreSetByEmailWithNames(t *testing.T) {
	s := userSnapshot(t)
	p := mustPlan(t, s, desired(t, `{"users": [{"email": "a@example.com", "auto_groups": ["vpn_access", "admins"]}]}`))
	op := opFor(p, "users", "a@example.com")
	if op == nil || op.Action != "update" || len(op.Risky) != 0 {
		t.Fatalf("want one plain update, got %+v", p.Ops)
	}
	a := &applier{snap: s, created: map[string]map[string]string{}}
	body, err := a.body(KindByName("users"), op, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, sent := body["email"]; sent {
		t.Error("email must not be sent")
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
	p := mustPlan(t, userSnapshot(t), desired(t, `{"users": [{"email": "a@example.com", "auto_groups": []}]}`))
	if op := opFor(p, "users", "a@example.com"); op == nil || len(op.Risky) == 0 {
		t.Fatalf("group removal not risky: %+v", op)
	}
}

func TestUserChangesTheServerRefusesFailThePlan(t *testing.T) {
	s := userSnapshot(t)
	cases := map[string]string{
		"owner role change":        `{"users": [{"email": "owner@example.com", "role": "user"}]}`,
		"owner blocked":            `{"users": [{"email": "owner@example.com", "is_blocked": true}]}`,
		"assign owner":             `{"users": [{"email": "a@example.com", "role": "owner"}]}`,
		"own role change":          `{"users": [{"email": "me@example.com", "role": "user"}]}`,
		"All group":                `{"users": [{"email": "a@example.com", "auto_groups": ["All"]}]}`,
		"user who never logged in": `{"users": [{"email": "ghost@example.com"}]}`,
	}
	for name, src := range cases {
		if _, err := MakePlan(s, desired(t, src)); err == nil {
			t.Errorf("%s: want plan error", name)
		}
	}
	// The owner's groups can be managed (the server allows admins to), and a
	// matching role declaration is fine.
	p := mustPlan(t, s, desired(t, `{"users": [{"email": "owner@example.com", "role": "owner", "auto_groups": ["admins", "vpn_access"]}]}`))
	if op := opFor(p, "users", "owner@example.com"); op == nil || op.Action != "update" {
		t.Fatalf("want owner groups update, got %+v", p.Ops)
	}
}

func TestPendingUserIsApprovedThenUpdated(t *testing.T) {
	p := mustPlan(t, userSnapshot(t), desired(t, `{"users": [{"email": "new@example.com", "auto_groups": ["vpn_access"]}]}`))
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
