BIN      := noise-deck
CMD      := ./cmd/noisedeck
BUILDDIR := build
WEB_ASSET_DIR := internal/server/webassets/dist
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  := -X main.version=$(VERSION)

# Pinned lint toolchain. TOOLS_BIN defaults to a location outside this repo and
# outside /tmp, which this container wipes on redeploy. Override to relocate.
TOOLS_BIN            ?= /workspace/tools/bin
GOLANGCI_LINT_VERSION ?= v2.6.2
GOLANGCI_LINT        ?= $(TOOLS_BIN)/golangci-lint

# The task engine package the tasks integration executes. NOISE_TASKS_SRC is
# explicit because the Tasks repository may be checked out under any name.
# NOISETASK_SRC remains a migration-plan-gated compatibility input for automation.
NOISE_TASKS_SRC          ?= $(if $(NOISETASK_SRC),$(NOISETASK_SRC),/workspace/tasks)
override NOISE_TASKS_REF := a03efe15d6bfcd68fa3cc812d612c63306736149
NOISE_TASKS_VERSION      ?= 2.10.1
NOISE_TASKS_PACKAGE_ROOT ?= /srv/noisedeck/noise-tasks-root
NOISETASK_BIN            ?= $(TOOLS_BIN)/noisetask
# The compatibility shim and plugin are built as one installation. The
# selected shim path fixes the adjacent plugin root at link time.
NOISETASK_LIBEXEC_ROOT   := $(abspath $(dir $(NOISETASK_BIN))../libexec/noise)
NOISE_TASKS_MODULE       := github.com/noisefloor-oss/tasks

# SQLite driver needs cgo; keep it explicit so a CGO_ENABLED=0 environment
# fails at build time rather than at first query.
export CGO_ENABLED := 1

.PHONY: all dev build build-frontend build-backend test test-race acceptance harness-bump-check provider-spike-codex provider-spike-devin capacity-live-probe lint lint-install verify-noise-tasks-source noise-tasks-package noisetask-install fmt vet tidy verify clean help

all: build

## dev: run the backend and the hot-reloading UI together at http://127.0.0.1:8837/__dev/
dev:
	scripts/dev.sh

## build: compile the production frontend and embed it in the backend binary
build: build-backend

## build-frontend: compile the deployable UI into the Go embed package
build-frontend:
	cd web && NOISEDECK_EMBED_BUILD=1 pnpm build
	find $(WEB_ASSET_DIR) -depth -mindepth 1 ! -name UNBUILT ! -name .gitignore -delete
	cp -R web/dist/. $(WEB_ASSET_DIR)/

## build-backend: compile the backend with the staged frontend assets
build-backend: build-frontend
	@mkdir -p $(BUILDDIR)
	go build -ldflags "$(LDFLAGS)" -o $(BUILDDIR)/$(BIN) $(CMD)

## test: run the unit test suite
test:
	go test ./...

## test-race: run tests under the race detector
test-race:
	go test -race ./...

## acceptance: run the fail-closed real-Claude vertical-slice release gate
acceptance:
	NOISEDECK_ACCEPTANCE_REQUIRED=1 go test -race ./internal/acceptance -count=1 -timeout 35m

## harness-bump-check: compare every agent harness pin with its upstream
harness-bump-check:
	go run ./cmd/noisedeck-harness-bump check

## provider-spike-codex: run the fail-closed real Codex 0.145.0 contract probe
provider-spike-codex:
	go run ./scripts/provider-spike/conformance/runner

## provider-spike-devin: run the real authenticated Devin contract probe at the contract's version_tested (NOISEDECK_DEVIN_EXPECTED_VERSION=x.y.z names a bump candidate instead)
provider-spike-devin:
	NOISEDECK_REAL_DEVIN=1 go test ./scripts/provider-spike/conformance \
		-run '^TestDevinProviderContract$$' -count=1 -v -timeout=6m

## capacity-live-probe: run the opt-in real-CLI capacity probe (requires logged-in claude and codex)
capacity-live-probe:
	NOISEDECK_LIVE_CAPACITY_PROBE=1 go test ./internal/capacity \
		-run '^TestLiveProbe' -count=1 -v -timeout=6m

## hygiene: report task lifecycle residue in this checkout (HYGIENE_APPLY=1 acts on it)
hygiene:
	./scripts/task-hygiene.sh $(if $(HYGIENE_APPLY),--apply,)

## lint: run golangci-lint (fails if the pinned version is missing)
lint:
	@command -v $(GOLANGCI_LINT) >/dev/null 2>&1 || { \
		echo "golangci-lint not found at '$(GOLANGCI_LINT)'."; \
		echo "Install the pinned version with: make lint-install"; \
		exit 1; \
	}
	@have=$$($(GOLANGCI_LINT) version --short 2>/dev/null | tr -d 'v'); \
	want=$$(echo $(GOLANGCI_LINT_VERSION) | tr -d 'v'); \
	if [ "$$have" != "$$want" ]; then \
		echo "golangci-lint version mismatch: have $$have, want $$want."; \
		echo "Reinstall with: make lint-install"; \
		exit 1; \
	fi
	$(GOLANGCI_LINT) run

## lint-install: install the pinned golangci-lint into TOOLS_BIN
lint-install:
	@mkdir -p $(TOOLS_BIN)
	GOBIN=$(abspath $(TOOLS_BIN)) go install \
		github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

## verify-noise-tasks-source: require the exact clean Tasks release checkout
verify-noise-tasks-source:
	@scripts/verify-noise-tasks-source.sh "$(NOISE_TASKS_SRC)" "$(NOISE_TASKS_REF)"

## noise-tasks-package: stage the pinned package for host install (root-only: Tasks 2.5 installs a setuid worktree broker and demands a root-owned 0700 staging tree)
noise-tasks-package: verify-noise-tasks-source
	install -d -o 0 -g 0 -m 0700 $(NOISE_TASKS_PACKAGE_ROOT)
	cd $(NOISE_TASKS_SRC) && CGO_ENABLED=0 $(MAKE) package \
		DESTDIR=$(abspath $(NOISE_TASKS_PACKAGE_ROOT)) \
		PREFIX=/usr/local VERSION=$(NOISE_TASKS_VERSION)
	@test -x $(NOISE_TASKS_PACKAGE_ROOT)/usr/local/bin/noise
	@test -x $(NOISE_TASKS_PACKAGE_ROOT)/usr/local/bin/noise-tasks
	@test -x $(NOISE_TASKS_PACKAGE_ROOT)/usr/local/bin/noisetask
	@test -x $(NOISE_TASKS_PACKAGE_ROOT)/usr/local/libexec/noise-setup
	@test -x $(NOISE_TASKS_PACKAGE_ROOT)/usr/local/libexec/noise-worktree-broker
	@test -x $(NOISE_TASKS_PACKAGE_ROOT)/usr/local/libexec/noise-broker-mint

## noisetask-install: install the migration-window shim and its fixed-root plugin
noisetask-install: verify-noise-tasks-source
	@mkdir -p "$(dir $(abspath $(NOISETASK_BIN)))" "$(NOISETASK_LIBEXEC_ROOT)"
	cd "$(NOISE_TASKS_SRC)" && CGO_ENABLED=0 go build -trimpath \
		-ldflags "-X $(NOISE_TASKS_MODULE)/internal/buildinfo.Version=$(NOISE_TASKS_VERSION) -X $(NOISE_TASKS_MODULE)/internal/libexec.Root=$(NOISETASK_LIBEXEC_ROOT)" \
		-o "$(NOISETASK_LIBEXEC_ROOT)/noise-tasks" ./cmd/noise-tasks
	cd "$(NOISE_TASKS_SRC)" && CGO_ENABLED=0 go build -trimpath \
		-ldflags "-X $(NOISE_TASKS_MODULE)/internal/buildinfo.Version=$(NOISE_TASKS_VERSION) -X $(NOISE_TASKS_MODULE)/internal/libexec.Root=$(NOISETASK_LIBEXEC_ROOT)" \
		-o "$(abspath $(NOISETASK_BIN))" ./cmd/noisetask
	"$(abspath $(NOISETASK_BIN))" version

## fmt: format all Go source
fmt:
	gofmt -s -w $$(git ls-files '*.go')

## vet: run go vet
vet:
	go vet ./...

## tidy: sync go.mod/go.sum
tidy:
	go mod tidy

## verify: the portable pre-PR gate — formatting, vet, lint, modules, unit tests
verify: vet lint
	@test -z "$$(gofmt -s -l $$(git ls-files '*.go') | tee /dev/stderr)" || { echo "gofmt: files need formatting"; exit 1; }
	go mod verify
	go test ./...

## clean: remove build output
clean:
	rm -rf $(BUILDDIR)
	find $(WEB_ASSET_DIR) -depth -mindepth 1 ! -name UNBUILT ! -name .gitignore -delete

## help: list targets
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/^## /  /'
