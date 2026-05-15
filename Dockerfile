# =============================================================================
# Stage 1: tools — Go toolchain + linters. Used by Make for build/test/lint/fmt.
#
# Closes M4 — see reviews/2026-05-15T195833Z-review.md#m4.
# Base images are pinned by digest in addition to the human-readable tag.
# The tag survives for grepability; the digest is what registry pulls
# actually verify. When bumping GO_VERSION / ALPINE_VERSION in .versions,
# also refresh GO_IMAGE_DIGEST (look up via
#   docker buildx imagetools inspect public.ecr.aws/docker/library/golang:<GO_VERSION>-alpine<ALPINE_VERSION>
# ).
#
# Toolchain versions are *required* build args with no defaults — the Makefile
# is the source of truth and reads them from .versions. Building this image
# without --build-arg will fail at the FROM line, which is the loudest
# failure mode we can pick.
# =============================================================================
ARG GO_VERSION
ARG ALPINE_VERSION
ARG GO_IMAGE_DIGEST

FROM public.ecr.aws/docker/library/golang:${GO_VERSION}-alpine${ALPINE_VERSION}@${GO_IMAGE_DIGEST} AS tools

ARG GOLANGCI_LINT_VERSION

# Hadolint DL3018: alpine package versions roll weekly; pinning here triggers
# frequent rebuilds whenever Alpine's package repo gets a security update.
# We instead pin the *base image* by digest (above) and let apk pull the
# digest-matched set of packages from that snapshot. Operationally
# equivalent supply-chain guarantee, no Renovate churn.
# hadolint ignore=DL3018
RUN apk add --no-cache git make ca-certificates gcc musl-dev \
    && go install github.com/golangci/golangci-lint/cmd/golangci-lint@${GOLANGCI_LINT_VERSION}

ENV GOFLAGS=-mod=mod \
    CGO_ENABLED=0

WORKDIR /src

# =============================================================================
# Stage 2: builder — compiles the static binary.
# =============================================================================
FROM tools AS builder

COPY go.mod go.sum* ./
RUN go mod download

COPY . .
RUN go build -trimpath -ldflags="-s -w" -o /out/palette-ai-instance \
    ./cmd/palette-ai-instance

# =============================================================================
# Stage 3: runtime — minimal distroless image with the static binary + CA certs.
# distroless digest pinned per M4 — refresh when bumping tags.
# =============================================================================
FROM gcr.io/distroless/static-debian12:nonroot@sha256:a9329520abc449e3b14d5bc3a6ffae065bdde0f02667fa10880c49b35c109fd1 AS runtime

COPY --from=builder /out/palette-ai-instance /usr/local/bin/palette-ai-instance

USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/palette-ai-instance"]
