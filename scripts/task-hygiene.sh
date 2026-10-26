#!/usr/bin/env bash
# Sweep task lifecycle residue for one repository family.
#
# The Tasks engine's `hygiene` report is read-only by design: it names stale
# runs, expired claims and claimless in-progress tasks and leaves the policy
# to an orchestrator. This script is that policy for a Deck checkout. It is
# a dry run unless --apply is given, and it only ever acts on residue whose
# content is provably elsewhere:
#
#   - stale runs are force-cancelled when their attempt lease expires (the
#     engine removes the run's clean bound worktree itself);
#   - task worktrees are removed when the task is done, the tree is clean
#     and the branch's pull request merged;
#   - local branches are deleted when their tip is exactly the head of a
#     merged pull request (squash merges hide this from `git branch
#     --merged`);
#   - git and engine worktree records are pruned, remote-tracking branches
#     are pruned and the repository is garbage-collected.
#
# Everything else is reported and left alone: dirty worktrees, worktrees
# and branches with no pull request, open pull requests, expired claims and
# in-progress tasks without a claim. Those need a person.
#
# usage: task-hygiene.sh [--apply] [--repo DIR | --all ROOT]
#
# --all ROOT sweeps every initialized task family the engine's doctor finds
# under ROOT, one after another, and exits non-zero if any sweep failed.
# TASK_HYGIENE_NO_GH=1 disables pull-request checks even when gh is available.
set -euo pipefail

apply=0
repo=""
root=""
while [ $# -gt 0 ]; do
  case "$1" in
    --apply) apply=1 ;;
    --repo) repo="${2:?--repo needs a directory}"; shift ;;
    --all) root="${2:?--all needs a root directory}"; shift ;;
    -h|--help) sed -n '2,28p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "task-hygiene: unknown argument: $1" >&2; exit 2 ;;
  esac
  shift
done

tasks_cli=""
for candidate in "noise tasks" noisetask noise-tasks; do
  if command -v "${candidate%% *}" >/dev/null 2>&1; then tasks_cli="$candidate"; break; fi
done
if [ -z "$tasks_cli" ]; then
  echo "task-hygiene: no Tasks engine CLI on PATH (noise, noisetask or noise-tasks)" >&2
  exit 2
fi
# shellcheck disable=SC2086
tasks() { $tasks_cli "$@"; }

if [ -n "$root" ]; then
  [ -z "$repo" ] || { echo "task-hygiene: --all and --repo are exclusive" >&2; exit 2; }
  failed=0
  if ! doctor=$(tasks doctor -json -root "$root"); then
    echo "task-hygiene: doctor failed for $root" >&2
    exit 2
  fi
  if ! families=$(printf '%s' "$doctor" | python3 -c 'import json,sys; print("\n".join(f["boundary"] for f in json.load(sys.stdin).get("families",[]) if f.get("initialized")))'); then
    echo "task-hygiene: could not parse doctor families" >&2
    exit 2
  fi
  while IFS= read -r family; do
    [ -n "$family" ] || continue
    args=(--repo "$family")
    [ "$apply" = 1 ] && args+=(--apply)
    "$0" "${args[@]}" || failed=1
  done <<< "$families"
  exit $failed
fi

if [ -z "$repo" ]; then
  repo=$(git rev-parse --show-toplevel 2>/dev/null) || { echo "task-hygiene: not in a git repository; pass --repo" >&2; exit 2; }
fi
cd "$repo"

have_gh=0
if [ "${TASK_HYGIENE_NO_GH:-0}" != 1 ] && command -v gh >/dev/null 2>&1 && gh auth status >/dev/null 2>&1; then have_gh=1; fi

mode="would"
[ "$apply" = 1 ] && mode="did"
say() { printf 'hygiene: %s\n' "$*"; }
failures=0
report_error=$(mktemp)
trap 'rm -f "$report_error"' EXIT
report_failed() {
  say "FAILED read the engine $1 report"
  cat "$report_error" >&2
  failures=$((failures + 1))
}
act() { # act <description> <command...>: run under --apply, print otherwise
  local what="$1" error; shift
  if [ "$apply" = 1 ]; then
    if error=$("$@" 2>&1 >/dev/null); then
      say "$mode $what"
    else
      say "FAILED $what"
      [ -z "$error" ] || printf '%s\n' "$error" >&2
      failures=$((failures + 1))
    fi
  else
    say "$mode $what"
  fi
}

# A merged pull request's head commit for a branch, or empty.
merged_head() {
  [ "$have_gh" = 1 ] || return 0
  gh pr list --head "$1" --state merged --limit 1 --json headRefOid -q '.[0].headRefOid // empty' 2>/dev/null || true
}
# The open pull request number for a branch, or empty.
open_pr() {
  [ "$have_gh" = 1 ] || return 0
  gh pr list --head "$1" --state open --limit 1 --json number -q '.[0].number // empty' 2>/dev/null || true
}
remote_pr_state() {
  [ "$have_gh" = 1 ] || return 0
  gh pr list --head "$1" --state all --limit 1 --json state -q '.[0].state // empty' 2>/dev/null
}
task_status() {
  tasks show -json -tag "$1" "$2" 2>/dev/null | python3 -c 'import json,sys; print(json.load(sys.stdin).get("status",""))' 2>/dev/null || true
}

say "repository $repo ($( [ "$apply" = 1 ] && echo apply || echo 'dry run' ); gh $( [ "$have_gh" = 1 ] && echo available || echo 'unavailable: pull-request checks skipped' ))"

# 1. Records that point nowhere.
act "prune git worktree records" git worktree prune
recreated=()
if records=$(tasks worktree list -json 2>"$report_error") &&
  sagas=$(printf '%s' "$records" | python3 -c '
import json,sys
for e in json.load(sys.stdin).get("entries",[]):
    if e["status"] in ("cleanup-required", "pending", "needs-broker"):
        print("%s|%s|%s|%s|%s" % (e["sagaId"], e["status"], e.get("branch", ""), e.get("baseCommit", ""), e.get("operation", "")))' 2>>"$report_error"); then
  while IFS='|' read -r saga status branch base operation; do
    [ -n "$saga" ] || continue
    if [ "$status" != cleanup-required ]; then
      say "NEEDS A PERSON: saga $saga is $status"
      continue
    fi
    if [ "$operation" != remove ]; then
      say "NEEDS A PERSON: saga $saga has operation $operation"
      continue
    fi
    git show-ref --verify --quiet "refs/heads/$branch" && continue
    if [ -z "$branch" ] || [ -z "$base" ] || [ "$(git cat-file -t "$base" 2>/dev/null || true)" != commit ]; then
      say "NEEDS A PERSON: saga $saga cannot recreate branch $branch at base commit $base"
      continue
    fi
    before=$failures
    act "recreate branch $branch at $base so saga $saga can prove removal" git branch "$branch" "$base"
    if [ "$apply" = 1 ] && [ "$failures" = "$before" ]; then
      recreated+=("$saga|$branch|$base")
    fi
  done <<< "$sagas"
else
  report_failed worktree
fi
act "prune engine worktree records" tasks worktree prune
if [ "$apply" = 1 ] && [ "${#recreated[@]}" -gt 0 ]; then
  if records=$(tasks worktree list -json 2>"$report_error") &&
    statuses=$(printf '%s' "$records" | python3 -c 'import json,sys; print(json.dumps({str(e["sagaId"]): e["status"] for e in json.load(sys.stdin).get("entries",[])}))' 2>>"$report_error"); then
    for entry in "${recreated[@]}"; do
      IFS='|' read -r saga branch base <<< "$entry"
      status=$(printf '%s' "$statuses" | python3 -c 'import json,sys; print(json.load(sys.stdin).get(sys.argv[1], "unknown"))' "$saga")
      if [ "$status" = removed ]; then
        before=$failures
        act "delete recreated branch $branch (saga $saga removed)" git update-ref -d "refs/heads/$branch" "$base"
        if [ "$failures" != "$before" ]; then
          say "NEEDS A PERSON: branch $branch moved; saga $saga removed"
        fi
      else
        say "NEEDS A PERSON: saga $saga is $status; keep recreated branch $branch"
      fi
    done
  else
    report_failed worktree
  fi
fi

# 2. Runs whose lease expired. The engine also removes a cancelled run's
#    clean bound worktree, so this is the one action that frees disk by itself.
if report=$(tasks hygiene -json 2>"$report_error") &&
  runs=$(printf '%s' "$report" | python3 -c 'import json,sys; d=json.load(sys.stdin); print(" ".join("%s:%s" % (r["runId"], r["task"]) for r in d.get("staleRuns",[])))' 2>>"$report_error") &&
  notices=$(printf '%s' "$report" | python3 -c '
import json,sys
d=json.load(sys.stdin)
for c in d.get("expiredClaims",[]): print("hygiene: NEEDS A PERSON: task %s still carries an expired claim of %s (expired %s)" % (c["task"], c["owner"], c["expiredAt"]))
for c in d.get("claimlessInProgress",[]): print("hygiene: NEEDS A PERSON: task %s is in-progress with no live claim (updated %s); finish it or move it to pending" % (c["task"], c["updatedAt"]))' 2>>"$report_error"); then
  stamp=$(date -u +%Y%m%dT%H%M%SZ)
  for run in $runs; do
    act "cancel stale run ${run%%:*} of task ${run##*:}" tasks run cancel -force -request-id "hygiene-$stamp-${run%%:*}" "${run%%:*}"
  done
  [ -z "$notices" ] || printf '%s\n' "$notices"
else
  report_failed hygiene
fi

# 3. Worktrees. Only engine task worktrees with a done task, a clean tree and
#    a merged pull request go; everything else is named.
main=$(git rev-parse --abbrev-ref HEAD)
while IFS= read -r line; do
  path=${line#worktree }
  [ "$path" = "$repo" ] && continue
  branch=$(git -C "$path" branch --show-current 2>/dev/null || true)
  name=${path#"$repo"/.worktrees/}
  dirty=$(git -C "$path" status --porcelain 2>/dev/null | wc -l | tr -d ' ')
  case "$name" in
    master-*|main-*)
      id=${name#*-}
      tag=${name%%-*}
      status=$(task_status "$tag" "$id")
      if [ "$dirty" != 0 ]; then
        say "NEEDS A PERSON: worktree $name (task $id, $status) has $dirty uncommitted change(s)"
      elif [ "$status" != done ]; then
        say "keep worktree $name: task $id is $status"
      elif [ "$have_gh" != 1 ]; then
        say "keep worktree $name: task $id is done but gh is unavailable to confirm $branch merged"
      elif [ -z "$(merged_head "$branch")" ]; then
        say "NEEDS A PERSON: worktree $name: task $id is done but no merged pull request was found for $branch"
      else
        act "remove worktree $name (task $id done, branch merged)" git worktree remove "$path"
      fi
      ;;
    review-*)
      say "keep review worktree $name: review scratch is removed by its reviewer"
      ;;
    *)
      say "NEEDS A PERSON: hand-made worktree $name on ${branch:-a detached head} ($dirty uncommitted change(s)); the SDLC provisions task worktrees through the engine"
      ;;
  esac
done < <(git worktree list --porcelain | grep '^worktree ')

# 4. Local branches whose tip is exactly a merged pull request's head.
for branch in $(git for-each-ref --format='%(refname:short)' refs/heads/); do
  [ "$branch" = "$main" ] && continue
  git worktree list --porcelain | grep -q "^branch refs/heads/$branch\$" && continue
  tip=$(git rev-parse "$branch")
  if [ "$have_gh" = 1 ]; then
    head=$(merged_head "$branch")
    if [ -n "$head" ] && [ "$head" = "$tip" ]; then
      act "delete local branch $branch (merged pull request head)" git branch -D "$branch"
      continue
    fi
    if [ -n "$(open_pr "$branch")" ]; then
      say "keep branch $branch: pull request open"
      continue
    fi
  fi
  say "NEEDS A PERSON: branch $branch has no merged pull request at its tip ($(git rev-list --count "$main..$branch" 2>/dev/null || echo '?') commit(s) not on $main)"
done

# 5. Residue that is never safe to remove automatically. Merged remote
#    branches are only proposed for deletion; the apply set stays unchanged.
for ref in $(git for-each-ref --format='%(refname)' refs/remotes/origin/); do
  branch=${ref#refs/remotes/origin/}
  case "$branch" in HEAD|main|master) continue ;; esac
  if [ "$have_gh" != 1 ]; then
    say "keep remote branch origin/$branch: gh is unavailable to classify its pull request"
    continue
  fi
  if ! state=$(remote_pr_state "$branch"); then
    say "NEEDS A PERSON: remote branch origin/$branch pull request lookup failed"
    continue
  fi
  case "$state" in
    MERGED) say "NEEDS A PERSON: remote branch origin/$branch is merged; delete it" ;;
    OPEN) say "keep remote branch origin/$branch: pull request open" ;;
    CLOSED) say "NEEDS A PERSON: remote branch origin/$branch has a closed, unmerged pull request" ;;
    *) say "NEEDS A PERSON: remote branch origin/$branch has no pull request" ;;
  esac
done

stashes=$(git stash list | wc -l | tr -d ' ')
[ "$stashes" = 0 ] || say "NEEDS A PERSON: $stashes stash(es) remain"

if milestones=$(tasks milestone list -json 2>"$report_error") &&
  backlog=$(tasks list -json 2>"$report_error") &&
  notices=$(python3 -c '
import datetime,json,sys
milestones=json.load(open(sys.argv[1])).get("milestones",[])
tasks=json.load(open(sys.argv[2])).get("tasks",[])
terminal={"done","cancelled"}
for m in milestones:
    if m.get("status") != "open": continue
    related=[t for t in tasks if t.get("milestone") == m.get("name")]
    if related and all(t.get("status") in terminal for t in related):
        print("hygiene: NEEDS A PERSON: milestone %s is open although every task is terminal" % m["name"])
cutoff=datetime.datetime.now(datetime.timezone.utc)-datetime.timedelta(days=30)
for t in tasks:
    if t.get("status") not in ("deferred","blocked"): continue
    try: updated=datetime.datetime.fromisoformat(t["updatedAt"].replace("Z","+00:00"))
    except (KeyError,ValueError): continue
    if updated <= cutoff:
        print("hygiene: NEEDS A PERSON: task %s has been %s since %s" % (t["id"],t["status"],t["updatedAt"]))' <(printf '%s' "$milestones") <(printf '%s' "$backlog") 2>>"$report_error"); then
  [ -z "$notices" ] || printf '%s\n' "$notices"
else
  report_failed "milestone/task list"
fi

# 6. Remote-tracking refs and garbage.
if git remote get-url origin >/dev/null 2>&1; then
  act "prune remote-tracking branches" git fetch --prune --quiet origin
else
  say "skip remote pruning: no origin remote"
fi
act "garbage-collect the repository" git gc --quiet

if [ "$apply" = 1 ] && [ "$failures" -gt 0 ]; then exit 1; fi
