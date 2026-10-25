package engine

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/netbirdio/netbird/shared/management/http/api"
)

// Ref names a field holding IDs of other objects. In desired documents the
// same field holds the target objects' names.
type Ref struct {
	Path string
	Kind string
	// KindFrom picks the target kind from the object containing the value,
	// for fields whose target depends on a sibling "type" field.
	KindFrom func(parent Doc) string
}

// Secret is a write-only field. Desired documents set "<Key>_file" in the
// object at Container instead; the file is read only when the secret is sent.
type Secret struct {
	Container string // path to the object, may contain [] segments
	Key       string
}

// Sink describes a secret the server returns once, at creation; it is handed
// to the object's "sink" command and never stored.
type Sink struct {
	Secret string // response path of the secret
	ID     string // response path of the new object's ID
}

// Kind describes one kind of NetBird object.
type Kind struct {
	Name      string
	Singleton bool
	// Child kinds live under an object of kind Parent. Desired documents name
	// the parent in ParentField; the Nix module nests them under ChildField.
	Parent      string
	ParentField string
	ChildField  string
	// ReqType is the update request type; live documents are projected onto it.
	ReqType reflect.Type
	Refs    []Ref
	Secrets []Secret
	// Immutable fields cannot be updated: a change replaces the object.
	Immutable []string
	// DesiredOnly keys exist in desired documents but are never sent or compared.
	DesiredOnly []string
	// CreateOnly keys are sent when creating and never compared.
	CreateOnly []string
	// Hidden fields cannot be declared (server-managed or read-only); import skips them.
	Hidden []string
	// Collection returns the list/create path; Item the update/delete path.
	Collection func(parentID string) string
	Item       func(parentID, id string) string
	// Transform turns a live response document into request shape before projection.
	Transform func(raw Doc) Doc
	// Key identifies an object (in named form, including ParentField).
	Key func(d Doc) string
	// Protected objects are never deleted by pruning. parent is the raw parent
	// object for child kinds.
	Protected func(raw, parent Doc) bool
	// Risky returns reasons a change from old to new needs --allow-risky.
	Risky func(old, new Doc) []string
	// Check returns reasons an update from old to new cannot be made at all
	// (the server would refuse it); they fail the plan.
	Check func(raw, old, new Doc) []string
	// Renew returns a reason to replace an otherwise unchanged object.
	Renew func(raw, want Doc) string
	// CreateBody and UpdateBody build request bodies from a named document.
	CreateBody func(d Doc) Doc
	UpdateBody func(d Doc) Doc
	Sink       *Sink
	// Skip drops live objects that this kind does not cover.
	Skip func(raw Doc) bool
	// Keep copies identifying response fields that the request type lacks
	// into named documents.
	Keep     []string
	NoCreate bool
	NoDelete bool
	// Quiet kinds do not list undeclared live objects in plans.
	Quiet bool
}

func collection(p string) func(string) string { return func(string) string { return p } }
func item(p string) func(string, string) string {
	return func(_, id string) string { return p + "/" + id }
}

func nameKey(d Doc) string { return str(d["name"]) }

func childKey(field string) func(Doc) string {
	return func(d Doc) string { return str(d[field]) + "/" + str(d["name"]) }
}

func routerKey(d Doc) string {
	target := str(d["peer"])
	if target == "" {
		var groups []string
		if l, ok := d["peer_groups"].([]any); ok {
			for _, g := range l {
				groups = append(groups, str(g))
			}
		}
		sort.Strings(groups)
		target = "groups:" + strings.Join(groups, ",")
	} else {
		target = "peer:" + target
	}
	return str(d["network"]) + "/" + target
}

// idsOf turns a list of objects with an "id" into a list of IDs.
func idsOf(v any) any {
	l, ok := v.([]any)
	if !ok {
		return v
	}
	out := make([]any, 0, len(l))
	for _, e := range l {
		if m, ok := e.(map[string]any); ok {
			out = append(out, m["id"])
		} else {
			out = append(out, e)
		}
	}
	return out
}

func resourceKind(parent Doc) string {
	if str(parent["type"]) == "peer" {
		return "peers"
	}
	return "network_resources"
}

func serviceTargetKind(parent Doc) string {
	switch str(parent["target_type"]) {
	case "peer":
		return "peers"
	case "host", "domain", "subnet":
		return "network_resources"
	}
	return "" // cluster targets are not references
}

func changed(old, new Doc, path string) bool { return !equalJSON(get(old, path), get(new, path)) }

func boolAt(d Doc, path string) bool { b, _ := get(d, path).(bool); return b }

func isCurrent(raw Doc) bool { b, _ := raw["is_current"].(bool); return b }

func isServiceUser(raw Doc) bool { b, _ := raw["is_service_user"].(bool); return b }

// userCheck lists user updates the server refuses (validateUserUpdate).
func userCheck(raw, old, new Doc) []string {
	var errs []string
	roleOrBlock := changed(old, new, "role") || changed(old, new, "is_blocked")
	if isCurrent(raw) && roleOrBlock {
		errs = append(errs, "the API token belongs to this user; the server refuses changes to its own role or blocked state")
	}
	if str(raw["role"]) == "owner" && roleOrBlock {
		errs = append(errs, "the owner's role and blocked state can only be changed by the owner (dashboard)")
	}
	if str(raw["role"]) != "owner" && str(new["role"]) == "owner" {
		errs = append(errs, "only the owner can transfer ownership (dashboard); leave role undeclared until then")
	}
	for _, g := range listOf(get(new, "auto_groups")) {
		if g == "All" {
			errs = append(errs, "the All group cannot be assigned to users")
		}
	}
	return errs
}

func userRisky(old, new Doc) []string {
	var r []string
	if changed(old, new, "role") {
		r = append(r, fmt.Sprintf("changes the role from %s to %s", str(old["role"]), str(new["role"])))
	}
	if !boolAt(old, "is_blocked") && boolAt(new, "is_blocked") {
		r = append(r, "blocks the user")
	}
	if removed := setMinus(get(old, "auto_groups"), get(new, "auto_groups")); len(removed) > 0 {
		r = append(r, "removes groups from the user and their peers: "+strings.Join(removed, ", "))
	}
	return r
}

// Kinds lists every managed kind in apply order: an object is created or
// updated after everything it can reference. Deletes run in reverse order.
var Kinds = []*Kind{
	{
		Name:       "posture_checks",
		ReqType:    reflect.TypeFor[api.PostureCheckUpdate](),
		Collection: collection("/posture-checks"),
		Item:       item("/posture-checks"),
		Key:        nameKey,
	},
	{
		Name:       "groups",
		Hidden:     []string{"resources"}, // set through network resources' groups
		ReqType:    reflect.TypeFor[api.GroupRequest](),
		Collection: collection("/groups"),
		Item:       item("/groups"),
		Refs:       []Ref{{Path: "peers[]", Kind: "peers"}},
		Transform: func(raw Doc) Doc {
			raw["peers"] = idsOf(raw["peers"])
			return raw
		},
		Key: nameKey,
		Protected: func(raw, _ Doc) bool {
			issued := str(raw["issued"])
			return str(raw["name"]) == "All" || (issued != "" && issued != "api")
		},
		Risky: func(old, new Doc) []string {
			removed := setMinus(get(old, "peers"), get(new, "peers"))
			if len(removed) > 0 {
				return []string{"removes peers from the group: " + strings.Join(removed, ", ")}
			}
			return nil
		},
	},
	{
		Name:       "networks",
		ReqType:    reflect.TypeFor[api.NetworkRequest](),
		Collection: collection("/networks"),
		Item:       item("/networks"),
		Key:        nameKey,
	},
	{
		Name:        "network_resources",
		Parent:      "networks",
		ParentField: "network",
		ChildField:  "resources",
		ReqType:     reflect.TypeFor[api.NetworkResourceRequest](),
		Collection:  func(p string) string { return "/networks/" + p + "/resources" },
		Item:        func(p, id string) string { return "/networks/" + p + "/resources/" + id },
		Refs:        []Ref{{Path: "groups[]", Kind: "groups"}},
		DesiredOnly: []string{"network"},
		Transform: func(raw Doc) Doc {
			raw["groups"] = idsOf(raw["groups"])
			return raw
		},
		Key: childKey("network"),
	},
	{
		Name:        "network_routers",
		Parent:      "networks",
		ParentField: "network",
		ChildField:  "routers",
		ReqType:     reflect.TypeFor[api.NetworkRouterRequest](),
		Collection:  func(p string) string { return "/networks/" + p + "/routers" },
		Item:        func(p, id string) string { return "/networks/" + p + "/routers/" + id },
		Refs:        []Ref{{Path: "peer", Kind: "peers"}, {Path: "peer_groups[]", Kind: "groups"}},
		DesiredOnly: []string{"network"},
		Key:         routerKey,
	},
	{
		Name:       "nameserver_groups",
		ReqType:    reflect.TypeFor[api.NameserverGroupRequest](),
		Collection: collection("/dns/nameservers"),
		Item:       item("/dns/nameservers"),
		Refs:       []Ref{{Path: "groups[]", Kind: "groups"}},
		Key:        nameKey,
	},
	{
		Name:       "dns_settings",
		Singleton:  true,
		ReqType:    reflect.TypeFor[api.DNSSettings](),
		Collection: collection("/dns/settings"),
		Item:       func(string, string) string { return "/dns/settings" },
		Refs:       []Ref{{Path: "disabled_management_groups[]", Kind: "groups"}},
		Key:        func(Doc) string { return "" },
		NoCreate:   true,
		NoDelete:   true,
	},
	{
		Name:       "policies",
		Hidden:     []string{"rules[].id"},
		ReqType:    reflect.TypeFor[api.PolicyCreate](),
		Collection: collection("/policies"),
		Item:       item("/policies"),
		Refs: []Ref{
			{Path: "source_posture_checks[]", Kind: "posture_checks"},
			{Path: "rules[].sources[]", Kind: "groups"},
			{Path: "rules[].destinations[]", Kind: "groups"},
			{Path: "rules[].authorized_groups{}", Kind: "groups"},
			{Path: "rules[].sourceResource.id", KindFrom: resourceKind},
			{Path: "rules[].destinationResource.id", KindFrom: resourceKind},
		},
		Transform: func(raw Doc) Doc {
			if rules, ok := raw["rules"].([]any); ok {
				for _, r := range rules {
					if rm, ok := r.(map[string]any); ok {
						rm["sources"] = idsOf(rm["sources"])
						rm["destinations"] = idsOf(rm["destinations"])
					}
				}
			}
			return raw
		},
		Key: nameKey,
		Protected: func(raw, _ Doc) bool {
			return strings.HasPrefix(str(raw["name"]), "Temporary access policy for peer ")
		},
	},
	{
		Name:        "setup_keys",
		ReqType:     reflect.TypeFor[setupKeyDoc](),
		Collection:  collection("/setup-keys"),
		Item:        item("/setup-keys"),
		Refs:        []Ref{{Path: "auto_groups[]", Kind: "groups"}},
		Immutable:   []string{"type", "usage_limit", "ephemeral", "allow_extra_dns_labels"},
		DesiredOnly: []string{"sink"},
		CreateOnly:  []string{"expires_in"},
		Sink:        &Sink{Secret: "key", ID: "id"},
		Key:         nameKey,
		Risky: func(old, new Doc) []string {
			if !boolAt(old, "revoked") && boolAt(new, "revoked") {
				return []string{"revokes the setup key"}
			}
			return nil
		},
		CreateBody: func(d Doc) Doc {
			body := pick(d, "name", "type", "expires_in", "auto_groups", "usage_limit", "ephemeral", "allow_extra_dns_labels")
			defaults(body, Doc{"expires_in": 0, "auto_groups": []any{}})
			return body
		},
		UpdateBody: func(d Doc) Doc {
			groups := d["auto_groups"]
			if groups == nil {
				groups = []any{}
			}
			return Doc{"revoked": boolAt(d, "revoked"), "auto_groups": groups}
		},
	},
	{
		Name:       "services",
		ReqType:    reflect.TypeFor[api.ServiceRequest](),
		Collection: collection("/reverse-proxies/services"),
		Item:       item("/reverse-proxies/services"),
		Refs: []Ref{
			{Path: "access_groups[]", Kind: "groups"},
			{Path: "auth.bearer_auth.distribution_groups[]", Kind: "groups"},
			{Path: "targets[].target_id", KindFrom: serviceTargetKind},
		},
		Secrets: []Secret{
			{Container: "auth.password_auth", Key: "password"},
			{Container: "auth.pin_auth", Key: "pin"},
			{Container: "auth.header_auths[]", Key: "value"},
		},
		Key: nameKey,
	},
	{
		Name:       "peers",
		ReqType:    reflect.TypeFor[api.PeerRequest](),
		Collection: collection("/peers"),
		Item:       item("/peers"),
		Key:        nameKey,
		NoCreate:   true,
		NoDelete:   true,
		Quiet:      true,
		Risky: func(old, new Doc) []string {
			var r []string
			if changed(old, new, "ip") || changed(old, new, "ipv6") {
				r = append(r, "changes the peer's overlay address")
			}
			if boolAt(old, "approval_required") != boolAt(new, "approval_required") && boolAt(new, "approval_required") {
				r = append(r, "requires approval for the peer")
			}
			return r
		},
	},
	{
		// Human users, adopted after their first login.
		Name:        "users",
		ReqType:     reflect.TypeFor[api.UserRequest](),
		Collection:  collection("/users"),
		Item:        item("/users"),
		Refs:        []Ref{{Path: "auto_groups[]", Kind: "groups"}},
		Keep:        []string{"email"},
		DesiredOnly: []string{"email"},
		Skip:        isServiceUser,
		Key:         func(d Doc) string { return strings.ToLower(str(d["email"])) },
		NoCreate:    true,
		NoDelete:    true,
		Check:       userCheck,
		Risky:       userRisky,
	},
	{
		Name:       "service_users",
		ReqType:    reflect.TypeFor[api.UserRequest](),
		Collection: collection("/users"),
		Item:       item("/users"),
		Refs:       []Ref{{Path: "auto_groups[]", Kind: "groups"}},
		Keep:       []string{"name"},
		CreateOnly: []string{"name"},
		Skip:       func(raw Doc) bool { return !isServiceUser(raw) },
		Key:        nameKey,
		Check:      userCheck,
		Risky:      userRisky,
		Protected:  func(raw, _ Doc) bool { return isCurrent(raw) },
		CreateBody: func(d Doc) Doc {
			body := pick(d, "name", "role", "auto_groups")
			defaults(body, Doc{"role": "user", "auto_groups": []any{}})
			body["is_service_user"] = true
			return body
		},
	},
	{
		Name:        "tokens",
		Parent:      "service_users",
		ParentField: "user",
		ChildField:  "tokens",
		ReqType:     reflect.TypeFor[tokenDoc](),
		Collection:  func(p string) string { return "/users/" + p + "/tokens" },
		Item:        func(p, id string) string { return "/users/" + p + "/tokens/" + id },
		Keep:        []string{"expiration_date"},
		DesiredOnly: []string{"user", "sink", "renew_before_days", "expiration_date"},
		Hidden:      []string{"expiration_date"},
		CreateOnly:  []string{"expires_in"},
		Sink:        &Sink{Secret: "plain_token", ID: "personal_access_token.id"},
		Key:         childKey("user"),
		// Never prune the tokens of the user nb-nix runs as: that would lock it out.
		Protected: func(_, parent Doc) bool { return isCurrent(parent) },
		Renew: func(raw, want Doc) string {
			exp, err := time.Parse(time.RFC3339, str(raw["expiration_date"]))
			if err != nil {
				return ""
			}
			days, _ := numberOf(want["renew_before_days"])
			if time.Until(exp) < time.Duration(days)*24*time.Hour {
				return "token expires " + exp.Format(time.DateOnly)
			}
			return ""
		},
		CreateBody: func(d Doc) Doc {
			body := pick(d, "name", "expires_in")
			defaults(body, Doc{"expires_in": 365})
			return body
		},
	},
	{
		Name:       "account",
		Hidden:     []string{"onboarding", "settings.embedded_idp_enabled", "settings.local_auth_disabled"},
		Singleton:  true,
		ReqType:    reflect.TypeFor[api.AccountRequest](),
		Collection: collection("/accounts"),
		Item:       item("/accounts"),
		Refs: []Ref{
			{Path: "settings.ipv6_enabled_groups[]", Kind: "groups"},
			{Path: "settings.peer_expose_groups[]", Kind: "groups"},
			{Path: "settings.extra.network_traffic_logs_groups[]", Kind: "groups"},
		},
		Key:      func(Doc) string { return "" },
		NoCreate: true,
		NoDelete: true,
		Risky: func(old, new Doc) []string {
			var r []string
			for _, p := range []string{"settings.network_range", "settings.network_range_v6"} {
				if changed(old, new, p) {
					r = append(r, p+" change reassigns every peer address")
				}
			}
			if changed(old, new, "settings.dns_domain") {
				r = append(r, "settings.dns_domain change renames every peer")
			}
			if boolAt(old, "settings.extra.peer_approval_enabled") && !boolAt(new, "settings.extra.peer_approval_enabled") {
				r = append(r, "turning off peer approval approves every pending peer")
			}
			if !boolAt(old, "settings.groups_propagation_enabled") && boolAt(new, "settings.groups_propagation_enabled") {
				r = append(r, "turning on groups propagation rewrites group memberships")
			}
			if changed(old, new, "settings.jwt_groups_enabled") {
				r = append(r, "settings.jwt_groups_enabled turns the identity-provider group sync and the jwt_allow_groups login gate on or off")
			}
			return r
		},
	},
}

// setupKeyDoc is the managed view of a setup key: the create request fields
// that the server returns, plus the updatable "revoked".
type setupKeyDoc struct {
	Name                string   `json:"name"`
	Type                string   `json:"type"`
	AutoGroups          []string `json:"auto_groups"`
	UsageLimit          int      `json:"usage_limit"`
	Ephemeral           bool     `json:"ephemeral"`
	AllowExtraDnsLabels bool     `json:"allow_extra_dns_labels"`
	Revoked             bool     `json:"revoked"`
}

// tokenDoc is the managed view of a personal access token: tokens cannot be
// updated, only created (with expires_in) and replaced.
type tokenDoc struct {
	Name string `json:"name"`
}

// KindByName returns the kind with the given name, or nil.
func KindByName(name string) *Kind {
	for _, k := range Kinds {
		if k.Name == name {
			return k
		}
	}
	return nil
}

func pick(d Doc, keys ...string) Doc {
	out := Doc{}
	for _, k := range keys {
		if v, ok := d[k]; ok {
			out[k] = v
		}
	}
	return out
}

func defaults(d, def Doc) {
	for k, v := range def {
		if _, ok := d[k]; !ok {
			d[k] = v
		}
	}
}

// listOf returns the strings in a JSON list.
func listOf(v any) []string {
	l, _ := v.([]any)
	out := make([]string, 0, len(l))
	for _, e := range l {
		out = append(out, str(e))
	}
	return out
}

func setMinus(a, b any) []string {
	in := map[string]bool{}
	if l, ok := b.([]any); ok {
		for _, e := range l {
			in[str(e)] = true
		}
	}
	var out []string
	if l, ok := a.([]any); ok {
		for _, e := range l {
			if !in[str(e)] {
				out = append(out, str(e))
			}
		}
	}
	sort.Strings(out)
	return out
}
