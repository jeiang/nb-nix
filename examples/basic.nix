# Example nb-nix configuration. Evaluate with
#   nix build .#example-desired   (or lib.evalConfig in your own flake)
{config, ...}: let
  nb = config.netbird;
in {
  netbird = {
    server = "https://netbird.example.com";
    prune.policies = true;

    account.settings = {
      dns_domain = "mesh.example";
      peer_login_expiration_enabled = true;
      peer_login_expiration = 86400;
      ipv6_enabled_groups = ["All"];
    };

    groups = {
      admins = {};
      servers.name = "Servers";
    };

    posture_checks.recent-client = {
      description = "NetBird 0.60 or newer";
      checks.nb_version_check.min_version = "0.60.0";
    };

    networks.home = {
      description = "Home LAN";
      resources.lan = {
        address = "192.168.1.0/24";
        groups = [nb.groups.servers.name];
      };
      routers.gateway = {
        peer = "gateway";
        metric = 9999;
        masquerade = true;
      };
    };

    nameserver_groups.quad9 = {
      description = "";
      nameservers = [
        {
          ip = "9.9.9.9";
          ns_type = "udp";
          port = 53;
        }
      ];
      groups = ["All"];
      primary = true;
      domains = [];
      search_domains_enabled = false;
    };

    policies.admin-ssh = {
      source_posture_checks = [nb.posture_checks.recent-client.name];
      rules = [
        {
          name = "admin-ssh";
          action = "accept";
          protocol = "tcp";
          ports = ["22"];
          bidirectional = false;
          sources = [nb.groups.admins.name];
          destinations = [nb.groups.servers.name];
        }
      ];
    };

    setup_keys.servers = {
      type = "reusable";
      expires_in = 0;
      usage_limit = 0;
      auto_groups = [nb.groups.servers.name];
      sink = {
        command = ["sops" "set" "--value-stdin" "secrets.yaml" ''["netbird"]["setup-key"]''];
        format = "json";
      };
    };

    services.grafana = {
      domain = "grafana.proxy.example.com";
      targets = [
        {
          target_id = "gateway";
          target_type = "peer";
          protocol = "http";
          port = 3000;
        }
      ];
      auth.password_auth = {
        enabled = true;
        password_file = "/run/secrets/grafana-proxy-password";
      };
      access_restrictions.crowdsec_mode = "enforce";
    };

    peers.gateway = {
      ip = "100.80.0.1";
      ssh_enabled = true;
    };
  };
}
