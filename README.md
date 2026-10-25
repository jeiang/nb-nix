# nb-nix

Declare NetBird management settings in Nix, then `plan` and `apply` them
against a self-hosted NetBird server with an admin API token. There is no
state file: every plan compares the declaration with the live server.

Each nb-nix version is built for exactly one NetBird version (currently
**0.81.4**) and refuses to talk to a server running another version unless
you pass `--ignore-version`.

## What it manages

| Option (`netbird.*`) | NetBird object | Identity |
|---|---|---|
| `account.settings` | account settings | singleton |
| `dns_settings` | DNS settings | singleton |
| `posture_checks.<k>` | posture checks | name |
| `groups.<k>` | groups | name |
| `networks.<k>` with `.resources.<k>` and `.routers.<k>` | networks, network resources, routing peers | name; resources by network + name; routers by network + peer or peer groups |
| `nameserver_groups.<k>` | nameserver groups | name |
| `policies.<k>` | access policies | name |
| `setup_keys.<k>` | setup keys | name |
| `services.<k>` | reverse-proxy services | name |
| `peers.<k>` | existing peers (adopt only; peers cannot be created) | name |
| `users.<k>` | existing human users: `role`, `auto_groups`, `is_blocked` (adopt only) | user ID |
| `service_users.<k>` with `.tokens.<k>` | service users and their personal access tokens | name; tokens by service user + name |

`<k>` is only a Nix handle; `name` defaults to it. Field names are the NetBird
API's (see `nix/options.generated.nix`, generated from the API spec). Every
field that holds IDs in the API (groups, peers, posture checks, network
resources, policy and service targets) takes **names** instead; nb-nix
resolves them, including objects created in the same apply.

Not managed: routes (use networks), identity providers, proxy tokens, DNS
zones, cloud-only integrations.

## Use

```nix
# flake.nix of your configuration
{
  inputs.nb-nix.url = "github:jeiang/nb-nix";
  outputs = {nb-nix, nixpkgs, ...}: let
    pkgs = nixpkgs.legacyPackages.x86_64-linux;
  in {
    packages.x86_64-linux.netbird-desired =
      (nb-nix.lib.evalConfig {inherit pkgs; modules = [./netbird.nix];}).config.netbird.desiredFile;
  };
}
```

```sh
export NETBIRD_API_TOKEN=...            # or --token-file FILE
nix build .#netbird-desired
nb-nix plan  -c result -o plan.json     # read-only
nb-nix apply plan.json                  # or: nb-nix apply -c result
```

Start from the live server: `nb-nix import --server https://netbird.example.com -o netbird.nix`
writes a module that declares the current state; a plan with it shows no
changes. `examples/basic.nix` shows every kind.

Commands: `status`, `import`, `plan [--detailed-exitcode]` (exit 2 when there
are changes), `apply [--allow-risky] [--rotate KIND/NAME]`.

## How changes are made

- **Undeclared fields keep their live values.** Updates read the live object,
  overlay the declared fields and send the whole object back, because the
  NetBird API resets omitted fields on PUT. Lists you declare replace the live
  list; lists of objects (policy rules, nameservers, targets) merge element by
  element, so undeclared fields inside them are kept.
- **Group members:** leave `peers` unset to keep live membership (setup keys,
  user groups and JWT sync also assign peers). Declaring `peers` makes that
  list exact. Do not declare `peers` on groups you assign through users.
- **Users** exist only after their first login and are matched by their
  NetBird user ID (`import` prints it; plans show the user's name beside it),
  so a public config need not contain email addresses. Declaring an ID that
  is not on the server fails the plan. A user's `auto_groups` are pushed to all of
  the user's peers (groups propagation), additions and removals alike. A
  declared user pending approval is approved. nb-nix refuses changes the
  server would reject: the owner's role or blocked state, assigning `owner`,
  the token's own role or blocked state, and the `All` group. Leave `role`
  undeclared for the owner (`import` does); ownership moves in the dashboard.
  Users are never deleted (that deletes their peers). Service users are a
  separate kind (`service_users`), which nb-nix can create and prune; the
  service user the API token belongs to is never pruned.
- **Read-only plans:** `plan` works with an Auditor-role token, for drift
  checks in CI. An auditor may not list other service users' tokens; the plan
  lists those as "Not checked" and plans no token changes for them.
- **Stale plans are refused.** The plan stores a hash of each object's
  request fields (what nb-nix would send back); apply re-reads them and stops
  if any changed. Server-maintained fields such as `last_seen` do not count.
- **Risky changes need `--allow-risky`:** deletes, setup key replacement,
  account `network_range`/`network_range_v6`/`dns_domain`, turning peer
  approval off, turning groups propagation on, turning `jwt_groups_enabled`
  on or off (it also switches the `jwt_allow_groups` login gate), peer
  `ip`/`ipv6`, removing peers from a declared membership, revoking a setup
  key, user role changes, blocking a user, removing groups from a user.
- **Apply is not transactional.** It stops at the first failure and reports
  how many changes were made; the next plan starts from the new live state.
- **Undeclared objects** are listed in every plan and left alone. Set
  `netbird.prune.<kind> = true` to delete them. Protected objects are never
  deleted: the `All` group, groups synced from the identity provider, and
  temporary-access policies.

### Secrets

- **Setup keys:** the key value is shown once, at creation. Creating a key
  requires `sink.command`, an argv (no shell) that gets the value on stdin,
  JSON-encoded by default:
  `sink.command = ["sops" "set" "--value-stdin" "secrets.yaml" ''["netbird"]["setup-key"]''];`
  (sops 3.11 or later). If the command fails, nb-nix deletes the new key.
  Changing `type`, `usage_limit`, `ephemeral` or `allow_extra_dns_labels`
  replaces the key (new value through the sink). `expires_in` only applies at
  creation.
- **Reverse-proxy passwords, PINs and header values** take `*_file` options
  (for example `auth.password_auth.password_file`). The file is read only when
  the secret must be sent: on create, when the auth method is turned on, or
  with `--rotate services/NAME`. Otherwise the server keeps its stored value.
  nb-nix cannot detect a changed secret file; rotate explicitly.
- **Tokens** (`service_users.<k>.tokens.<k>`) are created with `expires_in`
  days (default 365) and their value goes through `sink`, like setup keys.
  Tokens cannot be updated: with `renew_before_days = N`, a token that
  expires within N days is replaced (risky: new token through the sink, then
  the old one is deleted). nb-nix can renew the token it runs with: the old
  token is deleted as the very last request of the apply. Tokens of the user
  the API token belongs to are never pruned.
- Plan files never contain secret values, only file paths.

## Development

```sh
nix develop
just test          # go vet + unit tests
just check         # nix flake check: build, tests, version pin, generated options, example
just server-test   # plan/apply tests against a throwaway local NetBird server
just vm-test       # NixOS VM test with enrolled peers (x86_64-linux with KVM)
```

`just vm-test` builds on artemis through `--store ssh-ng://artemis.jeiang.vpn`;
change the recipe for another KVM-capable x86_64-linux host.

### Updating NetBird

1. `go get github.com/netbirdio/netbird@vX.Y.Z`, copy the `replace` directives
   from NetBird's `go.mod` into ours, `go mod tidy`.
2. Set `PinnedVersion` in `internal/nbapi/client.go` and the `netbird-src` URL
   in `flake.nix`; update both `vendorHash` values.
3. `just gen` and review the diff of `nix/options.generated.nix`.
4. `just test`: `TestRequestFieldsComeBackFromServer` fails if a new request
   field is not returned by the server (it would be reset on every apply);
   handle it in `internal/engine/kinds.go`.
5. Read the OpenAPI diff for semantic changes, then `just check`,
   `just server-test` and `just vm-test`.
