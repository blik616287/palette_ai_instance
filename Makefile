# All Go work happens inside the `tools` stage of ./Dockerfile so the
# host needs only docker — no Go, no golangci-lint, no helm CLI.

SHELL := /bin/bash

# Toolchain versions come from .versions (single source of truth shared with
# the Dockerfile). Tune via env var, .env, or make-args — all three work:
#   GO_VERSION=1.24.0 make ci                   (env var)
#   make GO_VERSION=1.24.0 ci                   (make arg)
#   export GO_VERSION=1.24.0 && make ci         (shell export)
# Or edit .versions directly for a persistent project-wide bump.
#
# NOTE: go.mod's `go 1.23` directive is the minimum *language* version this
# code requires — it's a separate axis from the toolchain version above and
# is updated only when the code starts using features from a newer Go.
include .versions
export GO_VERSION ALPINE_VERSION GOLANGCI_LINT_VERSION GO_IMAGE_DIGEST

# Strip the patch component off GO_VERSION (1.23.4 → 1.23) to get the value
# go.mod's `go` directive expects. `sync-versions` writes this into go.mod
# and `check-versions` errors if drift creeps in.
GO_LANG_VERSION := $(shell echo $(GO_VERSION) | awk -F. '{print $$1"."$$2}')

TOOLS_IMAGE       := palette-ai-instance-tools:go$(GO_VERSION)
RUNTIME_IMAGE     := palette-ai-instance:local
COVERAGE_FILE     := coverage.out
COVERAGE_MIN_PCT  := 95.0
BIN_DIR           := bin
BIN_NAME          := palette-ai-instance
PKG               := ./...
# main.go is a thin entrypoint — coverage is measured over internal/* only.
COVER_PKG         := ./internal/...

# Cross-compile knobs — override on the CLI.
TARGET_OS    ?= linux
TARGET_ARCH  ?= amd64

BUILD_ARGS := \
    --build-arg GO_VERSION=$(GO_VERSION) \
    --build-arg ALPINE_VERSION=$(ALPINE_VERSION) \
    --build-arg GOLANGCI_LINT_VERSION=$(GOLANGCI_LINT_VERSION) \
    --build-arg GO_IMAGE_DIGEST=$(GO_IMAGE_DIGEST)

# Run docker as the host user so files written into /src (go.sum, bin/, the
# coverage profile) stay owned by the host user instead of root.
HOST_UID := $(shell id -u)
HOST_GID := $(shell id -g)

# Go's module + build cache live inside the project so they survive between
# docker runs without needing named volumes (which start root-owned). Paths
# are container-relative (/src is the bind mount) so they don't leak the
# host's directory layout into the container env.
GO_CACHE_HOST    := $(CURDIR)/.cache/go-build
GO_MODCACHE_HOST := $(CURDIR)/.cache/go-mod
GO_CACHE_CTR     := /src/.cache/go-build
GO_MODCACHE_CTR  := /src/.cache/go-mod

DOCKER_RUN := docker run --rm \
    --user $(HOST_UID):$(HOST_GID) \
    -e HOME=/tmp \
    -e GOPATH=/src/.cache/gopath \
    -e GOCACHE=$(GO_CACHE_CTR) \
    -e GOMODCACHE=$(GO_MODCACHE_CTR) \
    -v $(CURDIR):/src \
    -w /src \
    $(TOOLS_IMAGE)

.PHONY: help tools-image cache-dirs build test lint fmt vet tidy coverage coverage-check image clean all ci sync-versions check-versions vulncheck

cache-dirs:
	@mkdir -p $(GO_CACHE_HOST) $(GO_MODCACHE_HOST)

help:
	@awk 'BEGIN {FS = ":.*?## "} /^[a-zA-Z_-]+:.*?## / {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

tools-image: cache-dirs ## Build the docker image that holds Go + golangci-lint
	docker build $(BUILD_ARGS) --target tools -t $(TOOLS_IMAGE) .

tidy: tools-image sync-versions ## go mod tidy + keep go.mod's `go` directive in sync with .versions
	$(DOCKER_RUN) go mod tidy

sync-versions: tools-image ## write GO_LANG_VERSION + toolchain into go.mod
	$(DOCKER_RUN) go mod edit -go=$(GO_LANG_VERSION) -toolchain=go$(GO_VERSION)

check-versions: ## fail if go.mod's `go` directive drifts from .versions
	@raw=$$(grep '^go ' go.mod | awk '{print $$2}'); \
	actual=$$(echo "$$raw" | awk -F. '{print $$1"."$$2}'); \
	expected=$(GO_LANG_VERSION); \
	if [ "$$actual" != "$$expected" ]; then \
	  echo "✗ go.mod says 'go $$raw' but .versions implies '$$expected'."; \
	  echo "  Run \`make sync-versions\` (or bump .versions) to align them."; \
	  exit 1; \
	fi; \
	echo "✓ go.mod and .versions agree on Go language version $$expected (go.mod: $$raw)"

fmt: tools-image ## gofmt -s -w against project sources (skips .cache)
	# `go fmt ./...` uses go's package list so it doesn't recurse into
	# .cache/. We invoke gofmt directly so we get the -s (simplify) flag.
	$(DOCKER_RUN) sh -c 'gofmt -s -w cmd internal'

vet: tools-image ## go vet
	$(DOCKER_RUN) go vet $(PKG)

lint: tools-image ## golangci-lint
	$(DOCKER_RUN) golangci-lint run --timeout 5m $(PKG)

test: tools-image ## go test with race detector + coverage profile
	# CGO is on for tests so -race works; the production binary stays static (CGO=0).
	$(DOCKER_RUN) sh -c 'CGO_ENABLED=1 go test -race -covermode=atomic \
	    -coverprofile=$(COVERAGE_FILE) -coverpkg=$(COVER_PKG) $(PKG)'

coverage: test ## show per-function coverage
	$(DOCKER_RUN) go tool cover -func=$(COVERAGE_FILE)

# Closes M1 — see reviews/2026-05-15T195833Z-review.md#m1.
# Vulnerability scan. Pinned to v1.1.4 because @latest now requires Go 1.25.x
# and we want one knob to bump in a single line when we move Go versions.
# Reusable across Go 1.24+ toolchains.
GOVULNCHECK_VERSION ?= v1.1.4

vulncheck: tools-image ## govulncheck against the project — fails on any reachable CVE
	$(DOCKER_RUN) sh -c '\
	  mkdir -p /src/.cache/gotools && \
	  test -x /src/.cache/gotools/govulncheck || GOBIN=/src/.cache/gotools go install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) && \
	  /src/.cache/gotools/govulncheck ./...'

coverage-check: test ## fail if total coverage < $(COVERAGE_MIN_PCT)%
	@$(DOCKER_RUN) sh -c '\
	  total=$$(go tool cover -func=$(COVERAGE_FILE) | awk "/^total:/ {gsub(\"%\",\"\",\$$3); print \$$3}"); \
	  echo "Total coverage: $${total}%"; \
	  awk -v t="$$total" -v m=$(COVERAGE_MIN_PCT) "BEGIN { exit !(t+0 >= m+0) }" \
	    || { echo \"FAIL: coverage $${total}% is below threshold $(COVERAGE_MIN_PCT)%\"; exit 1; }; \
	  echo "OK: coverage $${total}% meets threshold $(COVERAGE_MIN_PCT)%"'

# Closes M5 — see reviews/2026-05-15T195833Z-review.md#m5.
# Version stamping. VERSION defaults to git describe (or "dev"); GIT_SHA and
# BUILD_TIME are computed at make-time. Override on the CLI:
#   make VERSION=v1.2.3 build
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
GIT_SHA   ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_TIME ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

VERSION_LDFLAGS := -X main.version=$(VERSION) -X main.gitSHA=$(GIT_SHA) -X main.buildTime=$(BUILD_TIME)

build: tools-image ## compile a static binary into ./bin/ on the host
	# The Go build runs *inside* the tools container, but `$(BIN_DIR)` is the
	# bind-mounted host directory at /src/$(BIN_DIR), so the resulting binary
	# lands on the host filesystem — not buried in a docker image. CGO is off
	# and pure Go links statically by default, so the binary has no dynamic
	# library deps (verify with `file bin/$(BIN_NAME)` → "statically linked").
	mkdir -p $(BIN_DIR)
	$(DOCKER_RUN) env CGO_ENABLED=0 GOOS=$(TARGET_OS) GOARCH=$(TARGET_ARCH) \
	    go build -trimpath -ldflags="-s -w -extldflags=-static $(VERSION_LDFLAGS)" \
	    -o $(BIN_DIR)/$(BIN_NAME) ./cmd/palette-ai-instance

image: ## build the distroless runtime image
	docker build -t $(RUNTIME_IMAGE) .

ci: check-versions fmt vet lint coverage-check vulncheck build ## everything CI should run

all: ci ## alias for `ci`

clean: ## remove local build artefacts + project-local go caches
	rm -rf $(BIN_DIR) $(COVERAGE_FILE) .cache
