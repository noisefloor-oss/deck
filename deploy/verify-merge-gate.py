#!/usr/bin/env python3
"""Project merge preflight: exact-head review and code-aware CI evidence."""

from __future__ import annotations

import argparse
import json
import re
import subprocess
import sys


def gh(*args: str) -> object:
    result = subprocess.run(
        ["gh", *args], capture_output=True, text=True, check=False
    )
    if result.returncode != 0:
        message = result.stderr.strip() or result.stdout.strip() or "gh failed"
        raise RuntimeError(message)
    return json.loads(result.stdout)


def graphql_nodes(
    connection: str,
    selection: str,
    number: int,
    owner: str,
    name: str,
) -> list[dict[str, object]]:
    query = (
        "query($owner:String!,$name:String!,$number:Int!,$after:String){"
        "repository(owner:$owner,name:$name){pullRequest(number:$number){"
        f"{connection}(first:100,after:$after){{nodes{{{selection}}}"
        "pageInfo{hasNextPage endCursor}}}}}"
    )
    nodes: list[dict[str, object]] = []
    after: str | None = None
    while True:
        args = [
            "api",
            "graphql",
            "-f",
            f"query={query}",
            "-F",
            f"owner={owner}",
            "-F",
            f"name={name}",
            "-F",
            f"number={number}",
        ]
        if after is not None:
            args.extend(["-f", f"after={after}"])
        page = gh(*args)
        assert isinstance(page, dict)
        connection_data = (
            page.get("data", {})
            .get("repository", {})
            .get("pullRequest", {})
            .get(connection, {})
        )
        batch = connection_data.get("nodes", [])
        assert isinstance(batch, list)
        nodes.extend(batch)
        page_info = connection_data.get("pageInfo", {})
        if not page_info.get("hasNextPage"):
            return nodes
        after = page_info.get("endCursor")
        if not isinstance(after, str) or not after:
            raise RuntimeError(f"{connection} pagination omitted its cursor")


# What a verdict must say to count as covering a head.
#
# GitHub's review.commit.oid is not a record of what a review read: it has
# been observed re-associated to a commit created MINUTES AFTER the review
# was submitted (in each case the review body named one sha while the API
# reported a later one). Which re-associations GitHub performs, and why, is
# not established; that alone
# disqualifies the field from carrying a gate, because the failure it hides
# is an approval reading as current against bytes nobody approved.
#
# The body can carry the head instead. The review tool writes this marker
# into the verdict's envelope from the SHA it sampled at composition time,
# so the marker does not move when GitHub re-attributes a commit.
#
# What that does and does not establish, stated plainly:
#
#  - The marker is a producer convention, not an attestation. A review body
#    is editable through the API, so this gate is worth what the writers of
#    the gate identity's reviews are trusted to be. It stops re-association
#    and stale approvals; it does not stand against an actor who can rewrite
#    that identity's bodies, and no signature this gate could check would
#    change that unless the signing authority were outside that actor's
#    reach.
#  - The marker records the SHA sampled when the verdict was COMPOSED, not
#    necessarily the one the bytes were fetched at: those are equal only if
#    the producer pins the SHA across the fetch, which is producer-side and
#    outside this script.
#  - Anything able to post as the gate identity can compose the whole
#    envelope, this marker included. Parsing cannot show a review read the
#    bytes; it can only show which head the verdict claims, and refuse the
#    claims that are absent, ambiguous, or about something else.
#
# A verdict with no marker states nothing about which bytes it read, and
# this gate fails closed on it — unlike the dispatcher's duplicate-round
# check, where a false refusal costs a needed review and a false allow costs
# only a duplicate one. The remedy is a fresh review, never a marker edited
# onto an old approval.
HEAD_MARKER = "Reviewed head: "
BASE_MARKER = "Base: "

# How far into the body the envelope's metadata block reaches. The default
# composed shape is: verdict line, blank, Reviewed head, Base, and
# optionally Reviewer-Model and Round — the marker on line 3. Six lines
# holds that with room to spare; it is a CONTRACT with the producer, not a
# discovery, and a repository whose review template moves the marker below
# the header cannot be gated by this script. Beyond the header
# the body is the reviewer's prose, where a marker is a quotation or a
# fenced example rather than this verdict's statement.
HEADER_LINES = 6


def marked_head(body: object) -> str | None:
    """The head a verdict's envelope names, or None when it names none.

    The marker must be a line of its own IN THE HEADER, and exactly one such
    line may be there. Position is what keeps prose out of the answer: a
    body further down can quote a previous round's marker or fence an
    example, and a rule that searched the whole body would take whichever
    one suited it. Two markers in the header are an ambiguity, not a
    statement, and count as none. A header that quotes another verdict's
    envelope is still read as this one's — the header is the producer's to
    compose, and this is a contract with it, not a proof against it.
    """
    if not isinstance(body, str):
        return None
    header = [line.rstrip("\r") for line in body.split("\n")[:HEADER_LINES]]
    marked = [
        line[len(HEAD_MARKER):] for line in header if line.startswith(HEAD_MARKER)
    ]
    return marked[0] if len(marked) == 1 else None

def marked_base(body: object) -> str | None:
    """The base a verdict's envelope names, under the same header contract
    as the head marker: one line of its own among the first HEADER_LINES,
    exactly one, or the envelope does not state a base."""
    if not isinstance(body, str):
        return None
    header = [line.rstrip("\r") for line in body.split("\n")[:HEADER_LINES]]
    marked = [
        line[len(BASE_MARKER):] for line in header if line.startswith(BASE_MARKER)
    ]
    return marked[0] if len(marked) == 1 else None


def verdict_failures(
    reviews: list[dict[str, object]],
    head: str,
    reviewer: str,
    base: str = "",
) -> list[str]:
    """The merge gate's review rule, over already-fetched reviews."""
    expected_login = reviewer.removesuffix("[bot]")
    expected_type = "Bot" if reviewer.endswith("[bot]") else None
    verdicts = [
        review
        for review in reviews
        if (review.get("author") or {}).get("login") == expected_login
        and (
            expected_type is None
            or (review.get("author") or {}).get("__typename") == expected_type
        )
        and review.get("state") in {"APPROVED", "CHANGES_REQUESTED", "DISMISSED"}
    ]
    if not verdicts:
        return [f"latest formal {reviewer} verdict is not an approval"]
    # Timestamps can tie, and then "the latest verdict" is not one verdict.
    # Picking either is picking by list order; fail closed on the pair
    # instead, so an approval cannot be chosen over a change request it was
    # submitted alongside.
    newest = max(str(review.get("submittedAt", "")) for review in verdicts)
    latest_group = [
        review for review in verdicts if str(review.get("submittedAt", "")) == newest
    ]
    # EVERY verdict in the group has to hold, not a representative of it:
    # picking one is picking by list order, and a stale or markerless
    # approval beside a good one would be chosen half the time.
    for latest in latest_group:
        if latest.get("state") != "APPROVED":
            return [f"latest formal {reviewer} verdict is not an approval"]
        marked = marked_head(latest.get("body"))
        if marked is None:
            return [
                f"the latest {reviewer} approval carries no single "
                f"'{HEAD_MARKER}<sha>' line in its header, so nothing states "
                f"which bytes it read"
            ]
        if not head or marked != head:
            return [
                f"the latest {reviewer} approval was written against {marked}, "
                f"not the head being merged ({head or 'unknown'})"
            ]
        marked_base_sha = marked_base(latest.get("body"))
        if base and marked_base_sha != base:
            return [
                f"the latest {reviewer} approval names base {marked_base_sha or 'none'}, "
                f"not the merge base the queue will merge onto ({base}): the base moved "
                f"after the review, so its base-impact assessment no longer applies - "
                f"a fresh review at the current base is required"
            ]
    return []


CONDITIONAL_CHECKS = {
    "frontend", "probes", "lint", "go-serialized", "go-parallel",
    "deploy-suite", "experiment-enabled", "jail-probe", "source-export",
}


def action_run(check: dict[str, object], owner: str, name: str) -> str:
    prefix = "https://github.com/" + owner + "/" + name + "/actions/runs/"
    match = re.fullmatch(re.escape(prefix) + r"([0-9]+)/job/[0-9]+", str(check.get("detailsUrl", "")))
    if check.get("workflowName") != "Fast checks" or match is None:
        raise RuntimeError("conditional check is not from this repository's Fast checks")
    return match[1]


def check_failures(checks: list[dict[str, object]], head: str, number: int,
                   owner: str, name: str) -> list[str]:
    gates = [c for c in checks if c.get("name") == "fast-checks"
             and c.get("workflowName") == "Fast checks" and c.get("conclusion") == "SUCCESS"]
    if not gates:
        return ["required fast-checks gate is not successful"]
    runs = {}
    failures = []
    for check in checks:
        state = check.get("conclusion") or check.get("state") or check.get("status")
        if state in {"SUCCESS", "NEUTRAL"}:
            continue
        if state != "SKIPPED":
            failures.append(f"check {check.get('name')}: {state or 'UNKNOWN'}")
            continue
        check_name = str(check.get("name", ""))
        if (check_name not in CONDITIONAL_CHECKS
                and re.fullmatch(r"(?:probes|source-export) \([^()]+\)", check_name) is None):
            failures.append(f"unrecognized skipped check: {check_name}")
            continue
        run_id = action_run(check, owner, name)
        if not any(action_run(gate, owner, name) == run_id for gate in gates):
            failures.append(f"skipped check lacks a successful aggregate from its run: {check_name}")
            continue
        if run_id not in runs:
            runs[run_id] = gh("api", f"repos/{owner}/{name}/actions/runs/{run_id}")
        run = runs[run_id]
        if (not isinstance(run, dict) or run.get("head_sha") != head
                or run.get("path") != ".github/workflows/ci-fast.yaml"
                or run.get("event") != "pull_request" or run.get("status") != "completed"
                or run.get("conclusion") != "success"
                or not any(pr.get("number") == number for pr in run.get("pull_requests", []))):
            failures.append(f"skipped check lacks exact-PR workflow evidence: {check_name}")
    return failures


def commit_identity_failures(commits: object, expected: object) -> list[str]:
    """Every commit on a pull request's head must be authored by the pull
    request author's GitHub account. GitHub's squash merge turns any other
    author - including an identity it cannot map to an account at all,
    which is exactly what an agent's ad-hoc name and email look like - into
    a Co-authored-by trailer on the mainline commit, injected from commit
    metadata at merge time where no review of the pull request surface ever
    sees it. The check reads the account mapping GitHub itself uses, so it
    cannot disagree with the mechanism it guards against."""
    if not isinstance(commits, list) or not commits:
        return ["no commits were returned for the pull request's head"]
    login = expected if isinstance(expected, str) else None
    failures: list[str] = []
    for commit in commits:
        if not isinstance(commit, dict):
            failures.append("a commit entry was not an object")
            continue
        sha = str(commit.get("sha") or "?")[:12]
        who = commit.get("author")
        mapped = who.get("login") if isinstance(who, dict) else None
        if login is None or mapped != login:
            failures.append(
                f"commit {sha} is not authored by the pull request's author account"
                f" ({'unmapped identity' if not mapped else mapped}): GitHub's squash"
                " merge would lift it into a mainline Co-authored-by trailer"
            )
    return failures


def check_pr(number: int, reviewer: str, owner: str, name: str) -> list[str]:
    data = gh(
        "pr",
        "view",
        str(number),
        "--json",
        "state,isDraft,headRefOid,baseRefOid,mergeable,mergeStateStatus,statusCheckRollup,author",
    )
    assert isinstance(data, dict)
    failures: list[str] = []
    head = data.get("headRefOid")

    if data.get("state") != "OPEN":
        failures.append(f"state is {data.get('state')!r}, not OPEN")
    if data.get("isDraft") is not False:
        failures.append("PR is a draft")
    if data.get("mergeable") != "MERGEABLE":
        failures.append(f"mergeable is {data.get('mergeable')!r}")
    if data.get("mergeStateStatus") != "CLEAN":
        failures.append(f"merge state is {data.get('mergeStateStatus')!r}")

    # GitHub's squash merge lifts every head commit author it cannot map to
    # the PR author into a mainline "Co-authored-by:" trailer at merge time,
    # from commit metadata no review of the PR surface ever sees, so foreign
    # authorship fails closed here.
    commits = gh("api", f"repos/{owner}/{name}/pulls/{number}/commits", "--paginate")
    failures.extend(commit_identity_failures(commits, (data.get("author") or {}).get("login")))

    failures.extend(check_failures(data.get("statusCheckRollup") or [], str(head or ""), number, owner, name))

    reviews = graphql_nodes(
        "reviews",
        "state submittedAt body author{login __typename}",
        number,
        owner,
        name,
    )
    # The base the review must name: what a merge of this PR is actually
    # onto. A PR can carry an exact-head approval written against an older
    # base - the queue may have moved its target under it - and that
    # approval's base-impact assessment then covers bytes that are no
    # longer the base. The comparison is made from the PR's immutable base
    # commit, never a ref name a retarget could move mid-check; the whole
    # comparison object is fetched because a --jq projection would hand
    # the JSON parser a bare string.
    merge_base = ""
    if data.get("mergeable") == "MERGEABLE":
        comparison = gh(
            "api",
            f"repos/{owner}/{name}/compare/{data.get('baseRefOid', '')}...{head}",
        )
        candidate = (
            (comparison.get("merge_base_commit") or {}).get("sha")
            if isinstance(comparison, dict)
            else None
        )
        if isinstance(candidate, str) and len(candidate) == 40:
            merge_base = candidate
        else:
            # A mergeable PR whose comparison yields no valid base SHA must
            # block rather than quietly skip the base rule: an unusable
            # comparison is a verification error, not evidence the base
            # matches.
            failures.append(
                "the merge-base comparison returned no valid 40-character "
                "SHA, so the approval's named base cannot be verified"
            )
    failures.extend(verdict_failures(reviews, str(head or ""), reviewer, merge_base))

    threads = graphql_nodes(
        "reviewThreads",
        "isResolved",
        number,
        owner,
        name,
    )
    unresolved = sum(1 for thread in threads if not thread.get("isResolved"))
    if unresolved:
        failures.append(f"{unresolved} unresolved review thread(s)")

    return failures


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("prs", nargs="+", type=int, help="PR prefix, bottom to top")
    parser.add_argument("--reviewer", default="noise-review[bot]")
    parser.add_argument("--identity-only", action="store_true",
                        help="verify only head-commit authorship; the CI early signal")
    args = parser.parse_args()

    repo = gh("repo", "view", "--json", "nameWithOwner")
    assert isinstance(repo, dict)
    owner, name = str(repo["nameWithOwner"]).split("/", 1)

    failed = False
    for number in args.prs:
        try:
            if args.identity_only:
                data = gh("pr", "view", str(number), "--json", "author")
                assert isinstance(data, dict)
                commits = gh("api", f"repos/{owner}/{name}/pulls/{number}/commits", "--paginate")
                failures = commit_identity_failures(commits, (data.get("author") or {}).get("login"))
            else:
                failures = check_pr(number, args.reviewer, owner, name)
        except (RuntimeError, AssertionError, KeyError, TypeError, ValueError) as exc:
            failures = [f"verification error: {exc}"]
        if failures:
            failed = True
            print(f"PR #{number}: BLOCKED")
            for failure in failures:
                print(f"  - {failure}")
        else:
            print(f"PR #{number}: READY")

    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
