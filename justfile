# Run recipes inside the devshell: `nix develop -c just <recipe>`.

default:
    @just --list

# Vet and unit-test the Go code
test:
    go vet ./...
    go test ./...

# Flake checks (package build, version pin, generated options, Nix module example)
check:
    nix flake check

# Regenerate nix/options.generated.nix from the pinned NetBird OpenAPI spec
gen:
    go run ./cmd/gen-options > nix/options.generated.nix

# Plan/apply tests against a throwaway local NetBird server
server-test *args:
    ./scripts/server-test.sh {{args}}

# NixOS VM test with enrolled peers, built and run on artemis
vm-test:
    nix build -L --eval-store auto --store ssh-ng://artemis.jeiang.vpn .#checks.x86_64-linux.integration

# Read-only plan of DESIRED (desired-state JSON) against the live server ($NETBIRD_API_TOKEN)
live-plan desired *args:
    go run ./cmd/nb-nix plan -c {{desired}} --detailed-exitcode {{args}}
