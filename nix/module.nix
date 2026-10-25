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

  prunable = ["posture_checks" "groups" "networks" "network_resources" "network_routers" "nameserver_groups" "policies" "setup_keys" "services" "service_users" "tokens"];

  sinkOption = what:
    lib.mkOption {
      type = t.nullOr (t.submodule {
        options = {
          command = lib.mkOption {
            type = t.nonEmptyListOf t.str;
            description = "Command (argv, no shell) that receives the new ${what} on stdin.";
          };
          format = lib.mkOption {
            type = t.enum ["json" "raw"];
            default = "json";
            description = "Write the ${what} as a JSON string (for `sops set --value-stdin`) or as raw text.";
          };
        };
      });
      default = null;
      description = "Where a newly created ${what} goes (it is shown only once). Required to create one.";
    };
  optional = type: description:
    lib.mkOption {
      type = t.nullOr type;
      default = null;
      inherit description;
    };

  setupKeyExtra.options = {
    revoked = optional t.bool "Whether the setup key is revoked.";
    sink = sinkOption "setup key";
  };

  networkExtra.options = {
    resources = objects gen.NetworkResourceRequest "Network resources of this network.";
    routers = lib.mkOption {
      type = t.attrsOf (t.submodule gen.NetworkRouterRequest);
      default = {};
      description = "Routing peers of this network. Routers have no name; the attribute name is only a handle.";
    };
  };

  tokenOptions.options = {
    name = optional t.str "Token name; identifies the token within its service user.";
    expires_in = optional (t.ints.between 1 365) "Lifetime in days, applied when the token is created (default 365).";
    renew_before_days = optional t.ints.unsigned "Replace the token (risky) once it expires within this many days.";
    sink = sinkOption "token";
  };

  serviceUserExtra.options = {
    name = optional t.str "Service user name; identifies the user.";
    tokens = objects tokenOptions "Personal access tokens of this service user.";
  };

  # Child objects carry their parent's name in parentField.
  children = parents: field: parentField:
    lib.concatMap (p: map (c: c // {${parentField} = p.name;}) (lib.attrValues p.${field})) (lib.attrValues parents);
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
    service_users = objects [gen.UserRequest serviceUserExtra] "Service users and their tokens.";

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
      networks = map (n: removeAttrs n ["resources" "routers"]) (lib.attrValues cfg.networks);
      network_resources = children cfg.networks "resources" "network";
      network_routers = children cfg.networks "routers" "network";
      nameserver_groups = lib.attrValues cfg.nameserver_groups;
      policies = lib.attrValues cfg.policies;
      setup_keys = lib.attrValues cfg.setup_keys;
      services = lib.attrValues cfg.services;
      peers = lib.attrValues cfg.peers;
      users = lib.attrValues cfg.users;
      service_users = map (u: removeAttrs u ["tokens"]) (lib.attrValues cfg.service_users);
      tokens = children cfg.service_users "tokens" "user";
    };
    desiredFile = pkgs.writeText "netbird-desired.json" (builtins.toJSON cfg.desired);
  };
}
