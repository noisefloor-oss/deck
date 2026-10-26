# syntax=docker/dockerfile:1.7@sha256:a57df69d0ea827fb7266491f2813635de6f17269be881f696fbfdf2d83dda33e

ARG NOISE_TASKS_REF=a03efe15d6bfcd68fa3cc812d612c63306736149
ARG NOISE_TASKS_VERSION=2.10.1
ARG NOISE_VISUAL_DIFF_REF=9b5b0348791991f37e2cb5a4078f17ae55b7c644
ARG NOISE_VISUAL_DIFF_VERSION=0.14.1
ARG NOISE_SDLC_REF=abadbafa9f8a8fd3f41013efeb57e8ae55ecc9e2
ARG NOISE_SDLC_VERSION=0.4.1

ARG KIMI_VERSION=2.3.2
ARG OPENCODE_VERSION=1.19.4
ARG COMMAND_CODE_VERSION=1.88.0
ARG CLINE_VERSION=3.0.73
ARG KILO_VERSION=7.8.17
ARG QWEN_VERSION=0.26.1
ARG CRUSH_VERSION=0.98.2

# A local named context overrides this empty input stage when fonts are supplied.
FROM scratch AS licensed-fonts
COPY deploy/fonts-unsupplied /.no-licensed-fonts

FROM node:26.12.1-bookworm-slim@sha256:28147155d47dc31abe0a46e0d12b59b2fb3d0a2dcc5dc65fce76a074180b16fe AS web-build

WORKDIR /src/web
RUN npm install --global pnpm@11.18.0 \
    && test "$(pnpm --version)" = "11.18.0"
COPY web/package.json web/pnpm-lock.yaml ./
RUN pnpm install --frozen-lockfile
COPY web/ ./
# Bind inputs participate in BuildKit's cache key, including their removal.
ARG NOISEDECK_FONTS=auto
RUN --mount=type=bind,from=licensed-fonts,target=/run/noisedeck-fonts \
    if [ ! -f /run/noisedeck-fonts/.no-licensed-fonts ]; then \
      export NOISEDECK_LICENSED_FONTS_DIR=/run/noisedeck-fonts; \
    fi \
    && NOISEDECK_EMBED_BUILD=1 NOISEDECK_FONTS="${NOISEDECK_FONTS}" pnpm build \
    && test -f dist/index.html \
    && ! find dist \( -name '*.map' -o -name 'preview.html' \) | grep -q .

# Chrome security updates can arrive between Playwright releases. These
# archives contain the fixed browser for each supported Linux architecture.
FROM scratch AS chrome-amd64
ADD --checksum=sha256:e6c94490f0b6b337225aeb2d2a5034858516b7017356980ba94f5176d8774a50 https://storage.googleapis.com/chrome-for-testing-public/156.0.8078.47/linux64/chrome-linux64.zip /chrome.zip

FROM scratch AS chrome-arm64
ADD --checksum=sha256:9e37000e3ee2731e45e475a6a2eac587d3c0ee5f8aef7cab5211ecd52cf3fb7b https://storage.googleapis.com/chrome-for-testing-public/156.0.8078.47/linux-arm64/chrome-linux-arm64.zip /chrome.zip

FROM chrome-${TARGETARCH} AS chrome-archive

# Keep the upstream Chromium libraries, but exclude its unused Firefox/WebKit
# engines, WebKit media plugins and npm tooling. The server needs only the
# lockfile-resolved Playwright package and its playwright-core dependency.
FROM mcr.microsoft.com/playwright:v1.64.1-noble@sha256:47b510dd9230bf0b9bfc9b20102241e577ea7a99334ab4c3ca19f19770e9b8ea AS browser

# Remove the older bundled Chromium so it cannot be selected accidentally.
COPY --from=chrome-archive /chrome.zip /tmp/chrome.zip
RUN apt-get update \
    && apt-get install -y --no-install-recommends unzip \
    && apt-get purge -y gstreamer1.0-plugins-bad libgstreamer-plugins-bad1.0-0 \
    && rm -rf /var/lib/apt/lists/* /ms-playwright \
      /usr/lib/node_modules/npm /usr/bin/npm /usr/bin/npx \
    && unzip -q /tmp/chrome.zip -d /tmp/chrome \
    && mkdir -p /opt/noise-browser \
    && mv /tmp/chrome/chrome-* /opt/noise-browser/chrome \
    && rm -rf /tmp/chrome /tmp/chrome.zip \
    && test "$(/opt/noise-browser/chrome/chrome --version | awk '{print $NF}')" = "156.0.8078.47" \
    && ldd /opt/noise-browser/chrome/chrome > /tmp/chrome-libraries \
    && ! grep -q "not found" /tmp/chrome-libraries \
    && rm /tmp/chrome-libraries \
    && ! command -v npm \
    && ! command -v npx

# The G3 root signed the compromised eMudhra intermediates.
RUN sed -i '\|^mozilla/emSign_ECC_Root_CA_-_G3.crt$|s|^|!|' /etc/ca-certificates.conf \
    && update-ca-certificates --fresh \
    && test ! -e /etc/ssl/certs/emSign_ECC_Root_CA_-_G3.pem

# Chromium reads local distrust from NSS, independently of the OS CA bundle.
COPY deploy/distrusted-emudhra-g3.crt /opt/noise-browser/distrusted-emudhra-g3.crt
RUN apt-get update \
    && apt-get install -y --no-install-recommends libnss3-tools \
    && rm -rf /var/lib/apt/lists/* \
    && test "$(openssl x509 -in /opt/noise-browser/distrusted-emudhra-g3.crt -outform DER | sha256sum | cut -d ' ' -f 1)" = "86a1ecba089c4a8d3bbe2734c612ba341d813e043cf9e8a862cd5c57a36bbe6b" \
    && mkdir -p "/opt/noise-browser/nssdb" \
    && certutil -N --empty-password -d "sql:/opt/noise-browser/nssdb" \
    && certutil -A -d "sql:/opt/noise-browser/nssdb" -n emudhra-g3-distrusted -t "p,p,p" -i /opt/noise-browser/distrusted-emudhra-g3.crt \
    && find "/opt/noise-browser/nssdb" -type f -exec chmod 0444 {} + \
    && find "/opt/noise-browser/nssdb" -type d -exec chmod 0555 {} +

COPY deploy/assert-tls-roots.mjs /tmp/assert-tls-roots.mjs
RUN node /tmp/assert-tls-roots.mjs && rm /tmp/assert-tls-roots.mjs

RUN --mount=type=bind,from=web-build,source=/src/web/node_modules,target=/run/web-node-modules \
    client="$(readlink -f /run/web-node-modules/playwright)" \
    && install -d -m 0755 /opt/noise-browser/node_modules \
    && cp -RL "${client}" "${client}/../playwright-core" /opt/noise-browser/node_modules/ \
    && node -e "require('/opt/noise-browser/node_modules/playwright')"
COPY --chmod=0555 deploy/browser-entrypoint.sh /opt/noise-browser/entrypoint.sh
ENTRYPOINT ["/opt/noise-browser/entrypoint.sh"]
COPY deploy/browser-server.mjs /opt/noise-browser/server.mjs
COPY deploy/browser-versions.json /opt/noise-browser/browser-versions.json
RUN test "$(node --version)" = "v24.22.1" \
    && test "$(node -p "require('/opt/noise-browser/node_modules/playwright/package.json').version")" = "1.64.1" \
    && node --check /opt/noise-browser/server.mjs \
    && chmod 0555 /opt/noise-browser/server.mjs

FROM golang:1.26.10-bookworm@sha256:ee3901c61e423732605a51f1b8157c849ec01194ea0a274213b5202b2c11e7bf AS go-build

ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . ./
COPY --from=web-build /src/web/dist/ ./internal/server/webassets/dist/
RUN --mount=type=cache,id=deck-go-build,target=/root/.cache/go-build,sharing=locked \
    CGO_ENABLED=1 go build \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/noise-deck ./cmd/noisedeck \
    && /out/noise-deck version \
    && CGO_ENABLED=0 go build -o /out/noisedeck-provision ./cmd/noisedeck-provision

FROM golang:1.26.10-bookworm@sha256:ee3901c61e423732605a51f1b8157c849ec01194ea0a274213b5202b2c11e7bf AS task-workload-latch-build
WORKDIR /src
COPY go.mod go.sum ./
COPY cmd/task-workload-latch/ ./cmd/task-workload-latch/
RUN set -eu; \
    CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/task-workload-latch ./cmd/task-workload-latch; \
    set +e; /out/task-workload-latch >/dev/null 2>&1; latch_status="$?"; set -e; \
    test "${latch_status}" -eq 64

FROM golang:1.26.10-bookworm@sha256:ee3901c61e423732605a51f1b8157c849ec01194ea0a274213b5202b2c11e7bf AS noise-tasks-build

# The task engine is packaged from an exact reviewed revision. Its host and
# compatibility shim are linked to the fixed libexec root selected here.
# Tasks 2.5 packaging also installs a setuid-root worktree broker, so the
# package step runs as root into a private 0700 staging root: the script
# refuses a missing or accessible DESTDIR instead of building.
ARG NOISE_TASKS_REPO=https://github.com/noisefloor-oss/tasks.git
ARG NOISE_TASKS_REF
ARG NOISE_TASKS_VERSION
RUN --mount=type=secret,id=github_token \
    --mount=type=cache,id=deck-go-build,target=/root/.cache/go-build,sharing=locked \
    tasks_git_config="$(mktemp)" \
    && trap 'rm -f "${tasks_git_config}"' EXIT \
    && if [ -s /run/secrets/github_token ]; then \
      auth="$(printf 'x-access-token:%s' "$(cat /run/secrets/github_token)" | base64 -w0)"; \
      git config --file "${tasks_git_config}" http.extraheader "Authorization: Basic ${auth}"; \
      unset auth; \
    fi \
    && GIT_CONFIG_GLOBAL="${tasks_git_config}" \
      git clone --no-checkout "${NOISE_TASKS_REPO}" /src/noise-tasks \
    && rm -f "${tasks_git_config}" \
    && trap - EXIT \
    && git -C /src/noise-tasks checkout --detach "${NOISE_TASKS_REF}" \
    && test "$(git -C /src/noise-tasks rev-parse HEAD)" = "${NOISE_TASKS_REF}" \
    && cd /src/noise-tasks \
    && install -d -m 0700 /out \
    && CGO_ENABLED=0 make package DESTDIR=/out PREFIX=/usr/local VERSION="${NOISE_TASKS_VERSION}"

FROM golang:1.26.10-bookworm@sha256:ee3901c61e423732605a51f1b8157c849ec01194ea0a274213b5202b2c11e7bf AS noise-sdlc-build

# The SDLC catalog plugin is packaged from an exact reviewed revision, same
# fetch contract as noise-tasks above. Its version is a source constant
# (internal/sdlc.Version), so the build verifies the compiled artifact against
# the pin rather than stamping it.
ARG NOISE_SDLC_REPO=https://github.com/noisefloor-oss/sdlc.git
ARG NOISE_SDLC_REF
ARG NOISE_SDLC_VERSION
RUN --mount=type=secret,id=github_token \
    --mount=type=cache,id=deck-go-build,target=/root/.cache/go-build,sharing=locked \
    sdlc_git_config="$(mktemp)" \
    && trap 'rm -f "${sdlc_git_config}"' EXIT \
    && if [ -s /run/secrets/github_token ]; then \
      auth="$(printf 'x-access-token:%s' "$(cat /run/secrets/github_token)" | base64 -w0)"; \
      git config --file "${sdlc_git_config}" http.extraheader "Authorization: Basic ${auth}"; \
      unset auth; \
    fi \
    && GIT_CONFIG_GLOBAL="${sdlc_git_config}" \
      git clone --no-checkout "${NOISE_SDLC_REPO}" /src/noise-sdlc \
    && rm -f "${sdlc_git_config}" \
    && trap - EXIT \
    && git -C /src/noise-sdlc checkout --detach "${NOISE_SDLC_REF}" \
    && test "$(git -C /src/noise-sdlc rev-parse HEAD)" = "${NOISE_SDLC_REF}" \
    && cd /src/noise-sdlc \
    && CGO_ENABLED=0 go build -trimpath \
      -o /out/usr/local/libexec/noise/noise-sdlc ./cmd/noise-sdlc \
    && test "$(/out/usr/local/libexec/noise/noise-sdlc --version)" = "${NOISE_SDLC_VERSION}"

FROM golang:1.26.10-bookworm@sha256:ee3901c61e423732605a51f1b8157c849ec01194ea0a274213b5202b2c11e7bf AS visual-diff-go

FROM node:24.22.1-bookworm@sha256:6449e19d8184d84062525b6a7524def03146c2cb04a7f08b076680d0e8e4240a AS noise-visual-diff-build

# The SDK supports Visual Diff's anchored file-server helper.
# Keep the SDK in this build stage; only the resulting SEA enters runtime.
COPY --from=visual-diff-go /usr/local/go/ /usr/local/go/
ENV PATH="/usr/local/go/bin:${PATH}"

# The design-fidelity tool is packaged from an exact reviewed revision, same
# contract as noise-tasks above. The revision is the commit the release tag
# points at; the ref and version gates below are what bind it. Its SEA build
# postjects the stage's own Node binary, so the base image must equal the
# patched Node 24 runtime; the multi-arch base yields per-arch payloads
# for free.
ARG NOISE_VISUAL_DIFF_REPO=https://github.com/noisefloor-oss/visual-diff.git
ARG NOISE_VISUAL_DIFF_REF
ARG NOISE_VISUAL_DIFF_VERSION
RUN --mount=type=secret,id=github_token \
    vd_git_config="$(mktemp)" \
    && trap 'rm -f "${vd_git_config}"' EXIT \
    && if [ -s /run/secrets/github_token ]; then \
      auth="$(printf 'x-access-token:%s' "$(cat /run/secrets/github_token)" | base64 -w0)"; \
      git config --file "${vd_git_config}" http.extraheader "Authorization: Basic ${auth}"; \
      unset auth; \
    fi \
    && GIT_CONFIG_GLOBAL="${vd_git_config}" \
      git clone --no-checkout "${NOISE_VISUAL_DIFF_REPO}" /src/noise-visual-diff \
    && rm -f "${vd_git_config}" \
    && trap - EXIT \
    && git -C /src/noise-visual-diff checkout --detach "${NOISE_VISUAL_DIFF_REF}" \
    && test "$(git -C /src/noise-visual-diff rev-parse HEAD)" = "${NOISE_VISUAL_DIFF_REF}" \
    && cd /src/noise-visual-diff \
    && npm ci \
    && npm run build:sea \
    && test "$(node -p "require('./dist/manifest.json').version")" = "${NOISE_VISUAL_DIFF_VERSION}" \
    && install -D -m 0555 dist/noise-visual-diff /out/usr/local/libexec/noise/noise-visual-diff

# The runtime's tmux is BUILT, not taken from the distribution. Debian
# bookworm ships 3.3a, while every tmux behaviour this product measured — the
# pane identity tuple, remain-on-exit exit evidence, synchronized updates —
# was measured by CI against 3.6a. Shipping 3.3a runs the console on a tmux no
# test ever exercised. 3.3a also emits synchronized updates in the legacy
# iTerm2 DCS form no browser implements, so a provider's full-screen repaint
# reaches you unbatched and visibly redraws.
#
# The version and digest are the ones ci-fast.yaml pins, so the shipped binary
# and the tested binary are one artifact. The builder is the Go stage's base,
# already pinned above, rather than a new image to trust. The link is static so
# the runtime carries the binary alone, with no libevent or ncurses runtime to
# drift underneath it.
FROM golang:1.26.10-bookworm@sha256:ee3901c61e423732605a51f1b8157c849ec01194ea0a274213b5202b2c11e7bf AS tmux-build
ARG TMUX_VERSION=3.6a
ARG TMUX_SHA256=b6d8d9c76585db8ef5fa00d4931902fa4b8cbe8166f528f44fc403961a3f3759
RUN set -eu; \
    apt-get update; \
    apt-get install -y --no-install-recommends bison libevent-dev libncurses-dev pkg-config; \
    rm -rf /var/lib/apt/lists/*; \
    curl --fail --location --silent --show-error \
      "https://github.com/tmux/tmux/releases/download/${TMUX_VERSION}/tmux-${TMUX_VERSION}.tar.gz" \
      --output /tmp/tmux.tar.gz; \
    echo "${TMUX_SHA256}  /tmp/tmux.tar.gz" | sha256sum --check --strict; \
    tar --extract --gzip --file /tmp/tmux.tar.gz --directory /tmp; \
    cd "/tmp/tmux-${TMUX_VERSION}"; \
    ./configure --prefix=/usr/local --enable-static; \
    make --jobs "$(nproc)"; \
    make DESTDIR=/out install; \
    test "$(/out/usr/local/bin/tmux -V)" = "tmux ${TMUX_VERSION}"

# Install the large npm harness bundle while Deck and Noise tools compile.
FROM node:26.12.1-bookworm-slim@sha256:28147155d47dc31abe0a46e0d12b59b2fb3d0a2dcc5dc65fce76a074180b16fe AS npm-harnesses

ARG TARGETARCH
ARG KIMI_VERSION
ARG OPENCODE_VERSION
ARG COMMAND_CODE_VERSION
ARG CLINE_VERSION
ARG KILO_VERSION
ARG QWEN_VERSION
ARG CRUSH_VERSION
# Even version/help probes can launch Cline's detached npm updater.
ENV DEBIAN_FRONTEND=noninteractive \
    HOME=/root \
    CLINE_NO_AUTO_UPDATE=1 \
    npm_config_prefix=/opt/npm-harnesses \
    PATH=/opt/npm-harnesses/bin:$PATH
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl git python3 build-essential \
    && rm -rf /var/lib/apt/lists/*

# Kimi and the rest of the npm-distributed CLIs, pinned to the
# versions the launch gate enforces.
RUN --mount=type=cache,target=/root/.npm,sharing=locked \
    npm install --global \
      "@moonshot-ai/kimi-code@${KIMI_VERSION}" \
      "opencode-ai@${OPENCODE_VERSION}" \
      "command-code@${COMMAND_CODE_VERSION}" \
      "cline@${CLINE_VERSION}" \
      "@kilocode/cli@${KILO_VERSION}" \
      "@qwen-code/qwen-code@${QWEN_VERSION}" \
      "@charmland/crush@${CRUSH_VERSION}" \
    && find /opt/npm-harnesses/lib/node_modules \
         \( -type d -name "*-baseline" -o -type f -name "*-baseline.*" \) \
         -exec rm -rf {} + \
    && if test "${TARGETARCH}" = amd64; then \
         find /opt/npm-harnesses/lib/node_modules \
           -type d \( -name "*darwin*" -o -name "*win32*" -o -name "*windows*" -o -name "*arm64*" \) \
           -prune -exec rm -rf {} +; \
       else \
         find /opt/npm-harnesses/lib/node_modules \
           -type d \( -name "*darwin*" -o -name "*win32*" -o -name "*windows*" -o -name "*x64*" -o -name "*amd64*" \) \
           -prune -exec rm -rf {} +; \
       fi \
    && test "$(kimi --version | awk '{print $1}')" = "${KIMI_VERSION}" \
    && test "$(opencode --version)" = "${OPENCODE_VERSION}" \
    && test "$(command-code --no-auto-update --version)" = "${COMMAND_CODE_VERSION}" \
    && command-code --no-auto-update --help >/tmp/command-code-help \
    && grep -q -- '--no-auto-update' /tmp/command-code-help \
    && grep -q -- '--session' /tmp/command-code-help \
    && grep -q -- '--resume' /tmp/command-code-help \
    && grep -q -- '--skip-onboarding' /tmp/command-code-help \
    && grep -q -- '--yolo' /tmp/command-code-help \
    && test "$(cline --version)" = "${CLINE_VERSION}" \
    && cline --help >/tmp/cline-help \
    && grep -q -- '--tui' /tmp/cline-help \
    && grep -Fq -- '--auto-approve <boolean>' /tmp/cline-help \
    && test "$(kilo --version)" = "${KILO_VERSION}" \
    && kilo --help >/tmp/kilo-help 2>&1 \
    && grep -q -- '--session' /tmp/kilo-help \
    && grep -q -- '--auto' /tmp/kilo-help \
    && test "$(qwen --version)" = "${QWEN_VERSION}" \
    && qwen probe --help >/tmp/qwen-help \
    && grep -q -- '--resume' /tmp/qwen-help \
    && grep -q -- '--chat-recording' /tmp/qwen-help \
    && grep -q -- '--yolo' /tmp/qwen-help \
    && ! qwen --approval-mode yolo sessions list >/dev/null 2>&1 \
    && test "$(crush --version)" = "crush version v${CRUSH_VERSION}" \
    && rm -f /tmp/command-code-help /tmp/cline-help /tmp/kilo-help /tmp/qwen-help


# Shared Deck runtime, utilities, and Noise tools. Go remains a build
# dependency; optional runtime toolchains use derived images.
FROM node:26.12.1-bookworm-slim@sha256:28147155d47dc31abe0a46e0d12b59b2fb3d0a2dcc5dc65fce76a074180b16fe AS core

ARG NOISE_TASKS_VERSION
ARG NOISE_VISUAL_DIFF_VERSION
ARG NOISE_SDLC_VERSION
ARG TARGETARCH
ARG GH_CLI_VERSION=2.102.0
ARG GH_CLI_SHA256_AMD64=bb766f710eef8ede859c18578c72c327597cd4c8a85b06001b1f3843c6019386
ARG GH_CLI_SHA256_ARM64=7862c86c72f43df3a2d93ddde6f473285b4e2af61b494849846827e513ef6484
ARG GH_CLI_SHA256_DARWIN_ARM64=da922c20d1792e5b2cbf375593d7a658acf034c12c84e007e71c76ef959c337e

# gh has no browser to open in this image: without a browser command,
# gh auth login and gh browse die spawning xdg-open with a confusing
# error. echo stands in and prints the device-login URL so you
# can open it in a browser on your own machine.
ENV DEBIAN_FRONTEND=noninteractive \
    LANG=C.UTF-8 \
    LC_ALL=C.UTF-8 \
    TERM=xterm-256color \
    DISABLE_AUTOUPDATER=1 \
    CLINE_NO_AUTO_UPDATE=1 \
    GH_BROWSER=echo

# The utilities every variant needs: TLS roots, a fetcher, git and ssh for
# the SDLC an agent session runs, Python, a fast grep, an init, and unzip.
# A C compiler is not among them; the full image adds one for native
# extensions, and no variant can build Deck itself.
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
      age \
      ca-certificates \
      curl \
      git \
      jq \
      openssh-client \
      procps \
      python3 \
      ripgrep \
      tini \
      unzip \
    && command -v jq \
    && ps -p $$ -o pid= >/dev/null \
    && rm -rf /var/lib/apt/lists/*

# The G3 root signed the compromised eMudhra intermediates.
RUN sed -i '\|^mozilla/emSign_ECC_Root_CA_-_G3.crt$|s|^|!|' /etc/ca-certificates.conf \
    && update-ca-certificates --fresh \
    && test ! -e /etc/ssl/certs/emSign_ECC_Root_CA_-_G3.pem

COPY deploy/assert-tls-roots.mjs /tmp/assert-tls-roots.mjs
RUN node /tmp/assert-tls-roots.mjs && rm /tmp/assert-tls-roots.mjs

COPY --from=tmux-build --chmod=0555 /out/usr/local/bin/tmux /usr/local/bin/tmux
RUN test "$(tmux -V)" = "tmux 3.6a"

# The canonical host and compatibility binaries are immutable image content.
# Both dispatchers resolve only this fixed plugin root.
COPY --from=noise-tasks-build --chmod=0555 /out/usr/local/bin/noise /usr/local/bin/noise
COPY --from=noise-tasks-build --chmod=0555 /out/usr/local/bin/noise-tasks /usr/local/bin/noise-tasks
COPY --from=noise-tasks-build --chmod=0555 /out/usr/local/bin/noisetask /usr/local/bin/noisetask
COPY --from=noise-tasks-build --chmod=0555 /out/usr/local/lib/noise/payload/noise-tasks /usr/local/libexec/noise/noise-tasks
COPY --from=noise-visual-diff-build --chmod=0555 /out/usr/local/libexec/noise/noise-visual-diff /usr/local/libexec/noise/noise-visual-diff
COPY --from=noise-sdlc-build --chmod=0555 /out/usr/local/libexec/noise/noise-sdlc /usr/local/libexec/noise/noise-sdlc
# The §5.3 plugin help contract (--help/help/-h and version on stdout, exit 0
# from any cwd) is build-enforced only for the plugins that conform today;
# the probe extends to every plugin once the allowlisted ones (the sdlc
# `version` subcommand, visual-diff, the tasks `help` verb) land their help
# support.
RUN set -eu; \
    test "$(noise version)" = "noise ${NOISE_TASKS_VERSION}"; \
    test "$(noise plugins)" = "$(printf 'sdlc\ntasks\nvisual-diff')"; \
    test "$(noise tasks version)" = "noise-tasks ${NOISE_TASKS_VERSION}"; \
    test "$(noise visual-diff version)" = "noise-visual-diff ${NOISE_VISUAL_DIFF_VERSION}"; \
    test "$(noise sdlc --version)" = "${NOISE_SDLC_VERSION}"; \
    test "$(noise-tasks version)" = "noise-tasks ${NOISE_TASKS_VERSION}"; \
    test -n "$(noise tasks --help)"; \
    test -n "$(noise tasks -h)"; \
    test -n "$(noise sdlc --help)"; \
    test -n "$(noise sdlc help)"; \
    test -n "$(noise sdlc -h)"; \
    test -n "$(noise visual-diff --help)"; \
    test -n "$(noise visual-diff help)"; \
    noisetask version >/tmp/noisetask.stdout 2>/tmp/noisetask.stderr; \
    test "$(cat /tmp/noisetask.stdout)" = "noise-tasks ${NOISE_TASKS_VERSION}"; \
    test "$(cat /tmp/noisetask.stderr)" = "warning: noisetask is deprecated; use noise tasks (or noise-tasks); removal is gated by the published migration plan"; \
    test "$(wc -l </tmp/noisetask.stderr)" -eq 1; \
    noise tasks context --json >/tmp/noise-context.json; \
    node -e 'const c=JSON.parse(require("fs").readFileSync("/tmp/noise-context.json","utf8")); if(c.schemaVersion!==5 || c.storeOwnerBoundary!=="" || c.storeSchemaVersion!==0 || c.engineSchemaVersion!==6 || c.projectBoundary!=="/" || c.storePath!=="/.tasks/tasks.sqlite" || c.docsPath!==".tasks/docs/PRD.md" || c.preferencesPath!==".tasks/preferences.json" || c.initialized!==false || c.blocked!==false || c.blockedReason!=="") process.exit(1)'; \
    noise tasks live feed --help >/tmp/feed.stdout 2>/tmp/feed.stderr; \
    test ! -s /tmp/feed.stderr; \
    grep -q -- '-since' /tmp/feed.stdout; \
    code=0; noise tasks migrate >/tmp/migrate.stdout 2>/tmp/migrate.stderr || code=$?; \
    test "$code" -eq 2; \
    test ! -s /tmp/migrate.stdout; \
    noise tasks milestone list --help >/tmp/milestone.stdout 2>/tmp/milestone.stderr; \
    test ! -s /tmp/milestone.stderr; \
    grep -q -- 'usage: noise-tasks milestone list' /tmp/milestone.stdout; \
    noise tasks set-milestone --help >/tmp/set-milestone.stdout 2>/tmp/set-milestone.stderr; \
    test ! -s /tmp/set-milestone.stderr; \
    grep -q -- 'usage: noise-tasks set-milestone' /tmp/set-milestone.stdout; \
    printf '#!/bin/sh\necho unsafe\n' >/tmp/noise-planted; \
    chmod 0755 /tmp/noise-planted; \
    code=0; PATH="/tmp:/usr/local/bin:/usr/bin:/bin" NOISE_LIBEXEC_ROOT=/tmp noise planted >/tmp/planted.stdout 2>/tmp/planted.stderr || code=$?; \
    test "$code" -eq 127; \
    test ! -s /tmp/planted.stdout; \
    test "$(cat /tmp/planted.stderr)" = "noise: plugin not installed: planted"; \
    rm -f /tmp/noisetask.stdout /tmp/noisetask.stderr /tmp/noise-context.json \
      /tmp/milestone.stdout /tmp/milestone.stderr /tmp/set-milestone.stdout /tmp/set-milestone.stderr \
      /tmp/feed.stdout /tmp/feed.stderr /tmp/migrate.stdout /tmp/migrate.stderr /tmp/noise-planted /tmp/planted.stdout /tmp/planted.stderr

# The gh CLI ships in the image so sessions can run a full
# GitHub SDLC. Pinned version + checksum, same contract as the Claude
# binary above.
RUN case "${TARGETARCH}" in \
      amd64) gh_arch=amd64; gh_sha256="${GH_CLI_SHA256_AMD64}" ;; \
      arm64) gh_arch=arm64; gh_sha256="${GH_CLI_SHA256_ARM64}" ;; \
      *) echo "unsupported target architecture: ${TARGETARCH}" >&2; exit 1 ;; \
    esac \
    && curl -fsSL \
      "https://github.com/cli/cli/releases/download/v${GH_CLI_VERSION}/gh_${GH_CLI_VERSION}_linux_${gh_arch}.tar.gz" \
      -o /tmp/gh.tar.gz \
    && echo "${gh_sha256}  /tmp/gh.tar.gz" | sha256sum -c - \
    && tar -xzf /tmp/gh.tar.gz -C /tmp \
    && install -m 0555 "/tmp/gh_${GH_CLI_VERSION}_linux_${gh_arch}/bin/gh" /usr/local/bin/gh \
    && test "$(gh --version | awk '{print $3}')" = "${GH_CLI_VERSION}" \
    && rm -rf /tmp/gh.tar.gz "/tmp/gh_${GH_CLI_VERSION}_linux_${gh_arch}"

# The SessionStart hook receiver, first-boot provisioner, and core bootstrap.
COPY --from=go-build /out/noisedeck-provision /usr/local/bin/noisedeck-provision
COPY deploy/provider-hook/noisedeck-provider-hook /usr/local/bin/noisedeck-provider-hook
COPY deploy/provider-hook/noisedeck-codex-identity-hook /usr/local/bin/noisedeck-codex-identity-hook
COPY deploy/provider-hook/noisedeck-hook-diagnostics /usr/local/bin/noisedeck-hook-diagnostics
COPY deploy/provider-hook/noisedeck-bootstrap /usr/local/bin/noisedeck-bootstrap
COPY deploy/provider-hook/noisedeck-auth-mode-healthcheck /usr/local/bin/noisedeck-auth-mode-healthcheck
COPY deploy/provider-hook/noisedeck-deployment-host-guard /usr/local/bin/noisedeck-deployment-host-guard
COPY deploy/mempalace/configure.py /usr/local/lib/noisedeck/configure-mempalace.py
COPY deploy/skills/install_skills.py /usr/local/lib/noisedeck/install_skills.py
COPY deploy/skills/ /opt/noisedeck/skills/
COPY deploy/mempalace/probe_a2a_receive.py deploy/mempalace/probe_a2a_claude.py deploy/mempalace/probe_a2a_kimi.py deploy/mempalace/probe_a2a_codex.py deploy/mempalace/probe_a2a_opencode_plugin.py /usr/local/lib/noisedeck/a2a-probe/
COPY deploy/mempalace/hooks/ /usr/local/lib/noisedeck/a2a-probe/hooks/
RUN chmod 0555 /usr/local/bin/noisedeck-provision /usr/local/bin/noisedeck-provider-hook /usr/local/bin/noisedeck-codex-identity-hook \
    /usr/local/bin/noisedeck-hook-diagnostics \
    /usr/local/bin/noisedeck-bootstrap /usr/local/bin/noisedeck-auth-mode-healthcheck /usr/local/bin/noisedeck-deployment-host-guard \
    /usr/local/lib/noisedeck/configure-mempalace.py /usr/local/lib/noisedeck/install_skills.py \
    && rm -f /opt/noisedeck/skills/*.py

ARG NOISEDECK_RUNTIME_UID=1026
ARG NOISEDECK_RUNTIME_GID=100
RUN case "${NOISEDECK_RUNTIME_UID}" in ''|*[!0-9]*) echo "NOISEDECK_RUNTIME_UID must be a positive numeric UID" >&2; exit 1;; esac \
    && case "${NOISEDECK_RUNTIME_GID}" in ''|*[!0-9]*) echo "NOISEDECK_RUNTIME_GID must be a positive numeric GID" >&2; exit 1;; esac \
    && if [ "${NOISEDECK_RUNTIME_UID}" -eq 0 ] || [ "${NOISEDECK_RUNTIME_GID}" -eq 0 ]; then \
      echo "Deck refuses a root runtime UID or GID" >&2; exit 1; \
    fi \
    && if ! getent group "${NOISEDECK_RUNTIME_GID}" >/dev/null; then \
      groupadd --gid "${NOISEDECK_RUNTIME_GID}" noisedeck-agent; \
    fi \
    && existing_user="$(getent passwd "${NOISEDECK_RUNTIME_UID}" | cut -d: -f1)" \
    && if [ -z "${existing_user}" ]; then \
      useradd --uid "${NOISEDECK_RUNTIME_UID}" --gid "${NOISEDECK_RUNTIME_GID}" \
        --home-dir /home/agent --create-home --shell /bin/bash agent; \
    elif [ "${existing_user}" = node ]; then \
      usermod --login agent --home /home/agent --gid "${NOISEDECK_RUNTIME_GID}" --shell /bin/bash node; \
    else \
      echo "NOISEDECK_RUNTIME_UID ${NOISEDECK_RUNTIME_UID} is already owned by ${existing_user}" >&2; exit 1; \
    fi \
    && install -d -m 0700 -o "${NOISEDECK_RUNTIME_UID}" -g "${NOISEDECK_RUNTIME_GID}" /home/agent \
    && chown -R "${NOISEDECK_RUNTIME_UID}:${NOISEDECK_RUNTIME_GID}" /home/agent \
    && install -d -m 0700 -o "${NOISEDECK_RUNTIME_UID}" -g "${NOISEDECK_RUNTIME_GID}" /home/agent/.config \
    && chmod 0700 /home/agent \
    && install -d -m 0700 -o "${NOISEDECK_RUNTIME_UID}" -g "${NOISEDECK_RUNTIME_GID}" /data /run/noisedeck

ENV HOME=/home/agent

# Sessions drive the browser overlay's sidecar with this client. It comes
# from the lockfile install the sidecar runs, so the versions always match.
RUN --mount=type=bind,from=web-build,source=/src/web/node_modules,target=/run/web-node-modules \
    client="$(readlink -f /run/web-node-modules/playwright)" \
    && install -d -m 0755 /opt/noise-browser/node_modules \
    && cp -RL "${client}" "${client}/../playwright-core" /opt/noise-browser/node_modules/ \
    && node -e "require('/opt/noise-browser/node_modules/playwright')"

COPY --from=go-build --chown=root:root /out/noise-deck /usr/local/libexec/noise/noise-deck
# Keep the pre-canonical executable as an invocation-only compatibility adapter.
RUN chmod 0555 /usr/local/libexec/noise/noise-deck \
    && install -d -m 0555 /usr/local/libexec/noisedeck-login \
    && ln -s ../noise/noise-deck /usr/local/libexec/noisedeck-login/xdg-open \
    && ln -s ../libexec/noise/noise-deck /usr/local/bin/noise-deck \
    && ln -s noise-deck /usr/local/bin/noisedeck \
    && test "$(noise plugins)" = "$(printf 'deck\nsdlc\ntasks\nvisual-diff')" \
    && noise deck version \
    && test -n "$(noise deck --help)" \
    && test -n "$(noise deck help)" \
    && test -n "$(noise deck -h)" \
    && install -d -m 0700 -o "${NOISEDECK_RUNTIME_UID}" -g "${NOISEDECK_RUNTIME_GID}" /home/agent/.cache

EXPOSE 8837
HEALTHCHECK --interval=15s --timeout=5s --start-period=10s --retries=5 \
  CMD ["/usr/local/bin/noisedeck-auth-mode-healthcheck"]

USER ${NOISEDECK_RUNTIME_UID}:${NOISEDECK_RUNTIME_GID}
ENTRYPOINT ["/usr/bin/tini", "--"]
CMD ["/usr/local/bin/noisedeck-bootstrap"]

# The slim distribution (deck:slim): the core plus exactly two agent
# harnesses, Claude Code and Codex. Deck's own content is already in place
# above, so this stage installs as root and drops back to the runtime
# identity at its end — the same pattern every overlay target uses. HOME
# follows the same discipline: these installs and their probes write
# root-owned state (npm's cache, Claude's first-run tree) that must not land
# in the runtime user's home.
FROM core AS slim

ARG TARGETARCH
ARG NOISEDECK_RUNTIME_UID=1026
ARG NOISEDECK_RUNTIME_GID=100
ARG CLAUDE_CODE_VERSION=2.1.310
ARG CLAUDE_BINARY_SHA256_AMD64=b73676430af12f97316a798b8371f81d640f90a58bdcb8f3ce56a835bb47df56
ARG CLAUDE_BINARY_SHA256_ARM64=9c64d9a7442436b65fcd7cc02c1aa0051bb5a7041e7788576ed3b5f29c447d54
# These are the exact artifacts shipped by the image. Each descriptor's
# comments record the last behavioral measurement for that provider, which is
# not always the version pinned here.
ARG CODEX_VERSION=0.162.1

USER root
ENV HOME=/root

RUN case "${TARGETARCH}" in \
      amd64) claude_platform=linux-x64; claude_sha256="${CLAUDE_BINARY_SHA256_AMD64}" ;; \
      arm64) claude_platform=linux-arm64; claude_sha256="${CLAUDE_BINARY_SHA256_ARM64}" ;; \
      *) echo "unsupported target architecture: ${TARGETARCH}" >&2; exit 1 ;; \
    esac \
    && curl -fsSL \
      "https://downloads.claude.ai/claude-code-releases/${CLAUDE_CODE_VERSION}/${claude_platform}/claude" \
      -o /usr/local/bin/claude \
    && echo "${claude_sha256}  /usr/local/bin/claude" | sha256sum -c - \
    && chmod 0555 /usr/local/bin/claude \
    && test "$(claude --version | awk '{print $1}')" = "${CLAUDE_CODE_VERSION}" \
    && rm -rf /root/.claude /root/.claude.json

# Codex, pinned to the version the launch gate enforces. Codex is
# one of the two harnesses the slim distribution carries, so it installs here
# rather than with the harness set the full image adds.
#
# Provider packages ship payloads this image does not use: "-baseline"
# fallbacks for x86-64 CPUs without AVX2, and foreign-platform helper
# directories. On x86-64 the providers installed here therefore require an
# AVX2-class host, and each install layer re-probes its CLIs after stripping,
# so a payload that was actually needed fails the build.
RUN --mount=type=cache,target=/root/.npm,sharing=locked \
    npm install --global \
      "@openai/codex@${CODEX_VERSION}" \
    && find /usr/local/lib/node_modules \
         \( -type d -name "*-baseline" -o -type f -name "*-baseline.*" \) \
         -exec rm -rf {} + \
    && if test "${TARGETARCH}" = amd64; then \
         find /usr/local/lib/node_modules \
           -type d \( -name "*darwin*" -o -name "*win32*" -o -name "*windows*" -o -name "*arm64*" \) \
           -prune -exec rm -rf {} +; \
       else \
         find /usr/local/lib/node_modules \
           -type d \( -name "*darwin*" -o -name "*win32*" -o -name "*windows*" -o -name "*x64*" -o -name "*amd64*" \) \
           -prune -exec rm -rf {} +; \
       fi \
    && test "$(codex --version | awk '{print $2}')" = "${CODEX_VERSION}"

# Restore the runtime identity and home the core established.
ENV HOME=/home/agent
USER ${NOISEDECK_RUNTIME_UID}:${NOISEDECK_RUNTIME_GID}

# The full distribution (deck:latest): every remaining supported agent
# harness, added on top of the slim image, plus native-extension support.
# Deck's own content is already in place above, so these installs run as root
# and drop back to the runtime identity at the end — the same pattern the
# slim and MemPalace stages use. HOME follows the same discipline: the
# installs and their probes write root-owned state (npm's cache, each
# harness's scratch first-run tree) and must not leave it under the runtime
# user's home.
FROM slim AS runtime

ARG TARGETARCH
ARG NOISEDECK_RUNTIME_UID=1026
ARG NOISEDECK_RUNTIME_GID=100
# Shipped artifacts, same contract as the slim stage's descriptors above.
ARG KIMI_VERSION
ARG OPENCODE_VERSION
ARG COMMAND_CODE_VERSION
ARG CLINE_VERSION
ARG KILO_VERSION
ARG QWEN_VERSION
ARG MIMO_VERSION=0.1.15
ARG QODER_VERSION=1.1.78
ARG CURSOR_AGENT_VERSION=2026.10.22-27dcf5f
ARG CURSOR_AGENT_SHA256_AMD64=85296944e4600a83c4b764f578d1502bf9c416db200d33a593e8f3ecabfdc294
ARG CURSOR_AGENT_SHA256_ARM64=22a36df9720830668ccd4d707e5190204ab9c8a0efb2f068cc914895f804d3ec
ARG GROK_VERSION=1.0.70
ARG BUN_VERSION=1.3.14
ARG OMP_VERSION=18.12.6
ARG CRUSH_VERSION
ARG PI_VERSION=1.4.0
ARG DEVIN_VERSION=3000.11.205
ARG DEVIN_SHA256_AMD64=2e65c8b93682258fc91f5e4cead92e3698338cebf1b31dbc32a3201cb63705c4
ARG DEVIN_SHA256_ARM64=a9b10d318319439446eb5204b0d147cac01cb0a639c77f8740de73206a3181a5

USER root
ENV HOME=/root

# The full image is the development workbench as well as the console
# runtime, so it carries a build toolchain for native Python and Node
# extensions. This is build-essential rather than the gcc and libc6-dev the
# image used to carry, because those two alone never delivered the
# capability their comment claimed: node-gyp and pip source builds need make
# and a C++ compiler, and neither was ever installed. build-essential
# subsumes gcc and libc6-dev, so nothing is lost by replacing them.
#
# The core and slim images carry none of it. cgo — the reason gcc arrived
# originally — needs a Go toolchain no runtime image ships.
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
      build-essential \
    && rm -rf /var/lib/apt/lists/* \
    && command -v gcc \
    && command -v g++ \
    && command -v make

COPY --from=npm-harnesses /opt/npm-harnesses/ /usr/local/
RUN test "$(kimi --version | awk '{print $1}')" = "${KIMI_VERSION}" \
    && test "$(opencode --version)" = "${OPENCODE_VERSION}" \
    && test "$(command-code --no-auto-update --version)" = "${COMMAND_CODE_VERSION}" \
    && test "$(cline --version)" = "${CLINE_VERSION}" \
    && test "$(kilo --version)" = "${KILO_VERSION}" \
    && test "$(qwen --version)" = "${QWEN_VERSION}" \
    && test "$(crush --version)" = "crush version v${CRUSH_VERSION}"

# MiMo's launcher selects its platform binary at runtime, including the x64
# baseline fallback on CPUs without AVX2. Install it after the general provider
# pruning above so the matching baseline artifact remains available. The
# version and help checks run against a scratch home that is deleted
# afterwards: MiMo writes its config/data/state trees — and --help runs its
# one-time database migration — even for these commands, and that first-run
# state must not bake into the image layer.
RUN --mount=type=cache,target=/root/.npm,sharing=locked \
    npm install --global "@mimo-ai/cli@${MIMO_VERSION}" \
    && check=$(mktemp -d) \
    && export HOME="$check" \
    && test "$(mimo --version)" = "${MIMO_VERSION}" \
    && mimo --help >/tmp/mimo-help 2>&1 \
    && grep -q -- '--trust' /tmp/mimo-help \
    && grep -q -- '--yolo' /tmp/mimo-help \
    && rm -rf "$check" /tmp/mimo-help

# Qoder's postinstall and even its --version/--help write your own
# state: the npm install runs `configure-path` into ~/.qoder/entry, and every
# invocation appends to the ~/.qoder log tree and the global git ignore at
# ~/.config/git/ignore. The install and probes therefore run against a
# scratch home (with npm's cache pinned back onto the mounted cache) that is
# deleted in the same layer, so no first-run state bakes into the image.
RUN --mount=type=cache,target=/root/.npm,sharing=locked \
    check=$(mktemp -d) \
    && export HOME="$check" \
    && npm_config_cache=/root/.npm npm install --global "@qoder-ai/qodercli@${QODER_VERSION}" \
    && test "$(qoder --version)" = "${QODER_VERSION}" \
    && qoder --help >/tmp/qoder-help \
    && grep -q -- '--permission-mode' /tmp/qoder-help \
    && grep -q -- '--dangerously-skip-permissions' /tmp/qoder-help \
    && grep -q -- '--resume' /tmp/qoder-help \
    && grep -q -- '--session-id' /tmp/qoder-help \
    && rm -rf "$check" /tmp/qoder-help

# Cursor Agent ships as Cursor's official agent-cli-package tarball, pinned
# by version and per-arch sha256 like Devin's distribution. The package
# carries its own node interpreter, bundle, rg, and sandbox helpers; the
# launcher execs the vendored node, so no system node is required. Even
# --version and --help write ~/.cursor/cli-config.json and fill the Node
# compile cache under the cache home, so the probes run against a scratch
# home deleted in the same layer.
RUN case "${TARGETARCH}" in \
      amd64) cursor_arch=x64; cursor_sha256="${CURSOR_AGENT_SHA256_AMD64}" ;; \
      arm64) cursor_arch=arm64; cursor_sha256="${CURSOR_AGENT_SHA256_ARM64}" ;; \
      *) echo "unsupported target architecture: ${TARGETARCH}" >&2; exit 1 ;; \
    esac \
    && curl -fsSL \
      "https://downloads.cursor.com/lab/${CURSOR_AGENT_VERSION}/linux/${cursor_arch}/agent-cli-package.tar.gz" \
      -o /tmp/cursor-agent.tar.gz \
    && echo "${cursor_sha256}  /tmp/cursor-agent.tar.gz" | sha256sum -c - \
    && install -d -m 0555 "/opt/cursor-agent/${CURSOR_AGENT_VERSION}" \
    && tar --no-same-owner -xzf /tmp/cursor-agent.tar.gz \
      -C "/opt/cursor-agent/${CURSOR_AGENT_VERSION}" \
    && find "/opt/cursor-agent/${CURSOR_AGENT_VERSION}" -type d -exec chmod 0555 {} + \
    && find "/opt/cursor-agent/${CURSOR_AGENT_VERSION}" -type f -exec chmod 0444 {} + \
    && chmod 0555 "/opt/cursor-agent/${CURSOR_AGENT_VERSION}/dist-package/cursor-agent" \
      "/opt/cursor-agent/${CURSOR_AGENT_VERSION}/dist-package/node" \
      "/opt/cursor-agent/${CURSOR_AGENT_VERSION}/dist-package/rg" \
      "/opt/cursor-agent/${CURSOR_AGENT_VERSION}/dist-package/cursorsandbox" \
      "/opt/cursor-agent/${CURSOR_AGENT_VERSION}/dist-package/crepectl" \
    && ln -s "/opt/cursor-agent/${CURSOR_AGENT_VERSION}/dist-package/cursor-agent" /usr/local/bin/cursor-agent \
    && check=$(mktemp -d) \
    && export HOME="$check" \
    && test "$(cursor-agent --version)" = "${CURSOR_AGENT_VERSION}" \
    && cursor-agent --help >/tmp/cursor-agent-help \
    && grep -q -- '--trust' /tmp/cursor-agent-help \
    && grep -q -- '--yolo' /tmp/cursor-agent-help \
    && grep -q -- '--resume' /tmp/cursor-agent-help \
    && rm -rf "$check" /tmp/cursor-agent-help /tmp/cursor-agent.tar.gz

# Grok Build ships as the official @xai-official/grok npm trampoline plus a
# per-platform binary package (brotli-compressed) that npm selects for the
# build arch. The postinstall AND every invocation bootstrap the binary and
# state trees into ~/.grok, so the install and probes run against a scratch
# home (npm's cache pinned back onto the mounted cache) that is deleted in
# the same layer. The runtime bootstraps ~/.grok again on your first
# launch; the probe below proves the shipped trampoline resolves and runs
# the pinned platform binary.
RUN --mount=type=cache,target=/root/.npm,sharing=locked \
    check=$(mktemp -d) \
    && export HOME="$check" \
    && npm_config_cache=/root/.npm npm install --global "@xai-official/grok@${GROK_VERSION}" \
    && find /usr/local/lib/node_modules/@xai-official \
         \( -type d -name "*darwin*" -o -type d -name "*win32*" -o -type d -name "*windows*" \) \
         -prune -exec rm -rf {} + \
    && test "$(grok --version | awk '{print $2}')" = "${GROK_VERSION}" \
    && grok --help >/tmp/grok-help \
    && grep -q -- '--always-approve' /tmp/grok-help \
    && grep -q -- '--resume' /tmp/grok-help \
    && grep -q -- '--session-id' /tmp/grok-help \
    && rm -rf "$check" /tmp/grok-help

# omp is a bun-based retrieved-identity provider. Both the runtime and agent
# are pinned; the launch gate checks the carried-forward command surface while
# the behavioral record remains tied to its separately recorded version.
RUN export BUN_INSTALL=/usr/local \
    && curl -fsSL https://bun.sh/install | bash -s "bun-v${BUN_VERSION}" \
    && test "$(bun --version)" = "${BUN_VERSION}" \
    && bun install --global "@oh-my-pi/pi-coding-agent@${OMP_VERSION}" \
    && find /usr/local/install/global/node_modules \
         \( -type d -name "*-baseline" -o -type f -name "*-baseline.*" \
            -o -type d -name "*darwin*" -o -type d -name "*win32*" -o -type d -name "*windows*" \) \
         -exec rm -rf {} + \
    && omp --version | grep -q "${OMP_VERSION}" \
    # BUN_INSTALL relocates the cache to /usr/local/install/cache; the
    # legacy .bun path stays cleaned in case BUN_INSTALL changes again.
    && rm -rf /usr/local/install/cache /usr/local/.bun/install/cache

# Pi is a user-invoked subprocess harness, not a Deck session provider.
# Disable lifecycle scripts so only the pinned package artifacts ship.
# Pi permits compatible pi-ai releases; requiring identical versions would
# break unchanged builds whenever that dependency publishes an update.
ENV PI_SKIP_VERSION_CHECK=1 \
    PI_TELEMETRY=0
RUN npm install --global --ignore-scripts \
      "@earendil-works/pi-coding-agent@${PI_VERSION}" \
    && test "$(pi --version)" = "${PI_VERSION}" \
    && node -e 'const ai = require("/usr/local/lib/node_modules/@earendil-works/pi-coding-agent/node_modules/@earendil-works/pi-ai/package.json"); if (ai.name !== "@earendil-works/pi-ai") process.exit(1); console.log("Pi dependency:", ai.name, ai.version)' \
    && PI_OFFLINE=1 ZAI_API_KEY=build-probe pi --list-models 'zai/glm-5.2' \
      | awk '$1 == "zai" && $2 == "glm-5.2" { found=1 } END { exit !found }' \
    && pi --help >/tmp/pi-help \
    && for flag in --provider --model --print --no-session --no-tools \
      --no-extensions --no-skills --no-prompt-templates --no-context-files; do \
      grep -Eq -- "(^|[[:space:]])${flag}([[:space:],]|$)" /tmp/pi-help || exit 1; \
    done \
    && rm -f /tmp/pi-help \
    && rm -rf /root/.npm

# npm's later global Pi transaction can remove Cline's bin link — and, less
# often, the whole package — while the transaction itself reports success.
# Reconcile the final runtime state: reinstall the pin when the package is
# gone, relink, and re-probe.
RUN node -e "require('/usr/local/lib/node_modules/cline/package.json')" 2>/dev/null \
    || npm install --global "cline@${CLINE_VERSION}" \
    && test "$(node -p "require('/usr/local/lib/node_modules/cline/package.json').version")" = "${CLINE_VERSION}" \
    && ln -sf ../lib/node_modules/cline/bin/cline /usr/local/bin/cline \
    && test "$(cline --version)" = "${CLINE_VERSION}" \
    && cline --help >/tmp/cline-final-help \
    && grep -q -- '--tui' /tmp/cline-final-help \
    && rm -f /tmp/cline-final-help

# Devin's exact identity and resume contract has been measured and is admitted through
# its documented SessionStart hook. Authentication remains user-owned.
RUN case "${TARGETARCH}" in \
      amd64) devin_arch=x86_64; devin_sha256="${DEVIN_SHA256_AMD64}" ;; \
      arm64) devin_arch=aarch64; devin_sha256="${DEVIN_SHA256_ARM64}" ;; \
      *) echo "unsupported target architecture: ${TARGETARCH}" >&2; exit 1 ;; \
    esac \
    && curl -fsSL \
      "https://static.devin.ai/cli/${DEVIN_VERSION}/devin-${DEVIN_VERSION}-${devin_arch}-unknown-linux.tar.gz" \
      -o /tmp/devin.tar.gz \
    && echo "${devin_sha256}  /tmp/devin.tar.gz" | sha256sum -c - \
    && install -d -m 0555 "/opt/devin/${DEVIN_VERSION}" \
    && tar --no-same-owner -xzf /tmp/devin.tar.gz -C "/opt/devin/${DEVIN_VERSION}" \
    && find "/opt/devin/${DEVIN_VERSION}" -type d -exec chmod 0555 {} + \
    && find "/opt/devin/${DEVIN_VERSION}" -type f -exec chmod 0444 {} + \
    && chmod 0555 "/opt/devin/${DEVIN_VERSION}/bin/devin" \
    && ln -s "/opt/devin/${DEVIN_VERSION}/bin/devin" /usr/local/bin/devin \
    && test "$(devin --version | awk '{print $2}')" = "${DEVIN_VERSION}" \
    && devin --help >/tmp/devin-help \
    && for flag in --resume --model --print --permission-mode \
      --respect-workspace-trust; do \
      grep -Eq -- "(^|[[:space:]])${flag}([[:space:],]|$)" /tmp/devin-help || exit 1; \
    done \
    && rm -f /tmp/devin.tar.gz /tmp/devin-help

# The a2a client is runtime tooling, not a MemPalace extra: the sealed
# bootstrap runs `a2a courier`, and the batch workflow's claude receive probe
# loads it from the runtime image it builds. The mempalace stage inherits it.
COPY deploy/a2a/a2a.py /usr/local/bin/a2a
RUN chmod 0555 /usr/local/bin/a2a

# Restore the runtime identity and home the slim image established, so the
# full image's final configuration is the slim image's: same user, same HOME,
# same entrypoint, same healthcheck.
ENV HOME=/home/agent
USER ${NOISEDECK_RUNTIME_UID}:${NOISEDECK_RUNTIME_GID}

# Minimal substrate for Tasks-owned provider workloads. This target does not
# inherit the Deck server, Tasks engine, application healthcheck, or build
# toolchain. Provider artifacts are added by a separately pinned profile image;
# the trusted materializer supplies its exact argv and environment.
FROM node:26.12.1-bookworm-slim@sha256:28147155d47dc31abe0a46e0d12b59b2fb3d0a2dcc5dc65fce76a074180b16fe AS task-workload-substrate

ARG NOISEDECK_TASK_UID=1026
ARG NOISEDECK_TASK_GID=100
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates git openssh-client tini \
    && rm -rf /var/lib/apt/lists/* \
    && existing_group="$(getent group "${NOISEDECK_TASK_GID}" | cut -d: -f1)" \
    && if [ -z "${existing_group}" ]; then groupadd --gid "${NOISEDECK_TASK_GID}" task-agent; else groupmod --new-name task-agent "${existing_group}"; fi \
    && existing_user="$(getent passwd "${NOISEDECK_TASK_UID}" | cut -d: -f1)" \
    && if [ -z "${existing_user}" ]; then useradd --uid "${NOISEDECK_TASK_UID}" --gid "${NOISEDECK_TASK_GID}" --home-dir /home/agent --no-create-home --shell /bin/bash task-agent; else usermod --login task-agent --home /home/agent --gid "${NOISEDECK_TASK_GID}" --shell /bin/bash "${existing_user}"; fi \
    && install -d -m 0700 -o "${NOISEDECK_TASK_UID}" -g "${NOISEDECK_TASK_GID}" /home/agent /run/noisedeck \
    && install -d -m 1777 /tmp

# The G3 root signed the compromised eMudhra intermediates.
RUN sed -i '\|^mozilla/emSign_ECC_Root_CA_-_G3.crt$|s|^|!|' /etc/ca-certificates.conf \
    && update-ca-certificates --fresh \
    && test ! -e /etc/ssl/certs/emSign_ECC_Root_CA_-_G3.pem

COPY deploy/assert-tls-roots.mjs /tmp/assert-tls-roots.mjs
RUN node /tmp/assert-tls-roots.mjs && rm /tmp/assert-tls-roots.mjs

ENV HOME=/home/agent
HEALTHCHECK NONE
STOPSIGNAL SIGTERM
COPY --from=task-workload-latch-build --chmod=0555 /out/task-workload-latch /usr/local/libexec/noisedeck-task-workload-latch
USER ${NOISEDECK_TASK_UID}:${NOISEDECK_TASK_GID}
ENTRYPOINT ["/usr/bin/tini", "-g", "--", "/usr/local/libexec/noisedeck-task-workload-latch"]
CMD []

# MemPalace extends the full runtime. It is the one optional capability that
# is still built here rather than as a derived image, because the production
# profile requires it and verifies it live. Language toolchains are not: a
# project that needs Go or Rust builds a derived image, documented in
# docs/image-variants.md.
FROM runtime AS mempalace
ARG NOISEDECK_RUNTIME_UID=1026
ARG NOISEDECK_RUNTIME_GID=100
ARG UV_VERSION=0.13.5
ARG MEMPALACE_VERSION=3.10.0
# MemPalace PR 2631 (rowid-ordered page scans with early termination and
# compact tool replies) until it lands in a release: the logstream read
# hot path is 3x faster at every page size. The version below is what the
# pinned commit still reports, and the readiness checks assert it.
ARG MEMPALACE_COMMIT=bb01fd8c00bf5890111b67fd7783fddb48c5dcbe
COPY deploy/mempalace/security-uv.lock /tmp/security-uv.lock
USER root
RUN apt-get update \
    && apt-get install -y --no-install-recommends python3 python3-venv python3-nacl \
    && rm -rf /var/lib/apt/lists/* \
    && python3 -m venv /tmp/mempalace-uv \
    && /tmp/mempalace-uv/bin/pip install --disable-pip-version-check --no-input "uv==${UV_VERSION}" \
    && git init /tmp/mempalace-src \
    && git -C /tmp/mempalace-src remote add origin https://github.com/mempalace/mempalace.git \
    && git -C /tmp/mempalace-src fetch --depth 1 origin refs/pull/2631/head \
    && git -C /tmp/mempalace-src checkout --detach FETCH_HEAD \
    && test "$(git -C /tmp/mempalace-src rev-parse HEAD)" = "${MEMPALACE_COMMIT}" \
    && cp /tmp/security-uv.lock /tmp/mempalace-src/uv.lock \
    && UV_PROJECT_ENVIRONMENT=/opt/mempalace/venv \
      /tmp/mempalace-uv/bin/uv sync --project /tmp/mempalace-src --python /usr/bin/python3 --locked --no-dev --no-editable \
    && install -d /opt/mempalace/skills /opt/mempalace/hooks \
    && ln -s /opt/mempalace/venv/bin/mempalace /usr/local/bin/mempalace \
    && ln -s /opt/mempalace/venv/bin/mempalace-mcp /usr/local/bin/mempalace-mcp \
    && rm -rf /tmp/security-uv.lock /tmp/mempalace-uv /tmp/mempalace-src /root/.cache/uv /home/agent/.cache \
    && install -d -m 0700 -o "${NOISEDECK_RUNTIME_UID}" -g "${NOISEDECK_RUNTIME_GID}" /home/agent/.cache \
    && test "$(mempalace --version)" = "MemPalace ${MEMPALACE_VERSION}"
# Python clients use the same patched trust store as system TLS clients.
RUN ca_bundle="$(/opt/mempalace/venv/bin/python -c 'import certifi; print(certifi.where())')" \
    && ln -sf /etc/ssl/certs/ca-certificates.crt "${ca_bundle}" \
    && /opt/mempalace/venv/bin/python -c "import certifi, ssl; assert not any(value == 'emSign ECC Root CA - G3' for cert in ssl.create_default_context(cafile=certifi.where()).get_ca_certs() for rdn in cert['subject'] for key, value in rdn)"

COPY deploy/mempalace/skills/ /opt/mempalace/skills/
COPY deploy/mempalace/hook_forwarder.py /usr/local/lib/noisedeck/mempalace-hook-forwarder.py
COPY deploy/mempalace/hooks/ /opt/mempalace/hooks/
COPY scripts/task-hygiene.sh /usr/local/bin/noisedeck-task-hygiene
COPY scripts/model-offer/model-offer.sh /usr/local/bin/noisedeck-model-offer
COPY deploy/mempalace/noisedeck-bootstrap /usr/local/bin/noisedeck-bootstrap
RUN chmod 0555 /usr/local/bin/noisedeck-bootstrap /usr/local/bin/noisedeck-task-hygiene \
    /usr/local/bin/noisedeck-model-offer \
    /usr/local/lib/noisedeck/configure-mempalace.py \
    /usr/local/lib/noisedeck/mempalace-hook-forwarder.py /opt/mempalace/hooks/*.sh \
    && chmod 0444 /opt/mempalace/hooks/*.js
USER ${NOISEDECK_RUNTIME_UID}:${NOISEDECK_RUNTIME_GID}
