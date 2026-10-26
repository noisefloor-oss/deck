#!/usr/bin/env python3
"""Freeze a private Deck candidate and inspect readiness without publishing."""
import argparse
import datetime as dt
import json
import io
import zipfile
from pathlib import Path
import re
import subprocess

WORKFLOWS = ("ci-fast.yaml", "release-acceptance.yaml", "release-images.yaml")
FULL_FAST_JOBS = frozenset({
    "scope", "documentation", "lint", "frontend", "go-parallel", "go-serialized",
    "deploy-suite", "experiment-enabled", "jail-probe", "fast-checks",
    "source-export (parallel)", "source-export (serialized)", "source-export (deploy)",
    "probes (1)", "probes (2)", "probes (3)", "probes (previews)", "probes (gateway)",
})
LANES = (("self-hosted", "Linux", "ARM64", "native-linux"), ("self-hosted", "Linux", "X64", "native-linux"))


def api(path, paginate=False):
    command = ["gh", "api"] + (["--paginate", "--slurp"] if paginate else []) + [path]
    return json.loads(subprocess.check_output(command, text=True))


def validate(manifest):
    if manifest.get("version") != 1:
        raise ValueError("unsupported candidate manifest")
    if not re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", manifest.get("repository", "")):
        raise ValueError("invalid repository")
    if not re.fullmatch("[0-9a-f]{40}", manifest.get("sha", "")):
        raise ValueError("candidate must be a full commit SHA")
    if not isinstance(manifest.get("owner"), str) or not manifest["owner"].strip():
        raise ValueError("candidate needs one accountable owner")
    if not isinstance(manifest.get("scope"), list) or not manifest["scope"] or not all(isinstance(s, str) and s.strip() for s in manifest["scope"]):
        raise ValueError("candidate needs explicit scope")


def matching_run(runs, repository, sha, workflow):
    events = {"ci-fast.yaml": ("workflow_dispatch", "merge_group"), "release-acceptance.yaml": ("push", "schedule", "workflow_dispatch"), "release-images.yaml": ("workflow_dispatch", "workflow_run")}
    eligible = [r for r in runs if r.get("event") in events[workflow] and (workflow == "release-images.yaml" or r.get("head_sha") == sha) and (r.get("head_branch") == "main" if r.get("event") != "merge_group" else
                    workflow == "ci-fast.yaml" and r.get("head_branch", "").startswith("gh-readonly-queue/main/"))
                and r.get("path") == ".github/workflows/" + workflow
                and r.get("head_repository", {}).get("full_name") == repository]
    return max(eligible, key=lambda r: (r.get("id", 0), r.get("run_attempt", 1)), default=None)


def full_queue_proof(repository, sha, run, fetch=api):
    attempt = run.get("run_attempt", 1)
    pages = fetch(f"repos/{repository}/actions/runs/{run['id']}/attempts/{attempt}/jobs?per_page=100", True)
    jobs = [job for page in pages for job in page["jobs"]]
    covered = []
    for name in sorted(FULL_FAST_JOBS):
        matches = [job for job in jobs if job.get("name") == name]
        if len(matches) != 1:
            continue
        job = matches[0]
        if (job.get("status") == "completed" and job.get("conclusion") == "success"
                and job.get("head_sha") == sha and job.get("run_id") == run["id"]
                and job.get("run_attempt") == attempt):
            covered.append(name)
    return dict(contract="full-fast-jobs-v1", run=run["id"], attempt=attempt,
                covered=covered, missing=sorted(FULL_FAST_JOBS - set(covered)),
                unexpected=[job.get("name") for job in jobs if job.get("name") not in FULL_FAST_JOBS])


def publication_proof(repository, sha, run, fetch=api, download=None):
    pages = fetch(f"repos/{repository}/actions/runs/{run['id']}/artifacts?per_page=100", True)
    matches = [a for page in pages for a in page.get("artifacts", [])
               if a.get("name") == "release-publication-" + str(run.get("run_attempt", 1)) and not a.get("expired")]
    if len(matches) != 1 or matches[0].get("size_in_bytes", 100001) > 100000:
        return None
    artifact = matches[0]
    path = f"repos/{repository}/actions/artifacts/{int(artifact['id'])}/zip"
    data = download(path) if download else subprocess.check_output(["gh", "api", path])
    with zipfile.ZipFile(io.BytesIO(data)) as archive:
        if len(archive.infolist()) != 1 or archive.infolist()[0].file_size > 100000:
            return None
        record = json.loads(archive.read("release-publication.json"))
    return record if valid_publication(record, repository, sha, run) else None


def valid_publication(record, repository, sha, run):
    if not isinstance(record, dict) or not isinstance(record.get("image_set"), dict):
        return False
    images = record["image_set"].get("images", {})
    return (record.get("version") == 1 and record.get("repository") == repository
            and record.get("sha") == sha and record.get("image_set", {}).get("sha") == sha
            and record.get("run") == str(run["id"])
            and record.get("attempt") == str(run.get("run_attempt", 1))
            and isinstance(images, dict) and {"free", "browser"} <= set(images)
            and all(isinstance(v, str) and re.fullmatch(r"ghcr\.io/[a-z0-9_./-]+@sha256:[0-9a-f]{64}", v) for v in images.values()))


def inspect(manifest, fetch=api):
    validate(manifest)
    repo, sha = manifest["repository"], manifest["sha"]
    prefix = "repos/" + repo
    blockers, evidence = [], {}
    def get(name, path, pages=False):
        try:
            result = fetch(path, pages)
            evidence[name] = result
            return result
        except (subprocess.CalledProcessError, OSError, ValueError) as error:
            blockers.append(f"{name}: metadata unavailable ({type(error).__name__})")
            return None
    metadata = get("repository", prefix)
    if metadata and not metadata.get("private"):
        blockers.append("This preflight describes private Deck deployment; public release policy is separate")
    comparison = get("mainline", prefix + "/compare/" + sha + "...main")
    if comparison and comparison.get("status") not in ("ahead", "identical"):
        blockers.append("candidate is no longer on mainline")
    main = get("main", prefix + "/commits/main")
    moved = main is not None and main.get("sha") != sha
    for workflow in WORKFLOWS:
        info = get(workflow, prefix + "/actions/workflows/" + workflow)
        pages = get(workflow + " runs", prefix + "/actions/workflows/" + workflow + "/runs?per_page=100" + ("" if workflow == "ci-fast.yaml" else "&branch=main") + ("" if workflow == "release-images.yaml" else "&head_sha=" + sha), True)
        runs = [r for page in (pages or []) for r in page.get("workflow_runs", [])]
        run = matching_run(runs, repo, sha, workflow)
        if workflow == "ci-fast.yaml" and run and run.get("event") == "merge_group":
            try:
                proof = full_queue_proof(repo, sha, run, fetch)
                evidence["ci-fast.yaml coverage"] = proof
                if proof["missing"] or proof["unexpected"]:
                    blockers.append("ci-fast.yaml: merge queue did not pass every full-suite job")
                    run = None
            except (subprocess.CalledProcessError, OSError, ValueError, KeyError, TypeError):
                blockers.append("ci-fast.yaml: full merge-queue coverage unavailable")
                run = None
        if workflow == "release-images.yaml":
            run, proof = None, None
            for candidate in sorted(runs, key=lambda r: (r.get("id", 0), r.get("run_attempt", 1)), reverse=True):
                if (not matching_run([candidate], repo, sha, workflow)
                        or candidate.get("status") != "completed" or candidate.get("conclusion") != "success"):
                    continue
                try:
                    proof = publication_proof(repo, sha, candidate, fetch)
                except (subprocess.CalledProcessError, OSError, ValueError, KeyError, TypeError, zipfile.BadZipFile):
                    proof = None
                if proof:
                    run = candidate
                    evidence["publication"] = proof
                    break
            if not proof:
                blockers.append("release-images.yaml: actual candidate publication unverified; successful run metadata is insufficient")
        evidence[workflow + " qualification"] = {k: run.get(k) for k in ("id", "run_attempt", "html_url", "status", "conclusion")} if run else None
        if not run or run.get("status") != "completed" or run.get("conclusion") != "success":
            blockers.append(workflow + ": no current successful exact-candidate qualification")
            if info and info.get("state") != "active":
                blockers.append(workflow + ": disabled; enable only for the bounded qualification window")
            if moved:
                blockers.append(workflow + ": main advanced; do not dispatch main as though it were the frozen candidate")
    runners = get("runners", prefix + "/actions/runners?per_page=100", True)
    inventory = [r for page in (runners or []) for r in page.get("runners", [])]
    org = repo.split("/")[0]
    groups = get("runner groups", "orgs/" + org + "/actions/runner-groups?per_page=100", True)
    for page in groups or []:
        for group in page.get("runner_groups", []):
            if group.get("restricted_to_workflows"):
                continue  # A workflow-restricted group is not a general release lane.
            base = "orgs/" + org + "/actions/runner-groups/" + str(group["id"])
            visibility = group.get("visibility")
            eligible = visibility == "all" or (visibility == "private" and metadata and metadata.get("private"))
            if visibility == "selected":
                selected = get("group repositories " + str(group["id"]), base + "/repositories?per_page=100", True)
                eligible = any(r.get("full_name") == repo for p in (selected or []) for r in p.get("repositories", []))
            if eligible:
                members = get("group runners " + str(group["id"]), base + "/runners?per_page=100", True)
                inventory.extend(r for p in (members or []) for r in p.get("runners", []))
    for lane in LANES:
        matched = [r for r in inventory if r.get("status") == "online" and set(lane) <= {l["name"] for l in r.get("labels", [])}]
        evidence["lane " + ",".join(lane)] = [{k: r.get(k) for k in ("name", "busy")} for r in matched]
        if not matched:
            blockers.append("no online native runner for " + ",".join(lane))
    declaration = get("reviewer declaration", prefix + "/contents/.noise/review.env?ref=" + sha)
    if declaration is None:
        blockers.append("reviewer declaration unavailable; repair before starting reviews")
    # The report contains operational metadata, never tokens or environment values.
    evidence.pop("reviewer declaration", None)
    evidence.pop("runners", None)
    for key in list(evidence):
        if key == "runner groups" or key.startswith("group "):
            evidence.pop(key)
    evidence.pop("repository", None)
    for workflow in WORKFLOWS:
        evidence.pop(workflow + " runs", None)
    return dict(version=1, candidate=manifest, main_advanced=moved,
                ready=not blockers, blockers=blockers, evidence=evidence,
                limits=["Not publication authority or a security waiver", "Runner eligibility does not prove available capacity", "CLI/policy compatibility and runtime image identity remain acceptance checks"])


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="action", required=True)
    freeze = commands.add_parser("freeze")
    freeze.add_argument("--repo", required=True)
    freeze.add_argument("--sha", required=True)
    freeze.add_argument("--owner", required=True)
    freeze.add_argument("--scope", action="append", required=True)
    freeze.add_argument("--manifest", required=True)
    check = commands.add_parser("check")
    check.add_argument("--manifest", required=True)
    args = parser.parse_args()
    if args.action == "freeze":
        manifest = dict(version=1, repository=args.repo, sha=args.sha, owner=args.owner,
                        scope=args.scope, created=dt.datetime.now(dt.timezone.utc).isoformat())
        validate(manifest)
        # Exclusive creation prevents an accidental retry from silently moving a candidate.
        with open(args.manifest, "x") as output:
            output.write(json.dumps(manifest, indent=2) + "\n")
        print(args.manifest)
        return 0
    report = inspect(json.loads(Path(args.manifest).read_text()))
    print(json.dumps(report, indent=2))
    return 0 if report["ready"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
