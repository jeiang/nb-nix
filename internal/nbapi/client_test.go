package nbapi

import (
	"os"
	"regexp"
	"testing"
)

// The pinned version gates which servers nb-nix talks to; it must follow the
// NetBird module version the API types come from.
func TestPinnedVersionMatchesGoMod(t *testing.T) {
	b, err := os.ReadFile("../../go.mod")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^\s*github\.com/netbirdio/netbird v(\S+)`).FindSubmatch(b)
	if m == nil {
		t.Fatal("netbird requirement not found in go.mod")
	}
	if got := PinnedVersion; got != string(m[1]) {
		t.Fatalf("PinnedVersion %q, go.mod requires %q", got, m[1])
	}
}
