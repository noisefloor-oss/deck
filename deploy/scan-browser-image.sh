#!/bin/sh
set -eu

if [ "$#" -ne 2 ]; then
  echo "usage: scan-browser-image.sh IMAGE REPORT.json" >&2
  exit 2
fi
test "$(uname -m)" = x86_64
image=$1
report=$2
# Scan the built image by immutable ID, never a tag that could move mid-scan.
image_id=$(docker image inspect "$image" --format '{{.Id}}')
scan_dir=$(mktemp -d)
trap 'rm -rf "$scan_dir"' EXIT HUP INT TERM
curl --fail --location --silent --show-error \
  https://github.com/aquasecurity/trivy/releases/download/v0.76.1/trivy_0.76.1_Linux-64bit.tar.gz \
  --output "$scan_dir/trivy.tar.gz"
printf '%s  %s\n' c7cbe3137fb8a48a17574bede3dd6beb16742d1e9772929940fc90082c39d9c0 "$scan_dir/trivy.tar.gz" | sha256sum --check --status
tar -xzf "$scan_dir/trivy.tar.gz" -C "$scan_dir" trivy
"$scan_dir/trivy" image --image-src docker --scanners vuln \
  --cache-dir "$scan_dir/cache" --timeout 10m \
  --severity HIGH,CRITICAL --exit-code 1 --format json --output "$report" "$image_id"
