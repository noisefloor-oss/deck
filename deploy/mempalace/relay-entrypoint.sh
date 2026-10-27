#!/bin/sh
set -eu

test -n "${MEMPALACE_MCP_HTTP_TOKEN:-}" || {
  echo 'relay requires a bearer token' >&2
  exit 78
}

palace=/data/relay
expect_palace=no
for argument in "$@"; do
  if [ "$expect_palace" = yes ]; then
    palace=$argument
    expect_palace=no
    continue
  fi
  case "$argument" in
    --palace) expect_palace=yes ;;
    --palace=*) palace=${argument#--palace=} ;;
  esac
done
test "$expect_palace" = no || { echo 'relay --palace requires a path' >&2; exit 64; }

/opt/mempalace/venv/bin/python /usr/local/lib/migrate-mempalace-to-sqlite.py \
  --config "${MEMPALACE_CONFIG_DIR}/config.json" --palace "$palace"
exec /opt/mempalace/venv/bin/mempalace-mcp "$@"
