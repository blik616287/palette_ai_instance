# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

A Go CLI (`palette-ai-instance`) that drives a full PaletteAI install — `cert-manager` → optional messaging queue (Bitnami RabbitMQ) → `mural-crds` → `mural` — onto any Kubernetes cluster via the **helm SDK** (not the helm CLI). Two subcommands: `deploy` and `cleanup`. After deploy, an optional `--validate` pass reads the deployed Ingress + Service and prints the public URL.

## Build / test / lint

All Go work runs **inside the `tools` stage of the project Dockerfile** — the host needs only `docker` and `make`. Don't `go build` directly on the host.

```bash
make ci          # check-versions → fmt → vet → lint → coverage-check (≥95%) → build
make build       # static binary → ./bin/palette-ai-instance
make test        # go test with -race + coverage profile (CGO=1 for tests only)
make lint        # golangci-lint
make tidy        # go mod tidy + keep go.mod's `go` directive aligned with .versions
make clean       # remove ./bin, coverage.out, and .cache/
```

Single-test invocation goes through `DOCKER_RUN` too — match the Makefile shape:

```bash
docker run --rm --user $(id -u):$(id -g) -e HOME=/tmp \
  -e GOPATH=/src/.cache/gopath -e GOCACHE=/src/.cache/go-build -e GOMODCACHE=/src/.cache/go-mod \
  -v $(pwd):/src -w /src palette-ai-instance-tools:go1.23.4 \
  go test -run TestName ./internal/deployer/...
```

(Run `make tools-image` first if the image isn't built yet.)

Coverage is measured over `./internal/...` only — `cmd/palette-ai-instance/main.go` is a thin entrypoint and is excluded from the 95% threshold.

## Toolchain version source of truth

`.versions` is the **single source of truth** for `GO_VERSION`, `ALPINE_VERSION`, `GOLANGCI_LINT_VERSION`. The Makefile reads it via `include .versions`; the Dockerfile receives them as `--build-arg`s (it has no defaults — building without them fails loudly at the `FROM` line, by design).

`go.mod`'s `go 1.23` directive is a **separate axis** — it's the minimum language version, not the toolchain version. `make check-versions` errors if they drift; `make sync-versions` or `make tidy` realigns them.

## End-to-end smoke test (minikube)

```bash
make build
( cd examples/local_k8s && ansible-playbook up.yml )         # writes examples/local_k8s/artifacts/kubeconfig
./bin/palette-ai-instance deploy  --config examples/cluster_config.yaml
./bin/palette-ai-instance cleanup --config examples/cluster_config.yaml --level full
( cd examples/local_k8s && ansible-playbook down.yml )
```

Host prereqs for the playbooks: `kubectl`, `helm`, `minikube`, `ansible`, `python3` with `bcrypt`. The kubeconfig at `examples/local_k8s/artifacts/kubeconfig` is gitignored.

## Architecture

```
cmd/palette-ai-instance/main.go      thin entrypoint
internal/cli/                        cobra commands + flag-vs-config merge
internal/config/                     cluster_config.yaml loader
internal/deployer/                   orchestrates the helm install sequence
internal/cleaner/                    orchestrates teardown
internal/helm/                       helm SDK wrapper behind Installer / Uninstaller interfaces
internal/kube/                       client-go wrapper for --validate + cleaner
internal/postrender/                 fix-nil-values post-renderer
```

Things to understand before changing code:

**Flag-vs-config precedence (`deploy` only).** `internal/cli/deploy.go:mergeDeployRequest` layers values: cobra default → per-cluster `deploy:` block from `cluster_config.yaml` → CLI value *but only if the user explicitly set the flag* (`cmd.Flags().Changed(name)`). The `pickStr/pickBool/pickDuration/pickStrSlice` helpers all encode this rule — keep them consistent if you add a new flag.

**`cleanup` is intentionally CLI-only.** There is no `cleanup:` block in `cluster_config.yaml` — destructive operations must be re-typed on every invocation so they can't sneak in via config drift. Don't add per-cluster cleanup defaults.

**Install order is fixed in `deployer.Deploy`:** cert-manager → queue → mural-crds → mural. Each step is skipped if its chart URI is empty. The mural chart is installed with `Wait: false` deliberately — combining helm's wait phase with the post-renderer trips a race where helm reports "no Ingress with the name X found" even though the resource is present. Pod-readiness waiting happens out-of-band in `validate()`.

**The `fix-nil-values` post-renderer** (`internal/postrender/fixnil.go`) patches the rendered mural manifests in flight to work around two known chart bugs: (1) empty `stringData` fields render as YAML nil (rejected by the API server) — re-quoted as `""`; (2) the zot StatefulSet omits the required `serviceName` field — injected before `replicas:`. Mirrors `ansible/paletteai/fix-nil-values.sh` line-for-line; keep them in sync if you touch either.

**Cleanup levels** (`internal/cleaner/cleaner.go`):
- `uninstall` — helm-uninstall releases only; leaves namespaces, CRDs, webhooks.
- `full` (default) — uninstall + delete namespaces (with force-finalize escape hatch via `/finalize` subresource for stuck-`Terminating` ones) + clear three known stale admission webhooks.
- `reset` — skip helm entirely; force-delete namespaces + webhooks. Use when helm itself is jammed mid-rollback.

The cleaner also drops **ancillary namespaces** (`managed-cluster-set-*`, `open-cluster-management*`, `tenant-default`) created by the mural runtime *after* helm install — these survive `helm uninstall mural` and must be reaped separately.

**Path resolution in `cluster_config.yaml`.** Relative paths inside the file resolve from the **config file's own directory** (not the CLI's CWD). `oci://`, `https://`, `http://`, `file://` URIs pass through verbatim. See `internal/config/cluster.go:resolvePath`.

**Test seams.** `cli.deployRunner`/`cleanupRunner` function types let tests swap out the runner; `Deployer.{ConfigLoader,Helm,Kube,Log}` and `Cleaner.{ConfigLoader,Helm,Kube,Log}` are all exposed for substitution; `SDKInstaller.factory` (a `configFactory`) lets installer tests run against an in-memory helm `action.Configuration` without a real cluster. Prefer using these seams over invoking a real helm SDK in new tests.

**Test-package convention (M7 from the 2026-05-15 review).** Default to external `package foo_test` for new test files — that's the canonical Go pattern (tests must compile against the public API). Promote to internal `package foo` only when the test genuinely needs unexported access (e.g. `package helm` for tests that call `loadValues`, `releaseExists`, or `defaultConfigFactory`; `package kube` for `clientsetWrapper`-touching tests). The `helm` package legitimately has both shapes today and that's correct, not inconsistent.

## Production binary

`make build` produces a **statically linked** binary (`CGO_ENABLED=0`, `-ldflags=-extldflags=-static`). Tests build with `CGO_ENABLED=1` so the race detector works — that's intentional. Verify production builds with `file bin/palette-ai-instance` → "statically linked".

The distroless runtime image (`make image`) embeds that same binary.
