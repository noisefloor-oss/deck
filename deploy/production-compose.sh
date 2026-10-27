#!/bin/sh
set -eu

usage() {
  echo "usage: $0 <env-file> config|build|apply|verify|deploy|deploy-cached [builder ...]" >&2
  exit 64
}

test "$#" -ge 2 || usage
env_file=$1
action=$2
shift 2

test -f "$env_file" || {
  echo "production-compose: env file does not exist: $env_file" >&2
  exit 66
}
env_dir=$(CDPATH= cd -- "$(dirname -- "$env_file")" && pwd)
env_file="$env_dir/$(basename -- "$env_file")"

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repo_dir=$(CDPATH= cd -- "$script_dir/.." && pwd)
temporary_files=""

cleanup() {
  for temporary_file in $temporary_files; do
    case "$temporary_file" in
      /tmp/noisedeck-production.*) rm -f -- "$temporary_file" ;;
      *) echo "production-compose: refusing to remove unexpected temporary path: $temporary_file" >&2 ;;
    esac
  done
}
trap cleanup 0
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

compose() {
  if test -n "${NOISEDECK_RELAY_SSH_DIR:-}"; then
    case "$NOISEDECK_RELAY_SSH_DIR" in
      /*) ;;
      *) echo "production-compose: relay SSH directory must be absolute" >&2; return 1 ;;
    esac
    set -- --file "$repo_dir/compose.relay-tunnel.yaml" "$@"
  fi
  if test -n "${NOISEDECK_PREVIEW_TUNNEL_TOKEN_FILE:-}"; then
    case "$NOISEDECK_PREVIEW_TUNNEL_TOKEN_FILE" in
      /*) ;;
      *) echo "production-compose: preview connector token path must be absolute" >&2; return 1 ;;
    esac
    test -f "$NOISEDECK_PREVIEW_TUNNEL_TOKEN_FILE" || {
      echo "production-compose: preview connector token file is missing" >&2
      return 1
    }
    set -- --file "$repo_dir/compose.preview-tunnel.yaml" "$@"
  fi
  # The production frontend is built in go-base, not the final app stage.
  # Keep private font inputs opt-in and out of the checked-in build graph.
  if test -n "${NOISEDECK_LICENSED_FONTS_DIR:-}"; then
    case "$NOISEDECK_LICENSED_FONTS_DIR" in
      /*) ;;
      *) echo "production-compose: licensed font directory must be absolute" >&2; return 1 ;;
    esac
    test -d "$NOISEDECK_LICENSED_FONTS_DIR" || {
      echo "production-compose: licensed font directory does not exist" >&2
      return 1
    }
    set -- --file "$repo_dir/compose.licensed-fonts.yaml" \
      --file "$repo_dir/compose.go-licensed-fonts.yaml" "$@"
  fi
  docker compose \
    --file "$repo_dir/compose.yaml" \
    --file "$repo_dir/compose.mempalace.yaml" \
    --file "$repo_dir/compose.go.yaml" \
    --file "$repo_dir/compose.go-mempalace.yaml" \
    --file "$repo_dir/compose.browser.yaml" \
    --file "$repo_dir/compose.production.yaml" \
    --env-file "$env_file" "$@"
}

build_images() {
  # Compose resolves go-base as an additional build context and builds the
  # complete MemPalace + Go image in one builder graph.
  compose build --pull app chromium
}

apply_images() {
  set --
  if test -n "${NOISEDECK_RELAY_SSH_DIR:-}"; then
    set -- relay-tunnel
  else
    project=$(python3 -c 'import json,sys; print(json.load(sys.stdin)["name"])' <"$rendered") || return 1
    relays=$(docker ps -aq --filter "label=com.docker.compose.project=$project" --filter label=com.docker.compose.service=relay-tunnel) || return 1
    test -z "$relays" || {
      echo "production-compose: existing relay tunnel requires its SSH-directory opt-in; stop the remote writer before retiring it" >&2
      return 1
    }
  fi
  if test -n "${NOISEDECK_PREVIEW_TUNNEL_TOKEN_FILE:-}"; then
    compose up --detach --no-build --no-deps --force-recreate app chromium preview-tunnel "$@"
  else
    # Refuse to strand an existing connector on the previous app namespace.
    # The rendered project identity also covers a stopped or removed app.
    project=$(python3 -c 'import json,sys; name=json.load(sys.stdin).get("name"); assert isinstance(name,str) and name; print(name)' <"$rendered") || return 1
    connectors=$(docker ps -aq --filter "label=com.docker.compose.project=$project" --filter label=com.docker.compose.service=preview-tunnel) || return 1
    test -z "$connectors" || {
      echo "production-compose: existing preview connector requires its token-file opt-in; explicitly retire it before disabling" >&2
      return 1
    }
    compose up --detach --no-build --no-deps --force-recreate app chromium "$@"
  fi
}

preflight() {
  rendered=$(mktemp /tmp/noisedeck-production.XXXXXX) || return 1
  temporary_files="$temporary_files $rendered"
  compose config --format json >"$rendered" || return 1
  python3 "$script_dir/verify-production-compose.py" --check-host-files <"$rendered" || return 1
  "$script_dir/verify-production-tmp-host.sh" "$rendered"
}

wait_healthy() {
  app_id=$(compose ps -q app) || {
    echo "production-compose: cannot inspect the app container" >&2
    return 1
  }
  test -n "$app_id" || {
    echo "production-compose: app container is absent" >&2
    return 1
  }
  attempts=0
  while test "$attempts" -lt 60; do
    if ! health=$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' "$app_id"); then
      echo "production-compose: cannot inspect app health" >&2
      break
    fi
    test "$health" = healthy && return 0
    if test "$health" = exited || test "$health" = dead; then
      break
    fi
    attempts=$((attempts + 1))
    sleep 2
  done
  echo "production-compose: app did not become healthy" >&2
  compose ps >&2
  compose logs --tail 100 app >&2
  return 1
}

preserve_current_image() {
  app_id=$(compose ps -q app) || {
    echo "production-compose: cannot inspect the current app container" >&2
    return 1
  }
  test -z "$app_id" && return 0
  case "$app_id" in
    *'
'*) echo "production-compose: expected exactly one app container" >&2; return 1 ;;
  esac
  image_id=$(docker inspect --format '{{.Image}}' "$app_id") || {
    echo "production-compose: cannot resolve the current app image" >&2
    return 1
  }
  rollback_tag="deck:rollback-pre-production-$(date -u +%Y%m%dT%H%M%SZ)"
  docker image tag "$image_id" "$rollback_tag" || {
    echo "production-compose: cannot preserve the current app image" >&2
    return 1
  }
  echo "production-compose: preserved current image as $rollback_tag"
}

verify_runtime() {
  preflight
  app_id=$(compose ps -q app) || {
    echo "production-compose: cannot inspect the running app container" >&2
    return 1
  }
  browser_id=$(compose ps -q chromium) || {
    echo "production-compose: cannot inspect the running Chromium container" >&2
    return 1
  }
  test -n "$app_id" && test -n "$browser_id" || {
    echo "production-compose: app and Chromium must both be running" >&2
    return 1
  }
  case "$app_id$browser_id" in
    *'
'*) echo "production-compose: expected one app and one Chromium container" >&2; return 1 ;;
  esac
  test "$(docker inspect --format '{{index .Config.Labels "com.noisefloor.deck.capability-profile"}}' "$app_id")" = production-v1 || {
    echo "production-compose: running app lacks the production-v1 capability label" >&2
    return 1
  }
  test "$(docker inspect --format '{{.HostConfig.NetworkMode}}' "$browser_id")" = "container:$app_id" || {
    echo "production-compose: Chromium is not attached to the running app network namespace" >&2
    return 1
  }
  if test -n "${NOISEDECK_PREVIEW_TUNNEL_TOKEN_FILE:-}"; then
    tunnel_id=$(compose ps -q preview-tunnel)
    test -n "$tunnel_id" &&
      test "$(docker inspect --format '{{.HostConfig.NetworkMode}}' "$tunnel_id")" = "container:$app_id" &&
      test "$(docker inspect --format '{{.State.Running}}' "$tunnel_id")" = true || {
        echo "production-compose: preview connector is not running in the current app namespace" >&2
        return 1
      }
  fi
  if test -n "${NOISEDECK_RELAY_SSH_DIR:-}"; then
    relay_id=$(compose ps -q relay-tunnel)
    test -n "$relay_id" &&
      test "$(docker inspect --format '{{.HostConfig.NetworkMode}}' "$relay_id")" = "container:$app_id" &&
      test "$(docker inspect --format '{{.State.Running}}' "$relay_id")" = true &&
      test "$(docker inspect --format '{{.Image}}' "$relay_id")" = "$(docker inspect --format '{{.Image}}' "$app_id")" || {
        echo "production-compose: relay tunnel is not running with the current app image and namespace" >&2
        return 1
      }
    # Read only server identity, never chat data or key material.
    compose exec -T app python3 -c '
import json, pathlib, urllib.request
class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *args, **kwargs):
        return None
token = (pathlib.Path.home()/".a2a/relay-token").read_text()
data = json.dumps({"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"relay-check","version":"1"}}}).encode()
request = urllib.request.Request("http://127.0.0.1:8766/mcp", data=data, headers={"Content-Type":"application/json","Authorization":"Bearer "+token})
with urllib.request.build_opener(NoRedirect()).open(request, timeout=5) as response:
    assert json.load(response)["result"]["serverInfo"]["version"] == "3.11.0"
print("external relay authenticated and reachable")
'
  fi

  compose exec -T app sh -eu -c '
    tmp_fstype=$(stat -f -c %T /tmp)
    case "$tmp_fstype" in tmpfs|ramfs) echo "production /tmp is RAM-backed: $tmp_fstype" >&2; exit 1 ;; esac
    tmp_probe=$(mktemp /tmp/noisedeck-tmp-probe.XXXXXX)
    trap '\''rm -f -- "$tmp_probe"'\'' 0 HUP INT TERM
    printf '\''#!/bin/sh\nexit 0\n'\'' >"$tmp_probe"
    chmod 700 "$tmp_probe"
    "$tmp_probe"
    test "$(mempalace --version)" = "MemPalace 3.11.0"
    /opt/mempalace/venv/bin/python -c '\''
import importlib.util, json, pathlib
assert importlib.util.find_spec("chromadb") is None
config = json.loads((pathlib.Path.home()/".mempalace/config.json").read_text())
assert config["backend"] == "sqlite_exact"
from mempalace.backends import detect_backends_for_path
assert detect_backends_for_path(str(pathlib.Path.home()/".mempalace/palace")) == ["sqlite_exact"]
'\''
    test -x /usr/local/bin/mempalace-mcp
    test -d /home/agent/.mempalace
    test -w /home/agent/.mempalace
    test -f /home/agent/.claude/skills/mempalace/SKILL.md
    test -f /home/agent/.claude/skills/mempalace-logstream/SKILL.md
    test -f /home/agent/.codex/skills/mempalace/SKILL.md
    test -f /home/agent/.codex/skills/mempalace-logstream/SKILL.md
    test -f /home/agent/.kimi-code/skills/mempalace/SKILL.md
    test -f /home/agent/.kimi-code/skills/mempalace-logstream/SKILL.md
    test -f /home/agent/.config/opencode/skills/mempalace/SKILL.md
    test -f /home/agent/.config/opencode/skills/mempalace-logstream/SKILL.md
    test -f /home/agent/.pi/agent/extensions/mempalace.js
    test -f /home/agent/.pi/agent/skills/mempalace/SKILL.md
    test -f /home/agent/.omp/agent/extensions/mempalace.js
    test -f /home/agent/.omp/agent/skills/mempalace/SKILL.md
    test "$(command -v go)" = /usr/local/go/bin/go
    test "$(/bin/bash -lc "command -v go")" = /usr/local/go/bin/go
    test "$(go version | awk "{print \$3}")" = go1.26.10
    test "$(go env GOTOOLCHAIN)" = local
    test ! -w /usr/local/go/bin/go
    command -v cc >/dev/null
    grep -q mempalace /home/agent/.claude.json
    grep -q mempalace /home/agent/.claude/settings.json
    grep -q mempalace /home/agent/.codex/config.toml
    grep -q mempalace /home/agent/.codex/hooks.json
    grep -q mempalace /home/agent/.kimi-code/mcp.json
    grep -q mempalace /home/agent/.config/opencode/opencode.json
    mcp_response=$(mktemp /tmp/noisedeck-mcp.XXXXXX)
    trap '\''rm -f -- "$mcp_response"'\'' 0 HUP INT TERM
    curl --fail --silent --show-error --max-time 5 \
      -H "Content-Type: application/json" \
      -H "Accept: application/json, text/event-stream" \
      -d '\''{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"production-check","version":"1"}}}'\'' \
      --output "$mcp_response" http://127.0.0.1:8765/mcp
    grep -q '"serverInfo"' "$mcp_response"
    grep -q '3.11.0' "$mcp_response"
  '

  origin=$(python3 -c 'import json,sys; print(json.load(sys.stdin)["services"]["app"]["environment"]["NOISEDECK_SERVER_ALLOWED_ORIGINS"])' <"$rendered")
  app_playwright=$(compose exec -T app node -p 'require("/opt/noise-browser/node_modules/playwright/package.json").version')
  browser_playwright=$(compose exec -T chromium node -p 'require("/opt/noise-browser/browser-versions.json").playwright')
  test "$app_playwright" = "$browser_playwright" || {
    echo "production-compose: app Playwright client differs from the qualified browser service" >&2
    return 1
  }
  compose exec -T -e NOISEDECK_VERIFY_ORIGIN="$origin" chromium sh -eu -c '
    cd /opt/noise-browser
    node --input-type=module -e '\''
      import { chromium } from "playwright";
      import { readFileSync } from "node:fs";
      import { createRequire } from "node:module";
      const require = createRequire(import.meta.url);
      const versions = JSON.parse(readFileSync("/opt/noise-browser/browser-versions.json", "utf8"));
      if (require("playwright/package.json").version !== versions.playwright) throw new Error("Playwright service version mismatch");
      const browser = await chromium.connect("ws://127.0.0.1:3000/");
      if (browser.version() !== versions.chromium) throw new Error(`Chromium version mismatch: ${browser.version()}`);
      console.log(JSON.stringify({ playwright: versions.playwright, chromium: browser.version() }));
      const context = await browser.newContext();
      try {
        const page = await context.newPage();
        await page.goto("http://127.0.0.1:8837", {waitUntil: "domcontentloaded"});
        await page.getByText("Sign in with Google", {exact: true}).waitFor({state: "visible", timeout: 15000});
        if (await page.locator("input[type=password]").count() !== 0) throw new Error("password login rendered");
        if (await page.getByText("Sign in with Google", {exact: true}).count() !== 1) throw new Error("OIDC login did not render");
        await page.goto(process.env.NOISEDECK_VERIFY_ORIGIN, {waitUntil: "domcontentloaded"});
        await page.getByText("Sign in with Google", {exact: true}).waitFor({state: "visible", timeout: 15000});
        if (await page.locator("input[type=password]").count() !== 0) throw new Error("public password login rendered");
        if (await page.getByText("Sign in with Google", {exact: true}).count() !== 1) throw new Error("public OIDC login did not render from Chromium");
      } finally {
        await context.close();
        await browser.close();
      }
    '\''
  '

  headers=$(mktemp /tmp/noisedeck-production.XXXXXX)
  temporary_files="$temporary_files $headers"
  code=$(curl --silent --show-error --output /dev/null --dump-header "$headers" --write-out '%{http_code}' "$origin/auth/session")
  test "$code" = 401
  grep -qi '^X-Noisedeck-Auth-Mode: oidc' "$headers"
  code=$(curl --silent --show-error --output /dev/null --dump-header "$headers" --write-out '%{http_code}' "$origin/auth/login")
  test "$code" = 302
  grep -qi '^location: https://accounts.google.com/' "$headers"
  echo "production-compose: running MemPalace, browser, and OIDC verified"
}

case "$action" in
  deploy-cached)
    test "$#" -eq 0 || usage
    test -n "${NOISEDECK_RELEASE_CACHE_DIR:-}" && test -n "${NOISEDECK_RELEASE_REGISTRY:-}" || {
      echo "production-compose: cached deploy requires explicit cache directory and registry" >&2
      exit 64
    }
    preflight
    preserve_current_image
    python3 "$script_dir/release-images.py" select \
      --registry "$NOISEDECK_RELEASE_REGISTRY" \
      --prefix "${NOISEDECK_RELEASE_PACKAGE_PREFIX:-deck-deploy}" \
      --state-dir "$NOISEDECK_RELEASE_CACHE_DIR" --compose "$rendered" \
      --checkout "$repo_dir" \
      --font-dir "${NOISEDECK_LICENSED_FONTS_DIR:-}"
    apply_images
    wait_healthy
    verify_runtime
    ;;
  config)
    test "$#" -eq 0 || usage
    preflight
    ;;
  build)
    test "$#" -eq 0 || usage
    preflight
    preserve_current_image
    build_images
    ;;
  apply)
    test "$#" -eq 0 || usage
    preflight
    preserve_current_image
    apply_images
    wait_healthy
    verify_runtime
    ;;
  verify)
    test "$#" -eq 0 || usage
    wait_healthy
    verify_runtime
    ;;
  deploy)
    test "$#" -gt 0 || {
      echo "production-compose: deploy requires every BuildKit builder used by the deployment" >&2
      exit 64
    }
    preflight
    preserve_current_image
    build_images
    apply_images
    wait_healthy
    verify_runtime
    # Cache removal is unreachable unless every production verification above passed.
    "$script_dir/prune-build-cache.sh" --after-verified-deploy "$@"
    ;;
  *) usage ;;
esac
