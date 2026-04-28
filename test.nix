{ counter }:
{ pkgs, lib, ... }:
let
  tls-cert = pkgs.runCommand "selfSignedCerts" { buildInputs = [ pkgs.openssl ]; } ''
    openssl req \
      -x509 -newkey rsa:4096 -sha256 -days 365 \
      -nodes -out cert.pem -keyout key.pem \
      -subj '/CN=headscale' -addext "subjectAltName=DNS:headscale"
    mkdir -p $out
    cp key.pem cert.pem $out
  '';

  headscalePort = 8080;
  stunPort = 3478;

  mkCounterNode = name: {
    security.pki.certificateFiles = [ "${tls-cert}/cert.pem" ];
    environment.systemPackages = [ counter pkgs.curl ];
    systemd.services.counter = {
      description = "rqloud counter (${name})";
      after = [ "network-online.target" ];
      wants = [ "network-online.target" ];
      environment = {
        RQLOUD_CONTROL_URL = "https://headscale";
        TS_NO_LOGS_NO_SUPPORT = "true";
      };
      serviceConfig = {
        ExecStart = "${counter}/bin/counter -instance ${name} -data-dir /var/lib/counter -bootstrap-expect 3 -verbose";
        EnvironmentFile = "-/etc/default/counter";
        StateDirectory = "counter";
        Restart = "on-failure";
        RestartSec = "2";
      };
    };
  };
in
{
  name = "rqloud-counter";

  nodes = {
    headscale = {
      services = {
        headscale = {
          enable = true;
          port = headscalePort;
          settings = {
            server_url = "https://headscale";
            ip_prefixes = [ "100.64.0.0/10" ];
            derp = {
              server = {
                enabled = true;
                region_id = 999;
                stun_listen_addr = "0.0.0.0:${toString stunPort}";
              };
              urls = [ ];
            };
            dns = {
              base_domain = "tailnet";
              nameservers.global = [ "127.0.0.1" ];
            };
          };
        };
        nginx = {
          enable = true;
          virtualHosts.headscale = {
            addSSL = true;
            sslCertificate = "${tls-cert}/cert.pem";
            sslCertificateKey = "${tls-cert}/key.pem";
            locations."/" = {
              proxyPass = "http://127.0.0.1:${toString headscalePort}";
              proxyWebsockets = true;
            };
          };
        };
      };
      networking.firewall = {
        allowedTCPPorts = [ 80 443 ];
        allowedUDPPorts = [ stunPort ];
      };
      environment.systemPackages = [ pkgs.headscale ];
    };

    node1 = mkCounterNode "counter-1";
    node2 = mkCounterNode "counter-2";
    node3 = mkCounterNode "counter-3";
  };

  testScript = ''
    start_all()

    # Wait for headscale to be ready.
    headscale.wait_for_unit("headscale")
    headscale.wait_for_open_port(443)

    # Create a user and a reusable pre-auth key (headscale 0.27+ wants user ID, not name).
    headscale.succeed("headscale users create test")
    user_id = headscale.succeed("headscale users list -o json | ${pkgs.jq}/bin/jq -r '.[0].id'").strip()
    authkey = headscale.succeed(f"headscale preauthkeys -u {user_id} create --reusable").strip()

    # Write the auth key and start counter on each node.
    for node in [node1, node2, node3]:
        node.succeed(f"echo 'TS_AUTHKEY={authkey}' > /etc/default/counter")
        node.succeed("systemctl start counter")

    # Wait for all nodes to have their local HTTP listener up.
    for node in [node1, node2, node3]:
        node.wait_for_open_port(8080)

    # Verify counter starts at 0 on all nodes.
    for node in [node1, node2, node3]:
        node.wait_until_succeeds("curl -sf --max-time 10 http://localhost:8080/value | grep -q '^0$'")

    # Increment on node1.
    node1.succeed("curl -sf --max-time 10 -X POST -d 'action=inc' http://localhost:8080/")

    # Verify all nodes see 1 (replication).
    for node in [node1, node2, node3]:
        node.wait_until_succeeds("curl -sf --max-time 10 http://localhost:8080/value | grep -q '^1$'")

    # Increment on node2.
    node2.succeed("curl -sf --max-time 10 -X POST -d 'action=inc' http://localhost:8080/")

    # Verify all nodes see 2.
    for node in [node1, node2, node3]:
        node.wait_until_succeeds("curl -sf --max-time 10 http://localhost:8080/value | grep -q '^2$'")

    # Decrement on node3.
    node3.succeed("curl -sf --max-time 10 -X POST -d 'action=dec' http://localhost:8080/")

    # Verify all nodes see 1.
    for node in [node1, node2, node3]:
        node.wait_until_succeeds("curl -sf --max-time 10 http://localhost:8080/value | grep -q '^1$'")
  '';
}
