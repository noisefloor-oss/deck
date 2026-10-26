#!/usr/bin/env python3
"""Publish and pre-cache complete, digest-addressed Deck release sets."""
import argparse
import fcntl
import hashlib
import json
import os
import pathlib
import re
import subprocess
import sys
import tempfile
import urllib.error
import urllib.request

LABEL = "com.noisefloor.deck.release"
FONT_LABEL = "com.noisefloor.deck.font-files"
SHA = re.compile(r"[0-9a-f]{40}")
DIGEST = re.compile(r"sha256:[0-9a-f]{64}")
BUILD_COMPOSE_FILES = ("compose.yaml", "compose.mempalace.yaml", "compose.go.yaml",
                       "compose.go-mempalace.yaml", "compose.browser.yaml")
LICENSED_COMPOSE_FILES = ("compose.licensed-fonts.yaml", "compose.go-licensed-fonts.yaml")


def run(*args, input=None, env=None):
    return subprocess.check_output(args, input=input, text=True, env=env, stderr=subprocess.PIPE).strip()


def inspect(image):
    return json.loads(run("docker", "image", "inspect", image))[0]


def registry(value):
    if not re.fullmatch(r"ghcr\.io/[a-z0-9][a-z0-9-]*", value):
        raise ValueError("Expected an explicit ghcr.io owner")
    return value


def package(prefix, role):
    if not re.fullmatch(r"[a-z0-9][a-z0-9-]*", prefix):
        raise ValueError("Invalid release package prefix")
    if role not in ("free", "licensed", "browser", "catalog"):
        raise ValueError("Unknown release role")
    return prefix + "-" + role


def reference(reg, prefix, role):
    return registry(reg) + "/" + package(prefix, role)


def validate(document, reg, prefix, require_licensed=False):
    if not isinstance(document, dict):
        raise ValueError("Release metadata must be an object")
    if document.get("schema") != 1 or not SHA.fullmatch(document.get("sha", "")):
        raise ValueError("Invalid release schema or source SHA")
    if any(type(document.get(k)) is not int or not 0 < document[k] < 2**31 for k in ("uid", "gid")):
        raise ValueError("Invalid runtime identity")
    for key in ("playwright", "mempalace", "go"):
        if not re.fullmatch(r"[0-9]+\.[0-9]+\.[0-9]+", document.get(key, "")):
            raise ValueError("Missing exact capability version: " + key)
    roles = set(document.get("images", {}))
    if roles not in ({"free", "browser"}, {"free", "licensed", "browser"}):
        raise ValueError("Incomplete release set")
    if require_licensed and "licensed" not in roles:
        raise ValueError("Installation requires a licensed image")
    for role, ref in document["images"].items():
        name, sep, digest = ref.partition("@")
        if name != reference(reg, prefix, role) or sep != "@" or not DIGEST.fullmatch(digest):
            raise ValueError("Image is not an immutable reference in this release namespace")
    fonts = document.get("font_files", {})
    if "licensed" in roles and (not fonts or any(not re.fullmatch(r"[^/]+\.woff2", n) or not re.fullmatch(r"[0-9a-f]{64}", h) for n, h in fonts.items())):
        raise ValueError("Licensed font fingerprints are missing or invalid")
    return document


def font_files(directory):
    root = pathlib.Path(directory)
    files = sorted(root.glob("*.woff2"))
    if not files or any(p.is_symlink() or not p.is_file() for p in files):
        raise ValueError("Expected regular licensed WOFF2 files")
    return {p.name: hashlib.sha256(p.read_bytes()).hexdigest() for p in files}


def font_context_files(image):
    expected = json.loads(inspect(image)["Config"]["Labels"][FONT_LABEL])
    # Never start the scratch input image; read its immutable filesystem.
    with tempfile.TemporaryDirectory(prefix="deck-font-probe-") as directory:
        container = run("docker", "create", "--entrypoint", "/not-executed", image)
        try:
            run("docker", "cp", container + ":/.", directory)
            actual = font_files(directory)
            if actual != expected:
                raise ValueError("Private font input label disagrees with its actual file bytes")
            return actual
        finally:
            run("docker", "rm", container)


def api(path, method="GET", missing_ok=False):
    req = urllib.request.Request("https://api.github.com/" + path, method=method, headers={
        "Authorization": "Bearer " + os.environ["GH_TOKEN"],
        "Accept": "application/vnd.github+json",
        "X-GitHub-Api-Version": "2022-11-28",
    })
    try:
        with urllib.request.urlopen(req, timeout=30) as response:
            body = response.read()
            return json.loads(body) if body else None
    except urllib.error.HTTPError as error:
        error.close()
        if missing_ok and error.code == 404:
            return None
        raise RuntimeError("GitHub API " + method + " " + path + " returned HTTP " + str(error.code)) from None


def private_package(reg, name, missing_ok=False):
    owner = registry(reg).split("/")[1]
    result = api("orgs/" + owner + "/packages/container/" + name, missing_ok=missing_ok)
    if result is not None and result.get("visibility") != "private":
        raise ValueError("Refusing to publish licensed-capability images to a non-private package")
    return result


def push(image):
    output = run("docker", "push", image)
    digests = re.findall(r"digest: (sha256:[0-9a-f]{64})", output)
    if not digests:
        raise RuntimeError("Registry did not return a pushed digest")
    return image.rsplit(":", 1)[0] + "@" + digests[-1]


def catalog(reg, prefix, tag):
    image = reference(reg, prefix, "catalog") + ":" + tag
    try:
        subprocess.run(["docker", "pull", image], check=True, capture_output=True, text=True)
    except subprocess.CalledProcessError as error:
        # Only a missing manifest is absence. Auth/network failures fail closed;
        # the host needs registry read credentials, not a GitHub API token.
        detail = error.stderr.lower()
        if "manifest unknown" in detail or "manifest not found" in detail or image + ": not found" in detail:
            return None
        raise
    info = inspect(image)
    doc = validate(json.loads(info["Config"]["Labels"][LABEL]), reg, prefix)
    ref = next(r for r in info["RepoDigests"] if r.startswith(reference(reg, prefix, "catalog") + "@"))
    return doc, ref


def probe(document, role):
    ref = document["images"][role]
    info = inspect(ref)
    if info["Os"] != "linux" or info["Architecture"] != "amd64":
        raise ValueError("Deployment image must be linux/amd64")
    labels = info["Config"].get("Labels") or {}
    if labels.get(LABEL + ".sha") != document["sha"] or labels.get(LABEL + ".role") != role:
        raise ValueError("Image source or variant label disagrees with the release set")
    command = ["docker", "run", "--rm", "--network", "none", "--read-only",
               "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
               "--pids-limit", "128", "--memory", "512m", "--cpus", "1"]
    if role == "browser":
        version = run(*command, "--entrypoint", "node", ref, "-p",
                      "require('/opt/noise-browser/node_modules/playwright/package.json').version")
        if version != document["playwright"]:
            raise ValueError("Browser/client version mismatch")
        return
    if info["Config"]["User"] != str(document["uid"]) + ":" + str(document["gid"]):
        raise ValueError("Image runtime UID/GID mismatch")
    if run(*command, "--entrypoint", "noise", ref, "deck", "version") != document["sha"]:
        raise ValueError("Image contains a different Deck build")
    output = run(*command, "--entrypoint", "sh", ref, "-lc", "mempalace --version; go version")
    if "MemPalace " + document["mempalace"] not in output or "go" + document["go"] + " " not in output:
        raise ValueError("Production MemPalace/Go capabilities are missing")


def retained_versions(documents, catalogs, reg, prefix):
    keep = {role: set() for role in ("free", "licensed", "browser", "catalog")}
    pending = []
    for doc in documents:
        pending.extend(doc["images"].items())
    pending.extend(("catalog", ref) for ref in catalogs)
    # Preserve child manifests and attestations of retained OCI indexes too.
    while pending:
        role, ref = pending.pop()
        digest = ref.partition("@")[2]
        if digest in keep[role]:
            continue
        if not DIGEST.fullmatch(digest) or len(keep[role]) >= 32:
            raise ValueError("Invalid or unexpectedly large manifest graph")
        keep[role].add(digest)
        raw = json.loads(run("docker", "buildx", "imagetools", "inspect", "--raw", ref))
        for child in raw.get("manifests", []):
            pending.append((role, reference(reg, prefix, role) + "@" + child["digest"]))
    return keep


def prune_registry(documents, catalogs, reg, prefix):
    keep = retained_versions(documents, catalogs, reg, prefix)
    owner = registry(reg).split("/")[1]
    for role, digests in keep.items():
        name = package(prefix, role)
        if private_package(reg, name, missing_ok=True) is None:
            continue
        versions = []
        page = 1
        while True:
            batch = api("orgs/" + owner + "/packages/container/" + name + "/versions?per_page=100&page=" + str(page))
            versions.extend(batch)
            if len(batch) < 100:
                break
            page += 1
        for version in versions:
            if version["name"] not in digests:
                api("orgs/" + owner + "/packages/container/" + name + "/versions/" + str(version["id"]), method="DELETE")


def write_build_env(root):
    # Compose interpolates runtime mounts even for `build`. This path is only
    # a placeholder: publication never starts services or mounts a palace.
    envfile = root / "compose.env"
    envfile.write_text("NOISEDECK_IMAGE_TAG=release-image-build\n"
                       "NOISEDECK_MEMPALACE_HOST_PATH=" + str(root / "unused-mempalace") + "\n")
    return envfile


def can_promote(repository, sha, previous_sha=None):
    # A completed accepted build remains useful when main advances. Refuse
    # rewritten mainline ancestry or a downgrade of the published catalog.
    for base, head in [(sha, "main")] + ([(previous_sha, sha)] if previous_sha else []):
        if api("repos/" + repository + "/compare/" + base + "..." + head)["status"] not in ("ahead", "identical"):
            return False
    return True


def publication_receipt(args, document):
    destination = getattr(args, "receipt", "")
    if not destination:
        return
    record = {"version": 1, "repository": os.environ["GITHUB_REPOSITORY"],
              "run": os.environ["GITHUB_RUN_ID"], "attempt": os.environ["GITHUB_RUN_ATTEMPT"],
              "sha": args.sha, "image_set": document}
    with open(destination, "x") as output:
        output.write(json.dumps(record, sort_keys=True) + "\n")


def publish(args):
    if any(type(value) is not int or not 0 < value < 2**31 for value in (args.uid, args.gid)):
        raise ValueError("Invalid runtime identity")
    if not SHA.fullmatch(args.sha) or run("git", "rev-parse", "HEAD") != args.sha:
        raise ValueError("Build checkout does not match requested source")
    if run("git", "status", "--porcelain"):
        raise ValueError("Release source checkout is dirty")
    reg = registry(args.registry)
    repository = os.environ.get("GITHUB_REPOSITORY", "")
    if not re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", repository):
        raise ValueError("Publication requires an explicit GitHub source repository")
    source_label = "https://github.com/" + repository
    fonts = {}
    if args.font_context:
        if not re.fullmatch(re.escape(reg) + r"/[a-z0-9-]+@sha256:[0-9a-f]{64}", args.font_context):
            raise ValueError("Licensed font context must be a pinned private GHCR image")
        private_package(reg, args.font_context.split("/")[-1].split("@")[0])
        run("docker", "pull", args.font_context)
        fonts = font_context_files(args.font_context)
        expected = set(re.findall(r"/fonts/([^\s\"')]+\.woff2)", pathlib.Path("web/private/fonts/licensed.css").read_text()))
        if set(fonts) != expected:
            raise ValueError("Private font context does not match this checkout's required asset set")
    if args.require_licensed and not fonts:
        raise ValueError("Licensed deployment publication requires its private font context")
    roles = ["free", "licensed"] if fonts else ["free"]
    for role in [*roles, "browser", "catalog"]:
        private_package(reg, package(args.prefix, role), missing_ok=True)
    # The Compose sidecar pin is checked against the frozen lock by deploy tests;
    # never turn an npm range into an asserted resolved version.
    playwright = re.search(r"image: noise-browser:([0-9.]+)", pathlib.Path("compose.browser.yaml").read_text()).group(1)
    mempalace = re.search(r"^ARG MEMPALACE_VERSION=([0-9.]+)$", pathlib.Path("Dockerfile").read_text(), re.M).group(1)
    go = re.search(r"^ARG GO_VERSION=([0-9.]+)$", pathlib.Path("deploy/derived/go.Dockerfile").read_text(), re.M).group(1)
    doc = {"schema": 1, "sha": args.sha, "uid": args.uid, "gid": args.gid,
           "playwright": playwright, "mempalace": mempalace, "go": go,
           "font_files": fonts, "images": {}}
    old = catalog(reg, args.prefix, "latest") if private_package(reg, package(args.prefix, "catalog"), missing_ok=True) else None
    if old and all(old[0].get(key) == value for key, value in doc.items() if key != "images"):
        validate(old[0], reg, args.prefix, args.require_licensed)
        previous = catalog(reg, args.prefix, "previous")
        prune_registry([old[0]] + ([previous[0]] if previous else []),
                       [old[1]] + ([previous[1]] if previous else []), reg, args.prefix)
        publication_receipt(args, old[0])
        print("Exact verified release set already published: " + args.sha, flush=True)
        return
    env = dict(os.environ, NOISEDECK_BUILD_VERSION=args.sha,
               NOISEDECK_RUNTIME_UID=str(args.uid), NOISEDECK_RUNTIME_GID=str(args.gid))
    files = BUILD_COMPOSE_FILES
    with tempfile.TemporaryDirectory(prefix="deck-release-images-") as directory:
        root = pathlib.Path(directory)
        envfile = write_build_env(root)
        built = {}
        for role in roles:
            env["NOISEDECK_FONTS"] = role
            extra = []
            if role == "licensed":
                env["NOISEDECK_LICENSED_FONTS_DIR"] = "docker-image://" + args.font_context
                extra = LICENSED_COMPOSE_FILES
            app = reference(reg, args.prefix, role) + ":sha-" + args.sha
            browser = reference(reg, args.prefix, "browser") + ":sha-" + args.sha
            override = root / "images.json"
            override.write_text(json.dumps({"services": {
                "app": {"image": app, "build": {"labels": {LABEL + ".sha": args.sha, LABEL + ".role": role, "org.opencontainers.image.source": source_label}}},
                "chromium": {"image": browser, "build": {"labels": {LABEL + ".sha": args.sha, LABEL + ".role": "browser", "org.opencontainers.image.source": source_label}}}
            }}))
            command = ["docker", "compose", "--project-name", "deck-release-images", "--env-file", str(envfile)]
            for file in [*files, *extra, str(override)]:
                command += ["--file", file]
            subprocess.run([*command, "build", "--pull", "app", "chromium"], env=env, check=True)
            built[role] = app
            built["browser"] = browser
        # Probe every complete candidate before publishing a catalog pointer.
        for role, image in built.items():
            candidate = dict(doc, images={role: image})
            probe(candidate, role)
        for role, image in built.items():
            doc["images"][role] = push(image)
            private_package(reg, package(args.prefix, role))
        validate(doc, reg, args.prefix, args.require_licensed)
        catalog_tag = reference(reg, args.prefix, "catalog") + ":set-" + args.sha
        dockerfile = "FROM scratch\nLABEL org.opencontainers.image.source=" + json.dumps(source_label) + "\nLABEL " + LABEL + "=" + json.dumps(json.dumps(doc, sort_keys=True)) + "\n"
        run("docker", "build", "--tag", catalog_tag, "--file", "-", str(root), input=dockerfile)
        catalog_ref = push(catalog_tag)
        private_package(reg, package(args.prefix, "catalog"))
        if not can_promote(repository, args.sha, old[0]["sha"] if old else None):
            print("Candidate is outside current mainline or behind the published catalog; no promotion", flush=True)
            return
        if old and old[0] != doc:
            run("docker", "tag", old[1], reference(reg, args.prefix, "catalog") + ":previous")
            push(reference(reg, args.prefix, "catalog") + ":previous")
        run("docker", "tag", catalog_ref, reference(reg, args.prefix, "catalog") + ":latest")
        push(reference(reg, args.prefix, "catalog") + ":latest")
        previous = catalog(reg, args.prefix, "previous")
        documents = [doc] + ([previous[0]] if previous else [])
        catalogs = [catalog_ref] + ([previous[1]] if previous else [])
        prune_registry(documents, catalogs, reg, args.prefix)
    publication_receipt(args, doc)
    print("Published and verified release set " + args.sha, flush=True)


def write_state(root, sets, pending):
    temporary = root / "sets.json.pending"
    temporary.write_text(json.dumps({"sets": sets, "pending_eviction": pending}, sort_keys=True) + "\n")
    temporary.chmod(0o600)
    temporary.replace(root / "sets.json")


def evict(pending, reg, prefix):
    for ref in pending:
        name, sep, digest = ref.partition("@")
        if sep != "@" or not DIGEST.fullmatch(digest) or name not in {reference(reg, prefix, r) for r in ("free", "licensed", "browser")}:
            raise ValueError("Unexpected pending eviction reference")
    try:
        names = run("docker", "ps", "--all", "--quiet").splitlines()
        # A disappeared probe container cannot invalidate an admitted cache.
        in_use = set()
        for name in names:
            try:
                in_use.add(json.loads(run("docker", "inspect", name))[0]["Image"])
            except subprocess.CalledProcessError:
                return pending
    except subprocess.CalledProcessError:
        return pending
    remaining = []
    for ref in pending:
        try:
            if inspect(ref)["Id"] in in_use:
                remaining.append(ref)
                continue
            # No force: Docker also protects containers created since the snapshot.
            run("docker", "image", "rm", ref)
        except subprocess.CalledProcessError as error:
            if "no such image" not in (error.stderr or "").lower():
                remaining.append(ref)
    return remaining


def prefetch(args):
    root = pathlib.Path(args.state_dir)
    root.mkdir(mode=0o700, parents=True, exist_ok=True)
    with (root / "lock").open("a") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        old = json.loads((root / "sets.json").read_text()) if (root / "sets.json").exists() else {"sets": []}
        for doc in old["sets"]:
            validate(doc, args.registry, args.prefix)
        current = catalog(args.registry, args.prefix, "latest")
        if current is None:
            raise ValueError("No verified release catalog has been published")
        previous = catalog(args.registry, args.prefix, "previous")
        if previous and args.require_licensed and "licensed" not in previous[0]["images"]:
            previous = None
        sets = [current[0]] + ([previous[0]] if previous and previous[0] != current[0] else [])
        for doc in sets:
            validate(doc, args.registry, args.prefix, args.require_licensed)
        if sets == old["sets"]:
            try:
                for doc in sets:
                    for ref in doc["images"].values():
                        inspect(ref)
                print("Verified release sets are already cached", flush=True)
                pending = evict(old.get("pending_eviction", []), args.registry, args.prefix)
                write_state(root, sets, pending)
                return
            except subprocess.CalledProcessError:
                pass
        for doc in sets:
            validate(doc, args.registry, args.prefix, args.require_licensed)
            for role, ref in doc["images"].items():
                run("docker", "pull", ref)
                probe(doc, role)
        retained = {ref for doc in sets for ref in doc["images"].values()}
        pending = sorted(({ref for doc in old["sets"] for ref in doc["images"].values()} |
                          set(old.get("pending_eviction", []))) - retained)
        write_state(root, sets, pending)
        write_state(root, sets, evict(pending, args.registry, args.prefix))
        print("Pre-cached complete sets: " + ", ".join(doc["sha"] for doc in sets), flush=True)


def select(args):
    rendered = json.loads(pathlib.Path(args.compose).read_text())
    app = rendered["services"]["app"]
    build_args = app["build"]["args"]
    sha = build_args["VERSION"]
    role = build_args["NOISEDECK_FONTS"]
    if role not in ("free", "licensed") or not SHA.fullmatch(sha):
        raise ValueError("Cached deployments require explicit font selection and exact source SHA")
    verify_source(args.checkout, sha)
    uid, gid = (int(v) for v in app["user"].split(":"))
    state = json.loads(pathlib.Path(args.state_dir, "sets.json").read_text())
    selected = None
    for doc in state["sets"]:
        validate(doc, args.registry, args.prefix, role == "licensed")
        if (doc["sha"], doc["uid"], doc["gid"]) == (sha, uid, gid):
            if role == "licensed" and (not args.font_dir or font_files(args.font_dir) != doc["font_files"]):
                continue
            selected = doc
            break
    if selected is None:
        raise ValueError("Exact source/identity/font-input set is not pre-cached; no build fallback")
    for candidate in (role, "browser"):
        probe(selected, candidate)
    run("docker", "tag", selected["images"][role], app["image"])
    run("docker", "tag", selected["images"]["browser"], rendered["services"]["chromium"]["image"])
    print("Selected pre-cached " + role + " image " + selected["images"][role], flush=True)


def verify_source(checkout, sha):
    if run("git", "-C", checkout, "rev-parse", "HEAD") != sha:
        raise ValueError("Deployment checkout does not match the cached image source")
    if run("git", "-C", checkout, "status", "--porcelain"):
        raise ValueError("Deployment checkout is dirty")


def main():
    parser = argparse.ArgumentParser()
    sub = parser.add_subparsers(dest="action", required=True)
    for action in ("publish", "prefetch", "select"):
        cmd = sub.add_parser(action)
        cmd.add_argument("--registry", required=True, type=registry)
        cmd.add_argument("--prefix", default="deck-deploy")
        if action == "publish":
            cmd.add_argument("--sha", required=True)
            cmd.add_argument("--receipt", default="")
            cmd.add_argument("--uid", required=True, type=int)
            cmd.add_argument("--gid", required=True, type=int)
            cmd.add_argument("--font-context", default="")
            cmd.add_argument("--require-licensed", action="store_true")
        else:
            cmd.add_argument("--state-dir", required=True)
            if action == "prefetch":
                cmd.add_argument("--require-licensed", action="store_true")
            else:
                cmd.add_argument("--compose", required=True)
                cmd.add_argument("--checkout", required=True)
                cmd.add_argument("--font-dir", default="")
    args = parser.parse_args()
    {"publish": publish, "prefetch": prefetch, "select": select}[args.action](args)


if __name__ == "__main__":
    try:
        main()
    except subprocess.CalledProcessError as error:
        if error.stderr:
            print(error.stderr, file=sys.stderr)
        raise SystemExit(error.returncode) from None
