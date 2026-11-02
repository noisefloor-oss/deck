#!/usr/bin/env bash
# Bound each hosted job to its available CPUs; preserve the existing four-CPU ceiling.
set -euo pipefail
capacity="$(nproc)"
if [[ ! "$capacity" =~ ^[1-9][0-9]*$ ]]; then
  echo "nproc returned an invalid CPU count" >&2
  exit 1
fi
if (( capacity > 4 )); then capacity=4; fi
printf '%s\n' "$capacity"
