# nb-nix module: declare NetBird management objects. Evaluate with
# nb-nix's lib.evalConfig and pass config.netbird.desiredFile to
# `nb-nix plan -c`. Field names follow the NetBird API; every field that holds
# IDs in the API (groups, peers, posture checks, resources) takes names here.
{
  lib,
  config,
  pkgs ? null,
  ...
}: let
  t = lib.types;
  gen = import ./options.generated.nix {inherit lib;};
  cfg = config.netbird;

  # Objects default their `name` to the attribute name.
  named = modules:
    t.submodule (lib.toList modules
      ++ [
        ({name, ...}: {config.name = lib.mkDefault name;})
      ]);
  objects = modules: description:
    lib.mkOption {
      type = t.attrsOf (named modules);
      default = {};
      inherit description;
    };

  # Drop nulls (unset options) everywhere so that only declared fields remain.
  clean = v:
    if builtins.isAttrs v
    then lib.mapAttrs (_: clean) (lib.filterAttrs (_: x: x != null) v)
    else if builtins.isList v
    then map clean v
    else v;

  prunable = ["posture_checks" "groups" "networks" "network_resources" "network_routers" "nameserver_groups" "policies" "setup_keys" "services"];

  setupKeyExtra = {
    options = {
      revoked = lib.mkOption {
        type = t.nullOr t.bool;
        default = null;
        description = "Whether the setup key is revoked.";
      };
      sink = lib.mkOption {
        type = t.nullOr (t.submodule {
          options = {
            command = lib.mkOption {
              type = t.nonEmptyListOf t.str;
              description = "Command (argv, no shell) that receives a newly created key on stdin.";
            };
            format = lib.mkOption {
              type = t.enum ["json" "raw"];
              default = "json";
              description = "Write the key as a JSON string (for `sops set --value-stdin`) or as raw text.";
            };
          };
        });
        default = null;
        description = "Where a newly created setup key value goes. Required to create a key.";
      };
    };
  };

  networkExtra = {
    options = {
      resources = objects gen.NetworkResourceRequest "Network resources of this network.";
      routers = lib.mkOption {
        type = t.attrsOf (t.submodule gen.NetworkRouterRequest);
        default = {};
        description = "Routing peers of this network. Routers have no name; the attribute name is only a handle.";
      };
    };
  };

  networks = lib.attrValues cfg.networks;
  children = field: lib.concatMap (n: map (c: c // {network = n.name;}) (lib.attrValues n.${field})) networks;
in {
  options.netbird = {
    server = lib.mkOption {
      type = t.nullOr t.str;
      default = null;
      example = "https://netbird.example.com";
      description = "Management server URL.";
    };
    prune = lib.genAttrs prunable (kind:
      lib.mkOption {
        type = t.bool;
        default = false;
        description = "Delete live ${kind} that are not declared (protected objects are kept).";
      });
    account.settings = lib.mkOption {
      type = t.nullOr (t.submodule gen.AccountSettings);
      default = null;
      description = "Account settings. Undeclared fields keep their live values.";
    };
    dns_settings = lib.mkOption {
      type = t.nullOr (t.submodule gen.DNSSettings);
      default = null;
      description = "DNS settings.";
    };
    posture_checks = objects gen.PostureCheckUpdate "Posture checks, by attribute handle.";
    groups = objects gen.GroupRequest "Groups. Leave `peers` unset to keep the live members.";
    networks = objects [gen.NetworkRequest networkExtra] "Networks with their resources and routers.";
    nameserver_groups = objects gen.NameserverGroupRequest "Nameserver groups.";
    policies = objects gen.PolicyCreate "Access control policies.";
    setup_keys = objects [gen.CreateSetupKeyRequest setupKeyExtra] "Setup keys.";
    services = objects gen.ServiceRequest "Reverse-proxy services.";
    peers = objects gen.PeerRequest "Existing peers to manage (peers cannot be created).";
    users = lib.mkOption {
      type = t.attrsOf (t.submodule [
        gen.UserRequest
        {
          options.email = lib.mkOption {
            type = t.str;
            description = "Email of an existing (logged-in) user; identifies the user.";
          };
        }
      ]);
      default = {};
      description = "Existing human users to manage (role, auto_groups, is_blocked). Users are never created or deleted; leave `role` undeclared for the owner.";
    };

    desired = lib.mkOption {
      type = t.attrs;
      readOnly = true;
      description = "The desired state passed to nb-nix.";
    };
    desiredFile = lib.mkOption {
      type = t.package;
      readOnly = true;
      description = "The desired state as a JSON file (needs `pkgs`).";
    };
  };

  config.netbird = {
    desired = clean {
      inherit (cfg) server prune dns_settings;
      account =
        if cfg.account.settings == null
        then null
        else {inherit (cfg.account) settings;};
      posture_checks = lib.attrValues cfg.posture_checks;
      groups = lib.attrValues cfg.groups;
      networks = map (n: removeAttrs n ["resources" "routers"]) networks;
      network_resources = children "resources";
      network_routers = children "routers";
      nameserver_groups = lib.attrValues cfg.nameserver_groups;
      policies = lib.attrValues cfg.policies;
      setup_keys = lib.attrValues cfg.setup_keys;
      services = lib.attrValues cfg.services;
      peers = lib.attrValues cfg.peers;
      users = lib.attrValues cfg.users;
    };
    desiredFile = pkgs.writeText "netbird-desired.json" (builtins.toJSON cfg.desired);
  };
}
