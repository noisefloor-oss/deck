#!/usr/bin/env python3
"""Exercise resolver labeling on an enforcing SELinux Docker host, using only fixtures."""

import argparse
import json
import pathlib
import shutil
import subprocess
import tempfile
import time
import uuid


def docker(*args, check=True):
    return subprocess.run(["docker", *args], check=check, capture_output=True, text=True)


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--image", default="noise-browser:1.64.1-chromium-156.0.8078.47", help="already-built browser image; never pulled")
    args = parser.parse_args()
    if subprocess.check_output(["getenforce"], text=True).strip() != "Enforcing":
        raise SystemExit("This qualification requires enforcing SELinux; not a skipped pass")
    docker("image", "inspect", args.image)
    network = "deck-dns-test-" + uuid.uuid4().hex[:12]
    owned = []

    def inspect(name):
        return json.loads(docker("inspect", name).stdout)[0]

    def create(name, *options):
        docker("run", "-d", "--name", name, "--label", "noise.test=sidecar-dns",
               "--cap-drop=ALL", "--security-opt", "no-new-privileges:true", "--read-only",
               *options, "--entrypoint", "sleep", args.image, "600")
        owned.append(name)
        result = inspect(name)
        require(result["ProcessLabel"], "Docker is not labeling containers")
        return result

    def remove(name):
        docker("rm", "-f", name)
        owned.remove(name)

    lookup = "require('node:fs').readFileSync('/etc/resolv.conf'); require('node:dns').lookup('example.com',(e)=>process.exit(e?1:0))"
    with tempfile.TemporaryDirectory(prefix="deck-dns-acceptance-", dir="/var/tmp") as directory:
        root = pathlib.Path(directory)
        shutil.copyfile(pathlib.Path(__file__).with_name("sidecar-resolv.conf"), root / "resolver")
        (root / "marker").write_text("synthetic private marker\n")
        for name in ("resolver", "marker"):
            (root / name).chmod(0o644)
        docker("network", "create", network)
        try:
            for generation in range(2):
                app, sidecar, connector, control, outsider = [network + "-" + role for role in ("app", "browser", "connector", "control", "outsider")]
                app_state = create(app, "--network", network, "--health-cmd", "cat /etc/resolv.conf >/dev/null",
                                   "--health-interval", "1s", "--health-retries", "2",
                                   "-v", str(root / "marker") + ":/private-marker:ro,Z")
                sidecar_state = create(sidecar, "--network", "container:" + app,
                                       "-v", str(root / "resolver") + ":/etc/resolv.conf:ro,z")
                connector_state = create(connector, "--network", "container:" + app, "--user", "65532:65532",
                                         "-v", str(root / "resolver") + ":/etc/resolv.conf:ro,z")
                require(len({app_state["ProcessLabel"], sidecar_state["ProcessLabel"], connector_state["ProcessLabel"]}) == 3,
                        "Network-sharing containers must retain distinct process labels")
                create(control, "--network", "container:" + app)
                # Docker's archive path relabels the app's managed resolver privately.
                docker("cp", app + ":/etc/hostname", str(root / "copied-hostname"))
                rejected = docker("exec", control, "node", "-e", lookup, check=False)
                require(rejected.returncode != 0 and "EACCES" in rejected.stderr and "/etc/resolv.conf" in rejected.stderr,
                        "Control did not reproduce the resolver permission fault")
                remove(control)
                for _ in range(4):
                    docker("cp", app + ":/etc/hostname", str(root / "copied-hostname"))
                    docker("exec", app, "node", "-e", lookup)
                    docker("exec", sidecar, "node", "-e", lookup)
                    docker("exec", connector, "node", "-e", lookup)
                    time.sleep(1.1)
                require(inspect(app)["State"]["Health"]["Status"] == "healthy", "App health check failed")
                outsider_state = create(outsider, "--network", network, "-v", str(root / "marker") + ":/private-marker:ro")
                require(outsider_state["ProcessLabel"] != app_state["ProcessLabel"], "Outsider shares the app process label")
                rejected = docker("exec", outsider, "cat", "/private-marker", check=False)
                require(rejected.returncode != 0 and "Permission denied" in rejected.stderr,
                        "Outsider private-marker read was not refused")
                docker("exec", app, "cat", "/private-marker")
                require(docker("exec", sidecar, "test", "-e", "/private-marker", check=False).returncode != 0,
                        "Browser unexpectedly mounts app private data")
                require(docker("exec", connector, "test", "-e", "/private-marker", check=False).returncode != 0,
                        "Connector unexpectedly mounts app private data")
                print(json.dumps({"generation": generation + 1, "control_failed": True,
                                  "DNS_after_copy_exec_and_healthchecks": "passed",
                                  "distinct_labels": True, "private_marker_isolation": "passed"}), flush=True)
                for name in list(reversed(owned)):
                    remove(name)
        finally:
            for name in reversed(owned):
                docker("rm", "-f", name, check=False)
            docker("network", "rm", network)


if __name__ == "__main__":
    main()
