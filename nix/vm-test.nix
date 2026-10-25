# NixOS VM test: a NetBird server at the pinned version, two enrolled
# clients, and nb-nix applying configs that need real peers (setup key sink,
# peer adoption, peer IP pinning, exclusive group membership, routers on a
# peer). Run with `just vm-test`.
{
  pkgs,
  nb-nix,
  netbird-server,
  netbird-client,
  evalConfig,
}: let
  port = 8080;
  desired = config: (evalConfig {
    inherit pkgs;
    modules = [{netbird = config // {server = "http://server:${toString port}";};}];
  }).config.netbird.desiredFile;

  # Step 1: account network, group and a setup key for the clients.
  bootstrap = desired {
    account.settings.network_range = "100.80.0.0/16";
    groups.clients = {};
    setup_keys.vm = {
      type = "reusable";
      expires_in = 3600;
      usage_limit = 0;
      auto_groups = ["clients"];
      sink.command = ["sh" "-c" "cat > /tmp/setup-key.json"];
    };
  };
  # Step 2: adopt the enrolled peers.
  peers = desired {
    groups.clients = {};
    groups.admins.peers = ["client1"];
    peers.client1 = {
      ip = "100.80.0.50";
      ssh_enabled = true;
    };
    networks.office = {
      resources.lan = {
        address = "192.168.50.0/24";
        groups = ["clients"];
      };
      routers.via-client2 = {
        peer = "client2";
        metric = 100;
        masquerade = true;
      };
    };
    policies.admins-to-lan = {
      rules = [
        {
          name = "admins-to-lan";
          action = "accept";
          protocol = "all";
          bidirectional = false;
          sources = ["admins"];
          destinationResource = {
            id = "lan";
            type = "subnet";
          };
        }
      ];
    };
  };

  # Clients run `netbird up --foreground-mode` (no daemon), so they never
  # try the default api.netbird.io management server.
  client = {
    environment.systemPackages = [netbird-client pkgs.iproute2];
    networking.firewall.allowedUDPPorts = [51820];
  };
in
  pkgs.testers.runNixOSTest {
    name = "nb-nix-integration";
    globalTimeout = 1200;
    nodes = {
      server = {
        networking.firewall.allowedTCPPorts = [port];
        environment.systemPackages = [nb-nix pkgs.jq pkgs.curl];
        environment.etc."netbird-server.yaml".text = ''
          server:
            listenAddress: ":${toString port}"
            exposedAddress: "http://server:${toString port}"
            healthcheckAddress: "127.0.0.1:9000"
            metricsPort: 9090
            stunPorts: []
            logLevel: "info"
            logFile: "console"
            authSecret: "vm-test-relay-secret"
            dataDir: "/var/lib/netbird-server"
            disableAnonymousMetrics: true
            disableGeoliteUpdate: true
            auth:
              issuer: "http://server:${toString port}/oauth2"
            store:
              engine: "sqlite"
        '';
        systemd.services.netbird-server = {
          wantedBy = ["multi-user.target"];
          after = ["network-online.target"];
          wants = ["network-online.target"];
          environment = {
            NB_SETUP_PAT_ENABLED = "true";
            NB_DISABLE_GEOLOCATION = "true";
          };
          serviceConfig = {
            ExecStart = "${netbird-server}/bin/netbird-server --config /etc/netbird-server.yaml";
            StateDirectory = "netbird-server";
          };
        };
      };
      client1 = client;
      client2 = client;
    };

    testScript = ''
      import json

      start_all()
      server.wait_for_unit("netbird-server.service")
      server.wait_until_succeeds("curl -fsS http://localhost:${toString port}/api/instance")

      setup = json.loads(server.succeed(
          "curl -fsS -X POST http://localhost:${toString port}/api/setup -H 'Content-Type: application/json' "
          "-d '{\"email\":\"admin@vm.test\",\"password\":\"Vm-Test-Passw0rd!\",\"name\":\"vm\",\"create_pat\":true}'"
      ))
      env = f"NETBIRD_API_TOKEN={setup['personal_access_token']}"

      with subtest("status"):
          print(server.succeed(f"{env} nb-nix status --server http://server:${toString port}"))

      with subtest("bootstrap creates the setup key through the sink"):
          print(server.succeed(f"{env} nb-nix apply -c ${bootstrap} --allow-risky"))
          server.succeed(f"{env} nb-nix plan -c ${bootstrap} --detailed-exitcode")
          key = server.succeed("jq -r . /tmp/setup-key.json").strip()
          assert len(key) > 16, "setup key missing from sink"

      with subtest("clients enroll with the key"):
          for node in (client1, client2):
              node.wait_for_unit("multi-user.target")
              node.succeed(
                  "systemd-run --unit nb-client --property=StateDirectory=nb-client "
                  f"netbird up --foreground-mode --config /var/lib/nb-client/config.json --log-file console "
                  f"--management-url http://server:${toString port} --setup-key {key} --hostname {node.name}"
              )
          api = f"curl -fsS -H 'Authorization: Token {setup['personal_access_token']}' http://localhost:${toString port}/api"
          server.wait_until_succeeds(f"{api}/peers | jq -e 'map(.name) | sort == [\"client1\", \"client2\"]'", timeout=180)

      with subtest("peer config applies and converges"):
          plan = server.succeed(f"{env} nb-nix plan -c ${peers} || true")
          print(plan)
          print(server.succeed(f"{env} nb-nix apply -c ${peers} --allow-risky"))
          server.succeed(f"{env} nb-nix plan -c ${peers} --detailed-exitcode")

      with subtest("server state matches the declaration"):
          peers = {p["name"]: p for p in json.loads(server.succeed(f"{api}/peers"))}
          assert peers["client1"]["ip"] == "100.80.0.50", peers["client1"]["ip"]
          assert peers["client1"]["ssh_enabled"] is True
          groups = {g["name"]: g for g in json.loads(server.succeed(f"{api}/groups"))}
          assert [p["name"] for p in groups["admins"]["peers"]] == ["client1"], groups["admins"]
          # Undeclared membership (set by the setup key) is untouched.
          assert sorted(p["name"] for p in groups["clients"]["peers"]) == ["client1", "client2"], groups["clients"]

      with subtest("client gets its pinned address"):
          client1.wait_until_succeeds("ip -4 addr show wt0 | grep -q '100.80.0.50'", timeout=180)
    '';
  }
