#!/usr/bin/env bash
# Runs the `server`-tagged Go tests against a throwaway NetBird server (the
# flake's netbird-server package at the pinned version) on 127.0.0.1.
set -euo pipefail

server_bin="${NETBIRD_SERVER_BIN:-$(nix build --no-link --print-out-paths .#netbird-server)/bin/netbird-server}"
port="${NB_TEST_PORT:-18080}"
work="$(mktemp -d "${TMPDIR:-/tmp}/nb-nix-server.XXXXXX")"
pid=""
cleanup() {
  if [ -n "$pid" ]; then kill "$pid" 2>/dev/null || true; wait "$pid" 2>/dev/null || true; fi
  rm -rf "$work"
}
trap cleanup EXIT

base="http://127.0.0.1:$port"
mkdir -p "$work/data"
cat >"$work/config.yaml" <<EOF
server:
  listenAddress: "127.0.0.1:$port"
  exposedAddress: "$base"
  healthcheckAddress: "127.0.0.1:$((port + 1))"
  metricsPort: $((port + 2))
  stunPorts: []
  logLevel: "warn"
  logFile: "console"
  authSecret: "nb-nix-test-relay-secret"
  dataDir: "$work/data"
  disableAnonymousMetrics: true
  disableGeoliteUpdate: true
  auth:
    issuer: "$base/oauth2"
  store:
    engine: "sqlite"
EOF

NB_SETUP_PAT_ENABLED=true NB_DISABLE_GEOLOCATION=true \
  "$server_bin" --config "$work/config.yaml" >"$work/server.log" 2>&1 &
pid=$!

for _ in $(seq 1 120); do
  if curl -fsS "$base/api/instance" >/dev/null 2>&1; then break; fi
  if ! kill -0 "$pid" 2>/dev/null; then cat "$work/server.log" >&2; exit 1; fi
  sleep 0.5
done

token="$(curl -fsS -X POST "$base/api/setup" -H 'Content-Type: application/json' \
  -d '{"email":"admin@nb-nix.test","password":"nb-nix-Test-Passw0rd!","name":"nb-nix test","create_pat":true,"pat_expire_in":1}' |
  jq -r '.personal_access_token')"
if [ -z "$token" ] || [ "$token" = null ]; then
  echo "no token from /api/setup" >&2
  cat "$work/server.log" >&2
  exit 1
fi

NB_TEST_MANAGEMENT_URL="$base" NB_TEST_TOKEN="$token" \
  go test -tags server -count=1 "$@" ./internal/servertest/ || {
  status=$?
  echo "--- server log (tail) ---" >&2
  tail -n 50 "$work/server.log" >&2
  exit "$status"
}
