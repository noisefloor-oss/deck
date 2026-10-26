#!/usr/bin/env python3
"""Select PR and merge-queue checks from the complete local Git diff."""

import json
import os
from pathlib import Path, PurePosixPath
import re
import subprocess


SUITES = (
    "documentation", "frontend", "probes", "lint", "go-serialized",
    "go-parallel", "deploy-suite", "experiment-enabled", "jail-probe",
    "source-export",
)
GO = {"lint", "go-serialized", "go-parallel", "experiment-enabled"}
WEB = {"frontend", "probes"}
SECURITY = (
    "internal/auth", "internal/harnesslogin/", "internal/projectpolicy/",
    "internal/supervisor/", "internal/workspace", "internal/sealedstate/",
    "internal/terminal/", "internal/hookcapture/", "internal/relay/", "internal/continuity/",
    "web/src/security/", "web/src/app/security/", "web/src/api/contracts.ts",
    "cmd/noisedeck-harness-bump/", "internal/harnessbump/", "internal/ciheal/",
    "internal/apppreview/", "internal/project/", "internal/githubconnect/",
    "internal/githubrepos/", "internal/provider/", "internal/db/", "cmd/noisedeck-provision/",
    "internal/inputgate/", "internal/lease/", "internal/boot/", "internal/server/",
    "internal/session/", "docs/security/", "docs/adr/", "docs/provider-contracts/",
    "docs/PRINCIPLES.md",
)
PROBE_SHARDS = [1, 2, 3, "gateway", "previews"]
SHA = re.compile(r"[0-9a-f]{40}")


def full(reason):
    return {"mode": "full", "jobs": list(SUITES), "probe_shards": PROBE_SHARDS, "reason": reason}


def classify(paths):
    if not paths:
        return full("Empty diff: use full validation")
    jobs = {"documentation"}
    categories = set()
    probe_shards = [1, 2, 3]
    for path in paths:
        normalized = PurePosixPath(path)
        if (not path or str(normalized) != path or normalized.is_absolute()
                or ".." in normalized.parts or any(ord(c) < 32 for c in path)):
            return full("Unusual path: use full validation")
        if normalized.name in ("AGENTS.md", "SDLC.md"):
            jobs.add("deploy-suite")
            categories.add("policy")
            continue
        if path.startswith(("docs/security/", "docs/adr/", "docs/provider-contracts/")) or path == "docs/PRINCIPLES.md":
            jobs.update(GO | {"deploy-suite"})
            categories.add("tested policy inputs")
            continue
        if path.endswith(".md") and (
                path in ("README.md", "CHANGELOG.md", "CONTRIBUTING.md", "SECURITY.md")
                or path.startswith(("docs/", "skills/"))):
            categories.add("documentation")
        elif path.startswith("web/") and not path.endswith(".go"):
            jobs.update(WEB)
            if (path.startswith(("web/src/security/", "web/src/app/security/", "web/src/api/", "web/src/app/project-preview"))
                    or path.startswith(("web/tools/project-app-preview", "web/tools/security-go", "web/tools/onboarding-go"))
                    or not path.startswith(("web/src/", "web/public/", "web/tools/"))):
                probe_shards = PROBE_SHARDS
            if path in ("web/package.json", "web/pnpm-lock.yaml") or path.startswith(("web/tools/security-go", "web/tools/onboarding-go")):
                jobs.add("deploy-suite")
            categories.add("frontend")
        elif path.endswith(".go") and path.startswith(("cmd/", "internal/")):
            jobs.update(GO)
            if path.startswith(SECURITY):
                jobs.update(WEB)
                probe_shards = PROBE_SHARDS
            if path.startswith(("internal/harnessbump/", "internal/ciheal/", "cmd/noisedeck-harness-bump/")):
                jobs.update({"jail-probe", "deploy-suite"})
            categories.add("backend")
        elif path.startswith("web/"):
            jobs.update(GO | WEB)
            probe_shards = PROBE_SHARDS
            categories.add("browser fixtures")
        elif path.startswith((".github/", "deploy/", "scripts/")):
            jobs.add("deploy-suite")
            if path.startswith(".github/workflows/"):
                return full("Workflow changed: use full validation")
            if path.endswith(".go"):
                jobs.update(GO)
            if path.startswith(("scripts/harness-bump/", "scripts/ciheal/")):
                jobs.update(GO | {"jail-probe"})
            if path.startswith("deploy/export/"):
                jobs.add("source-export")
            categories.add("operations")
        elif path.startswith(".visual-diff/"):
            jobs.update(WEB)
            categories.add("visual fixtures")
        else:
            return full("Build, CI, dependency or unrecognized path changed")
    return {"mode": "selective", "jobs": [s for s in SUITES if s in jobs], "probe_shards": probe_shards,
            "reason": "Changed areas: " + ", ".join(sorted(categories))}


def changed_paths(base, head):
    if not SHA.fullmatch(base) or not SHA.fullmatch(head):
        raise ValueError("Invalid commit identity")
    bases = subprocess.check_output(
        ["git", "merge-base", "--all", base, head], stderr=subprocess.DEVNULL,
        text=True).splitlines()
    if len(bases) != 1:
        raise ValueError("Ambiguous merge base")
    raw = subprocess.check_output([
        "git", "diff", "--no-ext-diff", "--no-textconv", "--no-renames",
        "--raw", "-z", bases[0], head, "--"], stderr=subprocess.DEVNULL)
    fields = raw.split(b"\0")
    if fields[-1] != b"" or (len(fields) - 1) % 2:
        raise ValueError("Malformed diff")
    paths = []
    for index in range(0, len(fields) - 1, 2):
        metadata = fields[index].decode("ascii").split()
        if (len(metadata) != 5 or metadata[0] not in (":100644", ":000000")
                or metadata[1] not in ("100644", "000000")
                or metadata[4] not in ("A", "D", "M")):
            raise ValueError("Unsupported file mode or change kind")
        paths.append(fields[index + 1].decode("utf-8"))
    return paths


def plan(event_name, event):
    if event_name not in ("pull_request", "merge_group"):
        return full("Manual or non-change event: full validation")
    try:
        if event_name == "merge_group":
            group = event["merge_group"]
            paths = changed_paths(group["base_sha"], group["head_sha"])
        else:
            pr = event["pull_request"]
            if pr["head"]["repo"]["full_name"] != pr["base"]["repo"]["full_name"]:
                return full("Fork PR: retain complete exported-source validation")
            paths = changed_paths(pr["base"]["sha"], pr["head"]["sha"])
        result = classify(paths)
        result["path_count"] = len(paths)
        if result["mode"] != "full":
            # The floor beneath Jev is the deterministic classification of
            # every changed path, not only the security ones: the advisor
            # once read a help-screen and PTY-fixture change as docs-only
            # and the affected code suites never ran. Jev may add suites it
            # is confident about; it may never drop a suite the diff's own
            # paths demand.
            result["floor"] = {"mode": result["mode"], "jobs": result["jobs"],
                               "probe_shards": result["probe_shards"], "reason": result["reason"]}
        return result
    except (KeyError, TypeError, ValueError, UnicodeError, OSError,
            subprocess.SubprocessError):
        return full("Diff unavailable or unsupported: full validation")


def publish(result):
    if output := os.environ.get("GITHUB_OUTPUT"):
        with open(output, "a", encoding="utf-8") as stream:
            stream.write("mode=" + result["mode"] + "\n")
            stream.write("jobs=" + json.dumps(result["jobs"]) + "\n")
            stream.write("probe_shards=" + json.dumps(result["probe_shards"]) + "\n")
    if summary := os.environ.get("GITHUB_STEP_SUMMARY"):
        with open(summary, "a", encoding="utf-8") as stream:
            stream.write("## CI scope\n\n" + result["reason"] + ".\n\n")
            stream.write("Selected checks: " + ", ".join(result["jobs"]) + ".\n")
            stream.write("\nUnselected checks must report skipped; selected checks must pass.\n")
    print(json.dumps(result, sort_keys=True))


def with_floor(selected, floor):
    jobs = set(selected["jobs"]) | set(floor["jobs"])
    shards = set()
    for chosen in (selected, floor):
        if "probes" in chosen["jobs"]:
            shards.update(chosen["probe_shards"])
    return dict(selected, jobs=[s for s in SUITES if s in jobs],
                probe_shards=[s for s in PROBE_SHARDS if s in shards] or selected["probe_shards"])


def select_plan(event_name, event):
    pr = event.get("pull_request") or {}
    head_repo = (pr.get("head") or {}).get("repo") or {}
    base_repo = (pr.get("base") or {}).get("repo") or {}
    if (event_name == "pull_request"
            and (event.get("repository") or {}).get("private") is True
            and head_repo.get("full_name")
            and head_repo.get("full_name") == base_repo.get("full_name")):
        return {"mode": "selective", "jobs": ["documentation"],
                "probe_shards": [1, 2, 3], "selector": "pr-preflight",
                "reason": "Private PR preflight only; regression suites run on the exact merge-queue revision"}
    fallback = plan(event_name, event)
    floor = fallback.pop("floor", None)
    if event_name != "merge_group" or "path_count" not in fallback or fallback["mode"] == "full":
        return fallback
    if not os.environ.get("GITHUB_RUN_ID") or not os.environ.get("GITHUB_REPOSITORY"):
        return fallback
    if (event.get("repository") or {}).get("private") is not True:
        return fallback
    import jev_plan
    base, head = event["merge_group"]["base_sha"], event["merge_group"]["head_sha"]
    selected = jev_plan.load(os.environ["GITHUB_REPOSITORY"], os.environ["GITHUB_RUN_ID"], event_name, base, head)
    if selected is not None:
        if floor:
            selected = with_floor(selected, floor)
        selected["path_count"] = fallback["path_count"]
        return selected
    fallback["reason"] = "Jev plan unavailable; Python fallback. " + fallback["reason"]
    fallback["selector"] = "python-fallback"
    return fallback


if __name__ == "__main__":
    try:
        event = json.loads(Path(os.environ["GITHUB_EVENT_PATH"]).read_text())
    except (KeyError, OSError, ValueError):
        event = {}
    publish(select_plan(os.environ.get("GITHUB_EVENT_NAME", ""), event))
