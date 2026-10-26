#!/usr/bin/env bash
# Open a like-for-like reviewer model refresh PR when new models arrive.
#
# The reviewer models pinned in .noise/reviewer-models.json rot as providers
# ship successors. This job runs in the deployed workspace (the image
# bootstrap schedules it), where the pinned CLIs and their credentials
# live: it asks codex for its bundled model catalog, probes claude's local
# catalog with bounded candidates, reads kimi's configured model catalog, and
# when a pinned model is no longer the newest in its own vendor-and-tier family
# it opens ONE pull request that
# moves the pin store and every literal that repeats it. It never merges
# anything, it never writes to the local checkout (the offer branch and
# files are created through the GitHub API), and an ambiguous probe offers
# nothing — see internal/modelbump for the decision rules.
#
# usage: model-offer.sh --repo DIR [--slug OWNER/NAME] [--dry-run]
#
# --slug   repository to publish to (default: the checkout's origin, else
#          GITHUB_REPOSITORY).
# --dry-run prints the plan and the PR it would open, changing nothing.
# NOISEDECK_MODEL_OFFER_PLAN replaces discovery with a prepared plan JSON
# (the shape `model-plan -files` prints); it exists for the publication
# tests and manual replay, not for inventing offers.

set -euo pipefail

repo=""
slug=""
dry=""
while [ $# -gt 0 ]; do
  case "$1" in
    --repo) repo="${2:?}"; shift 2 ;;
    --slug) slug="${2:?}"; shift 2 ;;
    --dry-run) dry=1; shift ;;
    *) echo "model-offer: unknown argument $1" >&2; exit 2 ;;
  esac
done
[ -n "${repo}" ] || { echo "model-offer: --repo DIR is required" >&2; exit 2; }
repo="${repo%/}"
if [ -z "${slug}" ]; then
  # Strip a .git suffix FIRST (POSIX sed has no lazy quantifier), then
  # take the owner/repo tail after the last colon or slash.
  origin="$(git -C "${repo}" remote get-url origin 2>/dev/null || true)"
  origin="${origin%.git}"
  origin="${origin%/}"
  slug="$(printf '%s\n' "${origin}" | sed -nE 's#^.*[:/]([^/:]+/[^/]+)$#\1#p')"
  slug="${slug:-${GITHUB_REPOSITORY:-}}"
fi
[ -n "${slug}" ] || { echo "model-offer: no repository slug (try --slug OWNER/NAME)" >&2; exit 2; }

command -v codex >/dev/null || { echo "model-offer: codex CLI not found on PATH" >&2; exit 1; }
command -v claude >/dev/null || { echo "model-offer: claude CLI not found on PATH" >&2; exit 1; }
command -v kimi >/dev/null || { echo "model-offer: kimi CLI not found on PATH" >&2; exit 1; }

plan="${NOISEDECK_MODEL_OFFER_PLAN:-}"
owned_plan=""
if [ -z "${plan}" ]; then
  plan="$(mktemp)"
  owned_plan=1
  (cd "${repo}" && go run ./cmd/noisedeck-harness-bump model-plan -root . -files) >"${plan}"
fi
body="$(mktemp)"
# Both owned temporaries go on exit: a prepared-plan run owns only the
# body file, and successful, failed, and dry runs all clean up.
cleanup() {
  [ -n "${owned_plan}" ] && rm -f "${plan}"
  rm -f "${body}"
}
trap cleanup EXIT

count="$(jq '.offers | length' "${plan}")"
jq -r '.statuses[] | "\(.role): \(.pinned)\(if .latest then " -> \(.latest)" else "" end)\(if .note then " (\(.note))" else "" end)"' "${plan}"
if [ "${count}" -eq 0 ]; then
  echo "model-offer: every reviewer pin is the newest in its family; no offer."
  exit 0
fi

branch="model-bump/reviewers-$(jq -r .id "${plan}")"

# One live offer at a time: an open model PR means nobody has
# answered the previous one, and stacking a second would race the first.
if [ -z "${dry}" ]; then
  open="$(gh pr list --repo "${slug}" --base main --state open --limit 100 \
    --json headRefName --jq '[.[] | select(.headRefName | startswith("model-bump/reviewers-"))] | length')"
  if [ "${open}" -gt 0 ]; then
    echo "model-offer: a reviewer model offer is already open; skipping." >&2
    exit 0
  fi
  existing="$(gh pr list --repo "${slug}" --head "${branch}" --state all --json number --jq '.[0].number // empty')"
  if [ -n "${existing}" ]; then
    echo "model-offer: offer #${existing} already carries this exact pin set." >&2
    exit 0
  fi
fi

{
  echo 'Like-for-like reviewer model refresh: same vendor and tier family, newer version.'
  echo
  jq -r '.statuses[] | select(.latest != "") | "- \(.role): \(.pinned) -> \(.latest)"' "${plan}"
  jq -r '.statuses[] | select(.note != "" and .note != null) | "- \(.role): \(.note)"' "${plan}"
  echo
  echo 'Merging this PR accepts the refresh. It is never merged automatically.'
  echo 'When the NOISE_REVIEW_MODEL repository variable is set it overrides the'
  echo 'settle fallback literal this PR also updates — update the variable too.'
} >"${body}"
echo
echo "model-offer: would open ${branch}"
echo "title: chore(ci): offer reviewer model refresh $(jq -r .id "${plan}")"
sed 's/^/  | /' "${body}"
[ -n "${dry}" ] && exit 0

# Publication is convergent and never trusts a moving main: resolve the
# base commit ONCE, validate the plan's source bytes against that
# immutable commit by hash (command substitution strips trailing
# newlines, so byte equality must be hash equality), cut the branch from
# the same commit, and then PUT only the files the branch has not yet
# received. A run interrupted at ANY point — before the branch, between
# file writes, after all writes — resumes to the same end state without
# overwriting a foreign edit, because a branch blob that is neither the
# base bytes nor the planned bytes aborts the whole publication.
base_sha="$(gh api "repos/${slug}/git/ref/heads/main" --jq .object.sha)"

planned_hash() { jq -r --arg p "$1" '.files[$p]' "${plan}" | base64 -d | sha256sum | cut -d' ' -f1; }
source_hash()  { jq -r --arg p "$1" '.sources[$p] // empty' "${plan}"; }
blob_hash()    { gh api "repos/${slug}/contents/$1?ref=$2" | jq -r .content | base64 -d | sha256sum | cut -d' ' -f1; }

# The plan carries the hash of every source byte it was built from. The
# resolved base commit's blobs must match THOSE identities — comparing a
# live checkout instead would let a replayed old plan ride a checkout and
# main that advanced together, publishing bodies that delete the newer
# content.
for path in $(jq -r '.files | keys[]' "${plan}"); do
  source="$(source_hash "${path}")"
  if [ -z "${source}" ]; then
    echo "model-offer: plan carries no source hash for ${path}; refusing." >&2
    exit 1
  fi
  if [ "$(blob_hash "${path}" "${base_sha}")" != "${source}" ]; then
    echo "model-offer: ${path} at main ${base_sha} does not match the plan's source bytes; aborting before any write." >&2
    exit 1
  fi
done

if ! gh api -X POST "repos/${slug}/git/refs" -f "ref=refs/heads/${branch}" -f "sha=${base_sha}" >/dev/null 2>&1; then
  # A branch left by an interrupted run is the resume path; a branch the
  # lookup cannot find at all is a real failure. (No `grep -q` here:
  # under pipefail it SIGPIPEs the producer.)
  tip="$(gh api "repos/${slug}/git/ref/heads/${branch}" --jq .object.sha 2>/dev/null || true)"
  if [ -z "${tip}" ]; then
    echo "model-offer: creating branch ${branch} failed and it does not exist." >&2
    exit 1
  fi
fi

for path in $(jq -r '.files | keys[]' "${plan}"); do
  # One response: the branch's current bytes and the blob SHA that
  # authorizes replacing them. A branch edit after this response makes
  # the PUT conflict instead of overwriting the edit.
  response="$(gh api "repos/${slug}/contents/${path}?ref=${branch}")"
  current="$(printf '%s' "${response}" | jq -r .content | base64 -d | sha256sum | cut -d' ' -f1)"
  blob_sha="$(printf '%s' "${response}" | jq -r .sha)"
  if [ "${current}" = "$(planned_hash "${path}")" ]; then
    continue # a previous interrupted run already wrote this file
  fi
  if [ "${current}" != "$(source_hash "${path}")" ]; then
    echo "model-offer: ${path} on ${branch} is neither the plan's source bytes nor the planned bytes; refusing to overwrite." >&2
    exit 1
  fi
  content="$(jq -r --arg p "${path}" '.files[$p]' "${plan}")"
  gh api -X PUT "repos/${slug}/contents/${path}" \
    -f "message=chore(ci): move reviewer model pin in ${path}" \
    -f "content=${content}" -f "branch=${branch}" -f "sha=${blob_sha}" >/dev/null
done

gh pr create --repo "${slug}" --base main --head "${branch}" \
  --title "chore(ci): offer reviewer model refresh $(jq -r .id "${plan}")" \
  --body-file "${body}"
