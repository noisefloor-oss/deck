# syntax=docker/dockerfile:1.7@sha256:a57df69d0ea827fb7266491f2813635de6f17269be881f696fbfdf2d83dda33e
# deck:go — a derived image adding the pinned Go toolchain to a Deck image
# that already carries the native compiler needed by cgo projects.
#
#   docker compose -f compose.yaml -f compose.go.yaml \
#     --env-file deploy/compose.env up --build -d
#
# compose.go.yaml builds the full Deck image from this checkout as the
# deck-base context in the same BuildKit graph.
#
# The default is full, not slim: Deck's SQLite-backed packages use cgo, and a
# Go image that cannot run this repository's test suite would not solve the
# failure this variant exists for.
# Copy the toolchain from the same digest-pinned, multi-architecture image
# that builds Deck. A mutable download in the projects mount made `go` depend
# on one host and let any session replace the compiler for every other one.
FROM golang:1.26.11-bookworm@sha256:dc40a13b61304647ed7e5d581d246b86413dfc29d63c6d018310feb93cd98fc0 AS go-toolchain

# deck-base is a named build context. Compose can bind it either to a
# published Deck image or directly to another service build, so a dependent
# capability image never relies on a daemon-local intermediate tag.
FROM deck-base AS runtime

ARG NOISEDECK_RUNTIME_UID=1026
ARG NOISEDECK_RUNTIME_GID=100
ARG GO_VERSION=1.26.11

USER root
COPY --from=go-toolchain --chown=root:root /usr/local/go /usr/local/go
ENV GOTOOLCHAIN=local \
    PATH=/usr/local/go/bin:${PATH}
COPY --chmod=0444 deploy/derived/go-profile.sh /etc/profile.d/noisedeck-go.sh

# Toolchain bytes are shared image content and stay immutable to sessions.
# Builds use the runtime user's normal home for module and build caches.
RUN chown -R root:root /usr/local/go \
    && chmod -R go-w /usr/local/go \
    && test "$(go version | awk '{print $3}')" = "go${GO_VERSION}" \
    && test "$(go version /usr/local/go/bin/gofmt | awk '{print $2}')" = "go${GO_VERSION}" \
    && command -v cc

USER ${NOISEDECK_RUNTIME_UID}:${NOISEDECK_RUNTIME_GID}

# Exercise the effective session state, including Debian's login-shell PATH
# replacement and a real cgo link. Inspecting image instructions alone would
# miss both failures.
RUN set -eu; \
    test "$(command -v go)" = /usr/local/go/bin/go; \
    test "$(/bin/bash -lc 'command -v go')" = /usr/local/go/bin/go; \
    test "$(/bin/bash -lc 'go env GOTOOLCHAIN')" = local; \
    test ! -w /usr/local/go; \
    test ! -w /usr/local/go/bin; \
    test ! -w /usr/local/go/bin/go; \
    probe="$(mktemp -d)"; \
    trap 'rm -rf -- "$probe"' EXIT; \
    printf 'package main\n/* #include <stdlib.h> */\nimport "C"\nfunc main() {}\n' >"$probe/main.go"; \
    (cd "$probe" && GO111MODULE=off CGO_ENABLED=1 go build -o probe ./main.go); \
    test -x "$probe/probe"
