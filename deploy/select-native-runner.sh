#!/bin/sh
set -eu

backend=${NOISEDECK_NATIVE_BACKEND:-self-hosted}

case "$backend" in
  auto|self-hosted|github|blacksmith) ;;
  *)
    echo "NOISEDECK_NATIVE_BACKEND must be auto, self-hosted, github, or blacksmith" >&2
    exit 64
    ;;
esac

if test "$backend" = blacksmith; then
  selected_backend=blacksmith
  runner='"blacksmith-4vcpu-ubuntu-2404"'
elif test "$backend" = github; then
  selected_backend=github
  runner='"ubuntu-24.04"'
else
  selected_backend=self-hosted
  runner='["self-hosted","Linux","X64"]'
fi

printf 'backend=%s\n' "$selected_backend"
printf 'runner=%s\n' "$runner"

if test -n "${GITHUB_OUTPUT:-}"; then
  {
    printf 'backend=%s\n' "$selected_backend"
    printf 'runner=%s\n' "$runner"
  } >>"${GITHUB_OUTPUT}"
fi
