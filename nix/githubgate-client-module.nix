# NixOS module that runs the githubgate interceptor as a system service in a
# VM workspace. The service listens on 127.0.0.1:443 and the three GitHub host
# names resolve to it, so `gh`, `git` and `curl` need no setup.
{ config, lib, ... }:

let
  cfg = config.services.githubgate-client;

  stateDirName = "githubgate-intercept";
  trustDir = "/run/githubgate/trust";
  hosts = [ "api.github.com" "uploads.github.com" "github.com" ];

  args = [
    "intercept"
    "--listen=127.0.0.1:443"
    "--proxy-address=${cfg.proxyAddress}"
    "--proxy-ca=@${cfg.proxyCA}"
    "--token-file=${cfg.tokenFile}"
    "--trust-dir=${trustDir}"
    "--state-dir=/run/${stateDirName}"
    "--base-bundle=/etc/ssl/certs/ca-certificates.crt"
  ] ++ lib.optional (cfg.extraCA != null) "--extra-ca=@${cfg.extraCA}";
in
{
  options.services.githubgate-client = {
    enable = lib.mkEnableOption "the githubgate interceptor, which sends the VM's GitHub requests through the githubgate proxy";

    package = lib.mkOption {
      type = lib.types.package;
      description = "The package that provides bin/githubgate-client.";
    };

    proxyAddress = lib.mkOption {
      type = lib.types.str;
      example = "githubgate-proxy.githubgate.svc:443";
      description = "The host:port of the githubgate proxy.";
    };

    proxyCA = lib.mkOption {
      type = lib.types.str;
      example = "/etc/githubgate/proxy-ca.crt";
      description = "Path of the file that holds the proxy's CA certificate. It is the only root the interceptor uses to verify the proxy.";
    };

    extraCA = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = null;
      description = "Path of a PEM file of extra CA certificates to add to the trust bundle that the workload reads.";
    };

    tokenFile = lib.mkOption {
      type = lib.types.str;
      default = "/var/run/secrets/githubgate/token";
      description = "Path of the file that holds the ServiceAccount token (audience `githubgate`). The service user must be able to read it.";
    };
  };

  config = lib.mkIf cfg.enable {
    users.users.githubgate-intercept = {
      isSystemUser = true;
      group = "githubgate-intercept";
    };
    users.groups.githubgate-intercept = { };

    systemd.services.githubgate-intercept = {
      description = "githubgate interceptor";
      wantedBy = [ "multi-user.target" ];
      after = [ "network.target" "systemd-tmpfiles-setup.service" ];
      serviceConfig = {
        ExecStart = "${cfg.package}/bin/githubgate-client ${lib.concatStringsSep " " args}";
        User = "githubgate-intercept";
        Group = "githubgate-intercept";
        AmbientCapabilities = [ "CAP_NET_BIND_SERVICE" ];
        Restart = "on-failure";
        RuntimeDirectory = stateDirName;
        RuntimeDirectoryMode = "0700";
        RuntimeDirectoryPreserve = "yes";
      };
    };

    networking.hosts."127.0.0.1" = hosts;

    # The file exists from boot, so tools never fail on a missing bundle. The
    # service replaces it when it starts.
    systemd.tmpfiles.rules = [
      "d ${trustDir} 0755 githubgate-intercept githubgate-intercept -"
      "C ${trustDir}/bundle.crt 0444 githubgate-intercept githubgate-intercept - ${config.security.pki.caBundle}"
    ];

    environment.variables = {
      SSL_CERT_FILE = "${trustDir}/bundle.crt";
      CURL_CA_BUNDLE = "${trustDir}/bundle.crt";
      GIT_SSL_CAINFO = "${trustDir}/bundle.crt";
      REQUESTS_CA_BUNDLE = "${trustDir}/bundle.crt";
      NODE_EXTRA_CA_CERTS = "${trustDir}/ca.crt";
      GH_TOKEN = lib.mkDefault "githubgate";
    };
  };
}
