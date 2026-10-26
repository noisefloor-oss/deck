"""Summarize authoritative job/step intervals without summing nested tests."""
import argparse
import datetime as dt
import json
import os
from pathlib import Path
import re
import subprocess


def timestamp(value):
    return dt.datetime.fromisoformat(value.replace("Z", "+00:00")) if value else None


def seconds(start, end):
    start, end = timestamp(start), timestamp(end)
    return max(0, (end - start).total_seconds()) if start and end else None


def phase(name):
    if name.startswith(("Post ", "Complete job", "Save ", "Upload ", "Clean ")):
        return "cleanup_artifacts"
    if name.startswith(("Set up ", "Initialize ", "Prepare ", "Check out ", "Checkout", "Install ", "Restore ", "Download ", "Extract ")):
        return "setup_cache"
    if name.startswith(("Test ", "Run ", "Check ", "Verify ", "Build ", "Scan ", "Certify ")):
        return "execution"
    return "unclassified"


def summarize(job):
    result = {k: job.get(k) for k in ("id", "name", "status", "conclusion", "runner_name", "head_sha")}
    result["pending_seconds"] = seconds(job.get("created_at"), job.get("started_at"))
    result["wall_seconds"] = seconds(job.get("started_at"), job.get("completed_at"))
    result["total_seconds"] = seconds(job.get("created_at"), job.get("completed_at"))
    totals = dict.fromkeys(("setup_cache", "execution", "cleanup_artifacts", "unclassified"), 0.0)
    intervals = []
    for step in job.get("steps", []):
        if step.get("status") != "completed" or step.get("conclusion") == "skipped":
            continue
        start, end = step.get("started_at"), step.get("completed_at")
        duration = seconds(start, end)
        if duration is not None:
            totals[phase(step.get("name", ""))] += duration
            intervals.append((timestamp(start), timestamp(end)))
    intervals.sort()
    overlap = any(a[1] > b[0] for a, b in zip(intervals, intervals[1:]))
    result["step_seconds"] = totals
    result["overlapping_steps"] = overlap
    result["unattributed_seconds"] = None if overlap or result["wall_seconds"] is None else max(0, result["wall_seconds"] - sum(totals.values()))
    return result


def markdown(records):
    lines = ["## CI job timing", "", "Pending is job creation to start; dependency wait before job creation is excluded. It is not a pure runner-capacity measurement.",
             "Step phases are classified by step name. Unclassified steps and unattributed time remain visible. Parallel job durations must not be added as release elapsed time.", "",
             "| Job | Runner | Result | Pending | Setup/cache | Execution | Cleanup/artifacts | Other steps | Unattributed | Job wall | Total |",
             "|---|---|---|---:|---:|---:|---:|---:|---:|---:|---:|"]
    def label(value):
        return str(value or "—").replace("|", "\\|").replace("\n", " ").replace("<", "&lt;").replace(">", "&gt;")
    def duration(value):
        return "—" if value is None else f"{value:.1f}s"
    for row in records:
        phases = row["step_seconds"]
        cells = [label(row[k]) for k in ("name", "runner_name", "conclusion")]
        cells += [duration(row["pending_seconds"])]
        cells += [duration(phases[k]) for k in ("setup_cache", "execution", "cleanup_artifacts", "unclassified")]
        cells += [duration(row[k]) for k in ("unattributed_seconds", "wall_seconds", "total_seconds")]
        lines.append("| " + " | ".join(cells) + " |")
    return "\n".join(lines) + "\n"


def fetch_jobs(repo, run, attempt):
    output = subprocess.check_output(["gh", "api", "--paginate",
        f"repos/{repo}/actions/runs/{run}/attempts/{attempt}/jobs?per_page=100",
        "--jq", ".jobs[] | @json"], text=True)
    return [json.loads(line) for line in output.split('\n') if line.strip()]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--repo", required=True)
    parser.add_argument("--run", required=True)
    parser.add_argument("--attempt", required=True)
    parser.add_argument("--output", required=True)
    args = parser.parse_args()
    if not re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", args.repo) or not all(re.fullmatch(r"[1-9][0-9]*", v) for v in (args.run, args.attempt)):
        parser.error("invalid repository, run or attempt")
    records = [summarize(job) for job in fetch_jobs(args.repo, args.run, args.attempt)]
    Path(args.output).write_text(json.dumps(dict(version=1, repository=args.repo, run=args.run, attempt=args.attempt, jobs=records), indent=2) + "\n")
    report = markdown(records)
    print(report)
    if os.getenv("GITHUB_STEP_SUMMARY"):
        with open(os.environ["GITHUB_STEP_SUMMARY"], "a") as stream:
            stream.write(report)


if __name__ == "__main__":
    main()
