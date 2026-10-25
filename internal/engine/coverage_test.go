package engine

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/netbirdio/netbird/shared/management/http/api"
)

// responseTypes maps each kind to the API type the server returns for it.
var responseTypes = map[string]reflect.Type{
	"posture_checks":    reflect.TypeFor[api.PostureCheck](),
	"groups":            reflect.TypeFor[api.Group](),
	"networks":          reflect.TypeFor[api.Network](),
	"network_resources": reflect.TypeFor[api.NetworkResource](),
	"network_routers":   reflect.TypeFor[api.NetworkRouter](),
	"nameserver_groups": reflect.TypeFor[api.NameserverGroup](),
	"dns_settings":      reflect.TypeFor[api.DNSSettings](),
	"policies":          reflect.TypeFor[api.Policy](),
	"setup_keys":        reflect.TypeFor[api.SetupKey](),
	"services":          reflect.TypeFor[api.Service](),
	"peers":             reflect.TypeFor[api.Peer](),
	"users":             reflect.TypeFor[api.User](),
	"service_users":     reflect.TypeFor[api.User](),
	"tokens":            reflect.TypeFor[api.PersonalAccessToken](),
	"account":           reflect.TypeFor[api.Account](),
}

// TestRequestFieldsComeBackFromServer guards read-merge-PUT: an update sends
// the live object back, so every field of the update request must be readable
// from the response. A NetBird upgrade that adds a request-only field fails
// here instead of resetting that field on every apply.
func TestRequestFieldsComeBackFromServer(t *testing.T) {
	// Request fields the server does not return ("kind.path" -> reason). Empty
	// for the pinned version.
	writeOnly := map[string]string{}
	for _, k := range Kinds {
		resp, ok := responseTypes[k.Name]
		if !ok {
			t.Errorf("%s: no response type listed", k.Name)
			continue
		}
		respPaths := fieldPaths(resp, "")
		for _, p := range fieldPaths(k.ReqType, "") {
			if _, ok := writeOnly[k.Name+"."+p]; ok {
				continue
			}
			if !covered(p, respPaths) {
				t.Errorf("%s: request field %q is not in the %s response", k.Name, p, resp.Name())
			}
		}
	}
}

func covered(p string, resp []string) bool {
	for _, r := range resp {
		if r == p || strings.HasPrefix(r, p+".") {
			return true
		}
	}
	return false
}

// fieldPaths lists dotted JSON field paths of a type; slices add no segment.
func fieldPaths(t reflect.Type, prefix string) []string {
	for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice || t.Kind() == reflect.Map {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct || reflect.PointerTo(t).Implements(unmarshalerType) || t.PkgPath() == "time" {
		if prefix == "" {
			return nil
		}
		return []string{prefix}
	}
	var out []string
	for i := range t.NumField() {
		f := t.Field(i)
		name := jsonName(f)
		if name == "" {
			continue
		}
		p := name
		if prefix != "" {
			p = prefix + "." + name
		}
		sub := fieldPaths(f.Type, p)
		if len(sub) == 0 {
			sub = []string{p}
		}
		out = append(out, sub...)
	}
	sort.Strings(out)
	return out
}
