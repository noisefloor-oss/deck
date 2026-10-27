#!/usr/bin/env bash
# Captures an offline review; the trusted host validates and posts it.
set -euo pipefail
fail() { echo "noise-review capture: $1" >&2; exit 1; }
[ "${1:-}" = review ] || fail 'review subcommand is required'
shift
event=""; content=""; mode=""; model=""; round=""
while [ "$#" -gt 0 ]; do
 [ "$#" -ge 2 ] && [ -n "$2" ] || fail 'option requires a non-empty value'
 case "$1" in
  --event) [ -z "$event" ] || fail 'duplicate event'; event="$2" ;;
  --body|--body-file|--findings)
   [ -z "$mode" ] || fail 'review content must be supplied exactly once'
   mode="$1"; content="$2" ;;
  --repo) [ "$2" = "$JAIL_REPO" ] || fail 'repository differs from trusted snapshot' ;;
  --pr) [ "$2" = "$JAIL_PR" ] || fail 'PR differs from trusted snapshot' ;;
  --expect-head) [ "$2" = "$JAIL_HEAD" ] || fail 'head differs from trusted snapshot' ;;
  --reviewer-model) [[ "$2" != *$'\n'* && "$2" != *$'\r'* ]] || fail 'model must be a single line'; [ -z "$model" ] || fail 'duplicate model'; model="$2" ;;
  --round) [[ "$2" =~ ^[1-9][0-9]*$ ]] || fail 'invalid round'; [ -z "$round" ] || fail 'duplicate round'; round="$2" ;;
  *) fail 'unsupported option' ;;
 esac
 shift 2
done
case "$event" in APPROVE|REQUEST_CHANGES|COMMENT) ;; *) fail 'invalid review event' ;; esac
[ -n "$mode" ] || fail 'review content is required'
if [ "$mode" != --body ]; then
 [ -f "$content" ] && [ ! -L "$content" ] && [ -r "$content" ] || fail 'content must be a readable regular file'
 [ "$(wc -c <"$content")" -le 65536 ] || fail 'review content exceeds limit'
 content="$(cat -- "$content")"
fi
[ -n "$content" ] || fail 'review content is empty'
[ "$(printf %s "$content" | wc -c)" -le 65536 ] || fail 'review content exceeds limit'
if [ "$mode" = --findings ]; then
 [[ "$JAIL_HEAD" =~ ^[0-9a-f]{40}$ ]] && [[ "$JAIL_BASE" =~ ^[0-9a-f]{40}$ ]] || fail 'immutable snapshot is required'
 # Use the pinned tool's composition contract, without any network operation.
 [ -f "${JAIL_REVIEW_LIB:-/opt/review-lib}/review.sh" ] || fail 'pinned review composition library is unavailable'
 source "${JAIL_REVIEW_LIB:-/opt/review-lib}/review.sh"
 contains_secret "$content" && fail 'findings contain a secret shape'
 findings="$(_crb_parse_findings findings <<<"$content")"
 [ -n "$findings" ] || fail 'findings file has no findings'
 content="$(_crb_compose_body "$event" "$JAIL_HEAD" "$JAIL_BASE" "$model" "$round" "$findings" "")"
 _crb_validate_composed_body "$event" "$JAIL_HEAD" "$JAIL_BASE" "$findings" "$content"
else
 [ -z "$model$round" ] || fail 'model and round require findings mode'
fi
[ "$(printf %s "$content" | wc -c)" -le 65536 ] || fail 'composed review exceeds limit'
printf '%s\n%s\n' "$event" "$content" >"${JAIL_CAPTURE_PATH:-/work/.noise-verdict}"
echo '{"captured":true}'
