# =============================================================================
# Stage 1: tools — Go toolchain + linters. Used by Make for build/test/lint/fmt.
#
# Toolchain versions are *required* build args with no defaults — the Makefile
# is the source of truth and reads them from .versions. Building this image
# without --build-arg will fail at the FROM line (golang:-alpine doesn't
# exist), which is the loudest failure mode we can pick.
#
# To build standalone, supply all three:
#   docker build \
#     --build-arg GO_VERSION=1.23.4 \
#     --build-arg ALPINE_VERSION=3.20 \
#     --build-arg GOLANGCI_LINT_VERSION=v1.61.0 \
#     --target tools .
# =============================================================================
ARG GO_VERSION
ARG ALPINE_VERSION

FROM golang:${GO_VERSION}-alpine${ALPINE_VERSION} AS tools

ARG GOLANGCI_LINT_VERSION

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
# =============================================================================
FROM gcr.io/distroless/static-debian12:nonroot AS runtime

COPY --from=builder /out/palette-ai-instance /usr/local/bin/palette-ai-instance

USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/palette-ai-instance"]
