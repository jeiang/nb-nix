//go:build server

// Package servertest runs plan/apply against a real NetBird management
// server. Run it with `just server-test`, which starts a throwaway server and
// sets NB_TEST_MANAGEMENT_URL and NB_TEST_TOKEN.
package servertest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jeiang/nb-nix/internal/engine"
	"github.com/jeiang/nb-nix/internal/nbapi"
)

func client(t *testing.T) *nbapi.Client {
	t.Helper()
	url, token := os.Getenv("NB_TEST_MANAGEMENT_URL"), os.Getenv("NB_TEST_TOKEN")
	if url == "" || token == "" {
		t.Skip("NB_TEST_MANAGEMENT_URL and NB_TEST_TOKEN not set")
	}
	return nbapi.New(url, token)
}

func plan(t *testing.T, c *nbapi.Client, desired string) *engine.Plan {
	t.Helper()
	want, err := engine.ParseDesired([]byte(desired))
	if err != nil {
		t.Fatal(err)
	}
	snap, err := engine.Fetch(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	p, err := engine.MakePlan(snap, want)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func apply(t *testing.T, c *nbapi.Client, p *engine.Plan, risky bool) error {
	t.Helper()
	// Round-trip the plan through JSON, as `nb-nix plan -o` / `apply FILE` do.
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var fromFile engine.Plan
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.UseNumber()
	if err := dec.Decode(&fromFile); err != nil {
		t.Fatal(err)
	}
	return engine.Apply(context.Background(), c, &fromFile, engine.ApplyOptions{AllowRisky: risky, Log: io.Discard})
}

func describe(p *engine.Plan) string {
	var b strings.Builder
	p.Render(&b)
	return b.String()
}

func requireNoChanges(t *testing.T, c *nbapi.Client, desired string) {
	t.Helper()
	if p := plan(t, c, desired); len(p.Ops) != 0 {
		t.Fatalf("plan after apply is not empty:\n%s", describe(p))
	}
}

// fullConfig declares one object of every kind that needs no peers or proxy.
func fullConfig(sinkFile, ports string) string {
	return `{
	"account": {"settings": {"peer_login_expiration": 43200, "peer_login_expiration_enabled": true}},
	"posture_checks": [{"name": "recent-client", "description": "", "checks": {"nb_version_check": {"min_version": "0.60.0"}}}],
	"groups": [{"name": "admins"}, {"name": "servers"}, {"name": "routers"}],
	"networks": [{"name": "lab", "description": "lab net"}],
	"network_resources": [{"network": "lab", "name": "lab-subnet", "address": "10.10.0.0/24", "enabled": true, "groups": ["servers"]}],
	"network_routers": [{"network": "lab", "peer_groups": ["routers"], "metric": 100, "masquerade": true, "enabled": true}],
	"nameserver_groups": [{"name": "quad9", "description": "", "nameservers": [{"ip": "9.9.9.9", "ns_type": "udp", "port": 53}],
		"enabled": true, "groups": ["admins"], "primary": true, "domains": [], "search_domains_enabled": false}],
	"dns_settings": {"disabled_management_groups": ["routers"]},
	"policies": [{"name": "admin-ssh", "enabled": true, "source_posture_checks": ["recent-client"],
		"rules": [{"name": "admin-ssh", "enabled": true, "action": "accept", "bidirectional": false, "protocol": "tcp",
		           "ports": ` + ports + `, "sources": ["admins"], "destinations": ["servers"]}]},
	             {"name": "admin-lab", "enabled": true,
		"rules": [{"name": "admin-lab", "enabled": true, "action": "accept", "bidirectional": false, "protocol": "all",
		           "sources": ["admins"], "destinationResource": {"id": "lab-subnet", "type": "subnet"}}]}],
	"setup_keys": [{"name": "servers", "type": "reusable", "expires_in": 86400, "auto_groups": ["servers"], "usage_limit": 0,
		"ephemeral": false, "sink": {"command": ["sh", "-c", "cat > ` + sinkFile + `"], "format": "json"}}]
}`
}

func TestPlanApplyCycle(t *testing.T) {
	c := client(t)
	ctx := context.Background()
	if err := c.CheckVersion(ctx); err != nil {
		t.Fatal(err)
	}
	sink := filepath.Join(t.TempDir(), "key.json")
	cfg := fullConfig(sink, `["22"]`)

	t.Run("create everything, then no changes", func(t *testing.T) {
		p := plan(t, c, cfg)
		if got := p.Counts()["create"]; got < 10 {
			t.Fatalf("want creates, got:\n%s", describe(p))
		}
		if err := apply(t, c, p, false); err != nil {
			t.Fatal(err)
		}
		requireNoChanges(t, c, cfg)
		var key string
		b, err := os.ReadFile(sink)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(b, &key); err != nil || len(key) < 16 {
			t.Fatalf("sink did not get the setup key as a JSON string: %q", b)
		}
	})

	t.Run("change one field", func(t *testing.T) {
		changed := fullConfig(sink, `["22", "2222"]`)
		p := plan(t, c, changed)
		if len(p.Ops) != 1 || p.Ops[0].Kind != "policies" || p.Ops[0].Action != "update" {
			t.Fatalf("want one policy update, got:\n%s", describe(p))
		}
		if err := apply(t, c, p, false); err != nil {
			t.Fatal(err)
		}
		requireNoChanges(t, c, changed)
		cfg = changed
	})

	t.Run("rule targets switch between resource and groups", func(t *testing.T) {
		toGroups := strings.Replace(cfg, `"destinationResource": {"id": "lab-subnet", "type": "subnet"}`, `"destinations": ["servers"]`, 1)
		for _, want := range []string{toGroups, cfg} {
			if err := apply(t, c, plan(t, c, want), false); err != nil {
				t.Fatal(err)
			}
			requireNoChanges(t, c, want)
		}
	})

	t.Run("stale plan is refused", func(t *testing.T) {
		p := plan(t, c, fullConfig(sink, `["22"]`))
		// Someone edits the policy between plan and apply.
		snap, err := engine.Fetch(ctx, c)
		if err != nil {
			t.Fatal(err)
		}
		var id string
		for _, o := range snap.Objects["policies"] {
			if o.Raw["name"] == "admin-ssh" {
				id = o.ID
				body := o.Raw
				body["description"] = "edited out of band"
				rules := body["rules"].([]any)
				r := rules[0].(map[string]any)
				r["sources"] = idList(r["sources"])
				r["destinations"] = idList(r["destinations"])
				if err := c.Do(ctx, "PUT", "/policies/"+id, body, nil); err != nil {
					t.Fatal(err)
				}
			}
		}
		err = apply(t, c, p, false)
		if err == nil || !strings.Contains(err.Error(), "stale") {
			t.Fatalf("want stale plan error, got %v", err)
		}
		// The description is not declared, so the out-of-band value stays.
		requireNoChanges(t, c, cfg)
		snap, err = engine.Fetch(ctx, c)
		if err != nil {
			t.Fatal(err)
		}
		for _, o := range snap.Objects["policies"] {
			if o.ID == id && o.Raw["description"] != "edited out of band" {
				t.Fatalf("undeclared field was reverted: %v", o.Raw["description"])
			}
		}
	})

	t.Run("risky change needs the flag", func(t *testing.T) {
		risky := strings.Replace(cfg, `"peer_login_expiration": 43200`, `"peer_login_expiration": 43200, "network_range": "100.70.0.0/16"`, 1)
		p := plan(t, c, risky)
		if err := apply(t, c, p, false); err == nil || !strings.Contains(err.Error(), "--allow-risky") {
			t.Fatalf("want refusal, got %v", err)
		}
		if err := apply(t, c, plan(t, c, risky), true); err != nil {
			t.Fatal(err)
		}
		requireNoChanges(t, c, risky)
		cfg = risky
	})

	t.Run("prune deletes only undeclared unprotected objects", func(t *testing.T) {
		if err := c.Do(ctx, "POST", "/groups", map[string]any{"name": "stray"}, nil); err != nil {
			t.Fatal(err)
		}
		pruning := strings.Replace(cfg, `"account":`, `"prune": {"groups": true}, "account":`, 1)
		p := plan(t, c, pruning)
		if len(p.Ops) != 1 || p.Ops[0].Action != "delete" || p.Ops[0].Key != "stray" {
			t.Fatalf("want only the stray group deleted, got:\n%s", describe(p))
		}
		if err := apply(t, c, p, true); err != nil {
			t.Fatal(err)
		}
		requireNoChanges(t, c, pruning)
	})

	t.Run("failed sink leaves no new key", func(t *testing.T) {
		failing := strings.Replace(cfg, `"setup_keys": [`,
			`"setup_keys": [{"name": "doomed", "type": "one-off", "expires_in": 3600, "auto_groups": [], "usage_limit": 1, "sink": {"command": ["false"]}},`, 1)
		err := apply(t, c, plan(t, c, failing), false)
		if err == nil || !strings.Contains(err.Error(), "sink") {
			t.Fatalf("want sink failure, got %v", err)
		}
		snap, err := engine.Fetch(ctx, c)
		if err != nil {
			t.Fatal(err)
		}
		for _, o := range snap.Objects["setup_keys"] {
			if o.Raw["name"] == "doomed" {
				t.Fatalf("setup key left behind: %v", o.Raw)
			}
		}
	})

	t.Run("users are adopted by ID", func(t *testing.T) {
		var member struct{ ID string }
		if err := c.Do(ctx, "POST", "/users", map[string]any{
			"email": "member@nb-nix.test", "name": "Member", "role": "user", "auto_groups": []string{}, "is_service_user": false,
		}, &member); err != nil {
			t.Fatal(err)
		}
		users := `{"users": [{"id": "` + member.ID + `", "auto_groups": ["admins", "servers"]}]}`
		p := plan(t, c, users)
		if len(p.Ops) != 1 || p.Ops[0].Kind != "users" || p.Ops[0].Action != "update" || p.Ops[0].Label != "Member" {
			t.Fatalf("want one labeled user update, got:\n%s", describe(p))
		}
		if err := apply(t, c, p, false); err != nil {
			t.Fatal(err)
		}
		requireNoChanges(t, c, users)
		// The setup owner is the token's own user and the owner: role changes are refused at plan time.
		snap, err := engine.Fetch(ctx, c)
		if err != nil {
			t.Fatal(err)
		}
		var ownerID string
		for _, o := range snap.Objects["users"] {
			if o.Raw["role"] == "owner" {
				ownerID = o.ID
			}
		}
		want, err := engine.ParseDesired([]byte(`{"users": [{"id": "` + ownerID + `", "role": "user"}]}`))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := engine.MakePlan(snap, want); err == nil || !strings.Contains(err.Error(), "owner") {
			t.Fatalf("want owner refusal, got %v", err)
		}
	})

	t.Run("service user tokens go through the sink and renew", func(t *testing.T) {
		tokenFile := filepath.Join(t.TempDir(), "token")
		cfgFor := func(renew int) string {
			return fmt.Sprintf(`{
				"service_users": [{"name": "deployer", "role": "admin"}],
				"tokens": [{"user": "deployer", "name": "main", "expires_in": 30, "renew_before_days": %d,
				            "sink": {"command": ["sh", "-c", "cat > %s"], "format": "raw"}}]
			}`, renew, tokenFile)
		}
		tokenWorks := func() (string, bool) {
			b, err := os.ReadFile(tokenFile)
			if err != nil {
				t.Fatal(err)
			}
			_, err = nbapi.New(os.Getenv("NB_TEST_MANAGEMENT_URL"), string(b)).ServerVersion(ctx)
			return string(b), err == nil
		}
		p := plan(t, c, cfgFor(0))
		if p.Counts()["create"] != 2 {
			t.Fatalf("want service user and token created, got:\n%s", describe(p))
		}
		if err := apply(t, c, p, false); err != nil {
			t.Fatal(err)
		}
		first, ok := tokenWorks()
		if !ok {
			t.Fatal("token from the sink does not authenticate")
		}
		requireNoChanges(t, c, cfgFor(0))
		// A 30-day token renewed 60 days ahead is replaced on every plan.
		p = plan(t, c, cfgFor(60))
		if len(p.Ops) != 1 || p.Ops[0].Action != "replace" {
			t.Fatalf("want token replacement, got:\n%s", describe(p))
		}
		if err := apply(t, c, p, true); err != nil {
			t.Fatal(err)
		}
		second, ok := tokenWorks()
		if !ok || second == first {
			t.Fatal("replacement token missing or not working")
		}
		if _, err := nbapi.New(os.Getenv("NB_TEST_MANAGEMENT_URL"), first).ServerVersion(ctx); err == nil {
			t.Error("old token still works after replacement")
		}

		// The deployer renews its own token while also changing a setting
		// that applies after tokens: the old token must stay valid until the end.
		self := nbapi.New(os.Getenv("NB_TEST_MANAGEMENT_URL"), second)
		renewSelf := strings.Replace(cfgFor(60), `"service_users":`,
			`"account": {"settings": {"peer_login_expiration": 7200}}, "service_users":`, 1)
		p = plan(t, self, renewSelf)
		if len(p.Ops) != 2 || p.Ops[0].Kind != "tokens" || p.Ops[1].Kind != "account" {
			t.Fatalf("want token replace then account update, got:\n%s", describe(p))
		}
		if err := apply(t, self, p, true); err != nil {
			t.Fatalf("self-renewal broke the apply: %v", err)
		}
		third, ok := tokenWorks()
		if !ok || third == second {
			t.Fatal("self-renewed token missing or not working")
		}
		if _, err := self.ServerVersion(ctx); err == nil {
			t.Error("token used for the apply still works after it was replaced")
		}

		// An auditor may not list other service users' tokens: its plan
		// reports them as unchecked instead of failing or creating them.
		auditorFile := filepath.Join(t.TempDir(), "auditor")
		withAuditor := strings.Replace(cfgFor(0), `"tokens": [`, fmt.Sprintf(
			`"tokens": [{"user": "ci", "name": "ci", "sink": {"command": ["sh", "-c", "cat > %s"], "format": "raw"}}, `, auditorFile), 1)
		withAuditor = strings.Replace(withAuditor, `"service_users": [`, `"service_users": [{"name": "ci", "role": "auditor"}, `, 1)
		p = plan(t, c, withAuditor)
		if err := apply(t, c, p, false); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(auditorFile)
		if err != nil {
			t.Fatal(err)
		}
		p = plan(t, nbapi.New(os.Getenv("NB_TEST_MANAGEMENT_URL"), string(b)), withAuditor)
		if len(p.Ops) != 0 || !slices.Contains(p.Unchecked, `tokens of "deployer"`) {
			t.Fatalf("want no changes and deployer's tokens unchecked, got:\n%s\nunchecked: %v", describe(p), p.Unchecked)
		}
	})
}

func idList(v any) []any {
	var out []any
	for _, e := range v.([]any) {
		out = append(out, e.(map[string]any)["id"])
	}
	return out
}
