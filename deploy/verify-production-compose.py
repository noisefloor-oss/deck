#!/usr/bin/env python3
"""Fail closed unless rendered Compose is the complete production profile."""

import json
import ipaddress
import os
import pathlib
import re
import sys


def fail(message: str) -> None:
    raise SystemExit(f"production-compose: {message}")


try:
    document = json.load(sys.stdin)
except (OSError, ValueError) as error:
    fail(f"cannot parse rendered Compose JSON: {error}")

services = document.get("services", {})
app = services.get("app", {})
browser = services.get("chromium", {})
go_base = services.get("go-base", {})
environment = app.get("environment", {})
build = app.get("build", {})
image = str(app.get("image", ""))


def verify_sidecar_resolver(service, name):
    mounts = [mount for mount in service.get("volumes", []) if mount.get("target") == "/etc/resolv.conf"]
    if len(mounts) != 1:
        fail(f"{name} requires its dedicated resolver mount")
    mount = mounts[0]
    source = pathlib.Path(str(mount.get("source", "")))
    options = mount.get("bind", {})
    if (mount.get("type") != "bind" or not source.is_absolute()
            or mount.get("read_only") is not True or options.get("selinux") != "z"
            or options.get("create_host_path", False) is not False):
        fail(f"{name} resolver must be a read-only shared-label file bind")
    if "--check-host-files" in sys.argv:
        if source.is_symlink() or not source.is_file() or not source.stat().st_mode & 0o004:
            fail(f"{name} resolver must exist as a world-readable regular file")
        # Compose's Mount API can discard selinux:z with create_host_path:false.
        # Require the real host label before any container is recreated.
        if pathlib.Path("/sys/fs/selinux/enforce").exists():
            try:
                label = os.getxattr(source, "security.selinux").rstrip(b"\0").decode().split(":", 2)
            except (OSError, UnicodeError):
                fail(f"{name} resolver SELinux label is unreadable; provision its shared file label first")
            if len(label) != 3 or label[2] != "container_file_t:s0":
                fail(f"{name} resolver requires container_file_t:s0; label it with semanage fcontext and restorecon")


verify_sidecar_resolver(browser, "Chromium")

relay_mode = environment.get("NOISEDECK_RELAY_MODE", "local")
relay_tunnel = services.get("relay-tunnel")
if relay_mode not in ("local", "external"):
    fail("invalid relay mode")
if (relay_mode == "external") != (relay_tunnel is not None):
    fail("external relay mode requires the relay tunnel overlay, and vice versa")
if relay_tunnel is not None:
    if not environment.get("NOISEDECK_RELAY_TOKEN"):
        fail("external relay requires a bearer token")
    if (relay_tunnel.get("network_mode") != "service:app" or relay_tunnel.get("ports")
            or relay_tunnel.get("image") != image):
        fail("relay tunnel must use the app image and namespace without publishing ports")
    if (relay_tunnel.get("read_only") is not True or relay_tunnel.get("user") != "0:0"
            or "ALL" not in relay_tunnel.get("cap_drop", [])
            or "no-new-privileges:true" not in [str(v).replace("=", ":") for v in relay_tunnel.get("security_opt", [])]):
        fail("relay tunnel lacks its restricted runtime profile")
    if relay_tunnel.get("depends_on", {}).get("app", {}).get("condition") != "service_started":
        fail("relay tunnel must start before app health to avoid a startup deadlock")
    expected_command = ["-F", "/run/relay/config", "-NT", "-oBatchMode=yes",
                        "-oStrictHostKeyChecking=yes", "-oUserKnownHostsFile=/run/relay/known_hosts",
                        "-oIdentityFile=/run/relay/id_ed25519", "-oIdentitiesOnly=yes",
                        "-oExitOnForwardFailure=yes", "-oServerAliveInterval=10",
                        "-oServerAliveCountMax=3", "-oConnectTimeout=10",
                        "-L", "0.0.0.0:8766:127.0.0.1:18766", "relay"]
    if relay_tunnel.get("entrypoint") != ["/usr/bin/ssh"] or relay_tunnel.get("command") != expected_command:
        fail("relay tunnel must retain its pinned-host authenticated SSH command")
    verify_sidecar_resolver(relay_tunnel, "relay tunnel")
    mounts = relay_tunnel.get("volumes", [])
    keys = [mount for mount in mounts if mount.get("target") == "/run/relay"]
    if len(mounts) != 2 or len(keys) != 1 or keys[0].get("read_only") is not True or keys[0].get("type") != "bind":
        fail("relay tunnel mounts only its private read-only SSH directory and resolver")
    directory = pathlib.Path(str(keys[0].get("source", "")))
    if not directory.is_absolute():
        fail("relay SSH directory must be absolute")
    # A dedicated directory outside all app mounts keeps SSH authority out of agents.
    real_directory = directory.resolve()
    for mount in app.get("volumes", []):
        source = pathlib.Path(str(mount.get("source", ""))).resolve()
        if mount.get("type") == "bind" and (source == real_directory or source in real_directory.parents or real_directory in source.parents):
            fail("relay SSH directory must not be accessible through an app mount")
    if "--check-host-files" in sys.argv:
        if directory.is_symlink() or not directory.is_dir() or directory.stat().st_uid != 0 or directory.stat().st_mode & 0o077:
            fail("relay SSH directory must be root-owned, real, and mode 0700")
        for name in ("config", "known_hosts", "id_ed25519"):
            path = directory / name
            if path.is_symlink() or not path.is_file() or path.stat().st_uid != 0 or path.stat().st_mode & 0o077:
                fail("relay SSH files must be root-owned, real, and owner-only")

preview = services.get("preview-tunnel")
if preview is not None:
    if preview.get("network_mode") != "service:app" or preview.get("ports"):
        fail("preview connector must share the app namespace without publishing ports")
    if preview.get("image") != "cloudflare/cloudflared@sha256:faa8e0a1475f137c910be6025f43c8e4092fdd35461b31e19fd1390657a1ff39":
        fail("preview connector image is not pinned")
    if preview.get("read_only") is not True or preview.get("user") != "65532:65532" or "ALL" not in preview.get("cap_drop", []):
        fail("preview connector lacks its restricted runtime profile")
    if "no-new-privileges:true" not in [str(value).replace("=", ":") for value in preview.get("security_opt", [])]:
        fail("preview connector must prohibit new privileges")
    if preview.get("cpus") != 0.25 or str(preview.get("mem_limit")) != "268435456":
        fail("preview connector must retain its CPU and memory limits")
    verify_sidecar_resolver(preview, "preview connector")
    mounts = preview.get("volumes", [])
    tokens = [mount for mount in mounts if mount.get("target") == "/run/secrets/preview-token"]
    if len(mounts) != 2 or len(tokens) != 1 or tokens[0].get("read_only") is not True:
        fail("preview connector must mount only its read-only connector token and resolver")
    if environment.get("NOISEDECK_PROJECT_PREVIEWS_BIND") != "127.0.0.1:8840" or str(environment.get("NOISEDECK_PROJECT_PREVIEWS_UNCONFINED")) != "true":
        fail("preview connector requires the acknowledged private preview listener")

if build.get("target") != "runtime":
    fail("app build target is not the Go-derived runtime")
dockerfile = str(build.get("dockerfile", "")).replace("\\", "/")
if not dockerfile.endswith("/deploy/derived/go.Dockerfile") and dockerfile != "deploy/derived/go.Dockerfile":
    fail("app build does not use the checked-in Go-derived image")
if app.get("container_name") != "noisefloor-deck":
    fail("canonical production container name is missing")
if app.get("labels", {}).get("com.noisefloor.deck.capability-profile") != "production-v1":
    fail("production-v1 capability label is missing")
if not image.startswith("deck:") or image == "deck:local":
    fail("an explicit non-local production image tag is required")
if build.get("additional_contexts", {}).get("deck-base") != "service:go-base":
    fail("Go-derived image does not use the build-ordered MemPalace base")
if go_base.get("build", {}).get("target") != "mempalace":
    fail("Go base service does not build the MemPalace target")
if go_base.get("scale") != 0:
    fail("Go base service is not build-only")
if not app.get("healthcheck", {}).get("test"):
    fail("the production app healthcheck is missing")

required_environment = {
    "NOISEDECK_AUTH_MODE": "oidc",
    "MEMPALACE_MCP_IDLE_HOURS": "0",
    "MEMPALACE_MCP_HTTP_ALLOW_INSECURE_NO_TOKEN": "1",
    "NOISE_BROWSER_WS": "ws://127.0.0.1:3000/",
}
for key, expected in required_environment.items():
    if str(environment.get(key, "")) != expected:
        fail(f"{key} is not {expected!r}")

# The unconfined capacity-routing experiment switch is a rendered
# string, not Compose's boolean coercion: only exactly "true" or "false"
# passes, so a missing, empty, or abbreviated value fails closed.
experiment_switch = str(environment.get("NOISEDECK_EXPERIMENTS_UNCONFINED_CAPACITY_ROUTING", ""))
if experiment_switch not in ("true", "false"):
    fail("NOISEDECK_EXPERIMENTS_UNCONFINED_CAPACITY_ROUTING must render as exactly 'true' or 'false'")

for key in (
    "NOISEDECK_SERVER_ALLOWED_ORIGINS",
    "NOISEDECK_AUTH_GOOGLE_CLIENT_ID",
    "NOISEDECK_AUTH_GOOGLE_CLIENT_SECRET",
    "NOISEDECK_AUTH_GOOGLE_REDIRECT_URL",
    "NOISEDECK_AUTH_GOOGLE_ALLOWED_EMAIL",
    "NOISEDECK_AUTH_SESSION_SECRET",
    "NOISEDECK_DEPLOYMENT_HOST_SHA256",
):
    if not str(environment.get(key, "")).strip():
        fail(f"{key} is empty")

origin = str(environment["NOISEDECK_SERVER_ALLOWED_ORIGINS"])
if not origin.startswith("https://") or origin.endswith("/"):
    fail("production public origin must be HTTPS with no trailing slash")
if environment["NOISEDECK_AUTH_GOOGLE_REDIRECT_URL"] != origin + "/auth/callback":
    fail("OIDC callback does not exactly match the production origin")
if not re.fullmatch(r"[0-9a-f]{64}", environment["NOISEDECK_DEPLOYMENT_HOST_SHA256"]):
    fail("production host fingerprint is not a lowercase SHA-256 digest")

mounts = app.get("volumes", [])
palace_mounts = [mount for mount in mounts if mount.get("target") == "/home/agent/.mempalace"]
if (
    len(palace_mounts) != 1
    or palace_mounts[0].get("type") != "bind"
    or not str(palace_mounts[0].get("source", "")).startswith("/")
):
    fail("durable MemPalace bind mount is missing")

tmp_mounts = [mount for mount in mounts if mount.get("target") == "/tmp"]
if (
    len(tmp_mounts) != 1
    or tmp_mounts[0].get("type") != "bind"
    or not str(tmp_mounts[0].get("source", "")).startswith("/")
    or tmp_mounts[0].get("read_only", False)
):
    fail("writable disk-backed /tmp bind mount is missing")
for tmpfs in app.get("tmpfs", []):
    if str(tmpfs).split(":", 1)[0] == "/tmp":
        fail("production /tmp must not be RAM-backed tmpfs")

ports = app.get("ports", [])
console = [p for p in ports if p.get("target") == 8837]
relay = [p for p in ports if p.get("target") == 8766]
if len(ports) != 2 or len(console) != 1 or len(relay) != 1 or not console[0].get("host_ip"):
    fail("production publishes exactly two ports: the console on an explicit host address (target 8837) and the relay palace (target 8766)")
try:
    publish_address = ipaddress.ip_address(console[0]["host_ip"])
except ValueError:
    fail("production publish address is not an IP address")
if publish_address.is_unspecified:
    fail("production publish address must not be a wildcard")
# The relay is for the tailnet proxy on the same host: loopback only, so
# nothing on the network reaches the relay's own listener.
if relay[0].get("host_ip") != "127.0.0.1":
    fail("the relay palace must be published on 127.0.0.1 only")

if browser.get("network_mode") != "service:app":
    fail("Chromium does not share the app network namespace")

print("production-compose: rendered capability profile verified")
