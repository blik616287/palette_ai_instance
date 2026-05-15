# Code Review — palette-ai-instance

**Scope:** entire `cmd/` + `internal/` Go tree, plus `Dockerfile`, `Makefile`, `.golangci.yml`, and the docker-toolchain plumbing. Date: 2026-05-15. Branch reviewed: `main` @ `08b851b`.

**Methodology.** All tooling was run inside the project's docker `tools` stage so the host setup was zero. Output of every command below was captured live.

| Stage | Tool | Result |
|---|---|---|
| Format | `gofmt -s` (via `make fmt`) | clean (no diff) |
| Vet | `go vet ./...` (via `make vet`) | clean |
| Lint | `golangci-lint run` (project config: 17 linters incl. gosec, staticcheck, errcheck, revive, gocyclo, errorlint, unused, prealloc) | clean |
| Lint (extended) | `staticcheck -checks=all` (2025.1.1) | clean |
| Lint (extended) | `errcheck -ignoretests` (v1.9.0) | 8 findings — all `fmt.Fprintln/Fprintf` to `io.Writer`, the standard ignorable class |
| Security | `gosec ./...` (v2.21.4) | 1 finding (G304 — see [M3](#m3)) |
| Vulnerabilities | `govulncheck ./...` (v1.1.4) | **43 reachable CVEs** across stdlib + helm + containerd + x/net — see [C1](#c1) |
| Tests | `go test -race ./...` (via `make coverage`) | all pass, **total 95.1% coverage** |
| Dockerfile | `hadolint` | 1 finding (DL3018 — unpinned apk versions) |

**Top-line assessment.** The code is clean, well-commented, and uniformly idiomatic. Coverage is high (95%) and the test seams (function-typed runners, interface-typed installers/kube clients, in-memory helm config) are well chosen — the suite exercises real error paths instead of mocking them away. The reason this review has a substantial finding count anyway is that "production-grade" raises the bar past lint-clean: dependency hygiene, context plumbing, destructive-default UX, and operator-facing error reporting are where the gaps live.

---

## Severity legend

- 🔴 **Critical** — security-relevant or data-loss-class. Fix before next release.
- 🟠 **High** — observable bug, broken UX, or hard-to-debug production failure mode.
- 🟡 **Medium** — quality / supply-chain / hardening. Address in current quarter.
- 🟢 **Low / nice-to-have** — polish, consistency, future-proofing.

---

## Critical

### C1 — 43 known vulnerabilities reachable from the binary <a id="c1"></a>

**Severity:** 🔴 Critical
**Where:** `go.mod` (toolchain + 3 direct dep groups)
**Tool:** `govulncheck@v1.1.4`

`govulncheck ./...` reports your code (not just imports — *reachable* code paths) is affected by **43 vulnerabilities** drawn from four sources:

| Source | Pinned at | Vulns reachable | Recommended target |
|---|---|---|---|
| Go stdlib (`go 1.23.4` in Dockerfile) | `1.23.4` | ~30 (across `net/http`, `crypto/x509`, `crypto/tls`, `archive/tar`, `net/url`, `net/mail`, `os`, `os/exec`, `encoding/pem`, `encoding/asn1`, `crypto/internal/nistec`, `net/http/internal`) | bump `GO_VERSION` to **`1.23.12`** for the minimum-disruption fix (closes every 1.23-line CVE in the report); **`1.24.x`** closes more; **`1.25.x`** closes them all |
| `helm.sh/helm/v3` | `v3.16.3` | 4 (`GO-2025-3601`, `-3602`, `-3887`, `-3888`) | `v3.18.5` (closes all four) |
| `github.com/containerd/containerd` (transitive via helm) | `v1.7.23` | 3 (`GO-2025-3528`, `GO-2025-4100`, `GO-2025-4108`) | `v1.7.29` — likely pulled in automatically by the helm bump |
| `golang.org/x/net` (transitive) | `v0.26.0` | 2 (`GO-2025-3503`, `GO-2026-4918`) | `v0.53.0` — likely pulled in automatically by the helm bump |
| `github.com/docker/docker` (transitive) | `v25.0.6+incompatible` | 3 (`GO-2025-3829`, `GO-2026-4883`, `GO-2026-4887`) | `v25.0.13+incompatible` for one; two have "Fixed in: N/A" upstream — note for risk register |

Most of the helm transitive CVEs are in the OCI / registry / docker-resolver paths — directly reached by `helm.locateAndLoad` and `helm.runInstall` because PaletteAI charts are pulled over `oci://`.

**Fix.**

```bash
# 1. Bump .versions
sed -i 's/GO_VERSION             ?= 1.23.4/GO_VERSION             ?= 1.23.12/' .versions

# 2. Bump direct helm dep + tidy (run via the tools image)
make tidy        # rebuilds the image, then go mod tidy
# Or, for the larger jump:
docker run --rm --user $(id -u):$(id -g) -e HOME=/tmp -v $PWD:/src -w /src \
  palette-ai-instance-tools:go1.23.4 sh -c \
  'go get helm.sh/helm/v3@v3.18.5 && go mod tidy'

# 3. Verify
docker run --rm --user $(id -u):$(id -g) -e HOME=/tmp -v $PWD:/src -w /src \
  palette-ai-instance-tools:go1.23.4 govulncheck ./...
```

Helm v3.18 may pull in newer k8s.io/* deps — be ready to update `k8s.io/{api,apimachinery,cli-runtime,client-go}` from `v0.31.3` to a matching release.

**Also fix:** add `govulncheck` to `make ci` (see [M1](#m1)) so this never silently re-accumulates.

---

### C2 — `SDKInstaller.Uninstall` discards `context.Context` <a id="c2"></a>

**Severity:** 🔴 Critical (operator UX / safety)
**Where:** `internal/helm/installer.go:126`

```go
func (s *SDKInstaller) Uninstall(_ context.Context, opts UninstallOptions) error {
```

The context is named `_` and never used. Because `cleanup` is the destructive path, this means an operator who hits **Ctrl-C** during a teardown does *not* interrupt the in-flight helm uninstall — they only stop the *next* uninstall in the loop. The `main.go` signal handler captures SIGINT/SIGTERM into the context, but the context never reaches helm.

**Why this matters in practice.**

- `un.Run(name)` doesn't take a `ctx` (helm SDK limitation), but the cleaner calls `Uninstall` four times in sequence. The ctx-aware abort point at minimum belongs *between* uninstalls.
- The `cleanup --level full` path then iterates ~11 namespace deletions (`mural-system`, `messaging`, `cert-manager`, plus 8 ancillary OCM namespaces). Operators expect Ctrl-C to be respected there too — the kube client does honor ctx, so the bug is helm-specific, but a partial-honor system is the worst UX (operator can't tell what was interruptible).
- Helm itself sets `un.Timeout = opts.Timeout`, so the *current call* is bounded. But the operator-driven cancel signal still doesn't propagate.

**Fix.** Two layers:

1. Cheap guard at function entry and before the helm call:
   ```go
   func (s *SDKInstaller) Uninstall(ctx context.Context, opts UninstallOptions) error {
       if err := ctx.Err(); err != nil {
           return err
       }
       // ... unchanged setup ...
       if err := ctx.Err(); err != nil {
           return err
       }
       if _, err := un.Run(opts.ReleaseName); err != nil { ... }
   }
   ```
2. The `cleaner.runHelmUninstalls` loop already short-circuits on error; have it also check `ctx.Err()` between releases:
   ```go
   for _, r := range c.plan() {
       if err := ctx.Err(); err != nil { return err }
       // ...
   }
   ```

**Bonus.** The same `_ context.Context` pattern appears in test fakes (`fakeUninstaller.Uninstall`, `fakeKube.*`). Add a `contextcheck` linter to catch future regressions (see [M2](#m2)).

---

### C3 — Duplicate `error:` printed for early failures <a id="c3"></a>

**Severity:** 🔴 Critical (operator UX, log noise, parsability)
**Where:** `internal/cli/deploy.go:99-107`, `internal/cli/cleanup.go:82-89`, `cmd/palette-ai-instance/main.go:21`

`runDeployCmd` / `runCleanupCmd` print `error: ...` to stderr *and* return the error. `main.go` then prints `error: ...` *again*:

```go
// cmd/palette-ai-instance/main.go:20-22
if err := root.ExecuteContext(ctx); err != nil {
    fmt.Fprintln(os.Stderr, "error:", err)
    os.Exit(1)
}
```

```go
// internal/cli/deploy.go:99-107
cfg, err := config.Load(f.configPath)
if err != nil {
    fmt.Fprintln(errOut, "error:", err)   // <-- print 1
    return err                            // <-- main.go prints again
}
```

**Reproduced live** (just now in this review session):

```
$ ./bin/palette-ai-instance deploy --config /does/not/exist.yaml
error: read cluster config "/does/not/exist.yaml": open /does/not/exist.yaml: no such file or directory
error: read cluster config "/does/not/exist.yaml": open /does/not/exist.yaml: no such file or directory
exit=1

$ ./bin/palette-ai-instance deploy --config valid.yaml --cluster-name nonesuch --chart-uri oci://x
error: cluster not found in config: "nonesuch"
error: cluster not found in config: "nonesuch"
exit=1
```

Both `cleanup` paths are identical.

The runner-loop path doesn't have this bug because `runDeployCmd`/`runCleanupCmd` only wrap-with-`fmt.Errorf` rather than `Fprintln` before returning, so main prints just one line:
```
error: cluster "local": cluster "local": helm install "mural": ...
```
(There's a second, milder issue there: the cleaner/deployer also already wrap `cluster "%s": %w`, so when the CLI wraps `cluster %q:` *on top of that*, you get `cluster "local": cluster "local": ...` doubled. Verify and unwrap.)

**Fix.** Delete the four `fmt.Fprintln(errOut, "error:", err)` lines. Cobra is already configured with `SilenceErrors: true`, which makes `main.go` the single, authoritative error sink. Wrap-with-context in the returned error only:

```go
cfg, err := config.Load(f.configPath)
if err != nil {
    return fmt.Errorf("load config: %w", err)
}
```

After the fix, the demo above will produce one line, as it should.

---

## High

### H1 — `WaitForPodsReady` blocks indefinitely on pods without a `PodReady` condition <a id="h1"></a>

**Severity:** 🟠 High
**Where:** `internal/kube/client.go:162-205`

`isPodReady` returns `false` for any pod whose `Status.Conditions` lacks a `PodReady` entry. `WaitForPodsReady` then requires `len(NotReady) == 0` to return success, so the loop times out.

This matters for two real scenarios:

1. **Helm install/post-install hook Jobs.** Their pods may have phase `Succeeded` and no `PodReady` condition at all. They count as not-ready forever.
2. **Pending pods during a roll.** A pod whose first condition row hasn't been written yet (very fast `List` after a `Create`) lacks PodReady. Race-dependent, but observable in test environments.

The doc comment claims parity with `kubectl wait --for=condition=Ready`, which *does* in fact also block — but kubectl is invoked against a specific resource, not "every pod in a namespace." The PaletteAI deploy creates dozens of pods including Helm hook Job pods, so the namespace-wide check is the wrong granularity.

**Fix.** Skip pods that are terminal (Succeeded / Failed) and exclude them from the total. Treat phase as authoritative when no condition exists:

```go
func isPodReady(p *corev1.Pod) bool {
    // Terminal pods don't gate readiness — they're done.
    if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
        return true
    }
    for _, c := range p.Status.Conditions {
        if c.Type == corev1.PodReady {
            return c.Status == corev1.ConditionTrue
        }
    }
    return false
}
```

Adjust the test `TestWaitForPodsReady_PodWithoutPodReadyConditionIsNotReady` accordingly — it currently locks in the wrong behavior.

---

### H2 — `--level full` silently force-finalizes namespaces after 3 minutes <a id="h2"></a>

**Severity:** 🟠 High (data-integrity / orphaned cluster-scoped resources)
**Where:** `internal/cleaner/cleaner.go:242-271`, `internal/cli/cleanup.go:71-73`

`dropNamespaces` does this for every namespace:

```go
if err := k.WaitForNamespaceGone(ctx, ns, req.NamespaceWait); err != nil {
    if !errors.Is(err, kube.ErrTimeout) { return err }
    c.logf("namespace %q stuck terminating after %s; force-finalizing", ns, req.NamespaceWait)
    if err := k.ForceFinalizeNamespace(ctx, ns); err != nil { return err }
    // ...
}
```

`ForceFinalizeNamespace` clears `spec.finalizers` via `/finalize` — exactly the escape hatch Kubernetes warns against, because finalizers gate cleanup of cluster-scoped resources (CRs, PVs, etc.).

This is currently:
- **automatic** (no flag to disable),
- on the **default** cleanup level (`full`),
- with a **3-minute** default deadline (`--namespace-wait`).

3 minutes is short. A namespace with PVCs backed by an external CSI, or finalized by an in-cluster controller doing its own cleanup, will trip this routinely. The operator gets one log line and the resources are orphaned.

The code's own docstring for `ForceFinalizeNamespace` says:

> Use only when a Delete + Wait have already failed; **finalizers exist for a reason and ripping them out can orphan cluster-scoped resources.**

…and yet the caller invokes it unconditionally.

**Fix.** Make force-finalize opt-in:

1. Add `--force-finalize-stuck` (bool, default `false`) to `cleanup.go`.
2. Plumb it through `cleaner.Request.ForceFinalizeStuck`.
3. In `dropNamespaces`, on timeout: if not opted-in, return the timeout error with a clear message (`namespace %q still terminating after %s; pass --force-finalize-stuck to clear spec.finalizers (DANGEROUS)`); if opted-in, do what it does today.
4. Consider bumping the default `--namespace-wait` to **10m** — closer to realistic finalizer cleanup latency.

The `reset` level can keep force-finalize on (`reset` is the explicit "blow it all away" lever).

---

### H3 — Helm uninstall hardcodes `DisableHooks = true` with no override <a id="h3"></a>

**Severity:** 🟠 High (incorrect teardown for charts that rely on pre-delete hooks)
**Where:** `internal/helm/installer.go:157-162`

```go
// `DisableHooks: true` matches helm's `--no-hooks` flag. We turn it on
// because the mural chart's pre-delete hook job often hangs ...
un.DisableHooks = true
```

The justification is specific to the mural chart's broken pre-delete hook. But the `Uninstall` interface is reused by `cert-manager`, `queue` (Bitnami RabbitMQ), and `mural-crds` — none of which have that bug. cert-manager in particular has pre-delete hooks that the cluster needs (the cleanup webhook that prevents stale `Certificate` finalizers).

**Fix.**

1. Add `DisableHooks bool` to `helm.UninstallOptions`.
2. In `cleaner.plan()`, set `DisableHooks: true` only for `{name: "mural"}` (the actual broken chart) — leave others at `false`.
3. Or, more conservatively: add a `--disable-helm-hooks` CLI flag that defaults to `true` for backwards compat, so an operator can flip it.

This is also the right shape for future PaletteAI chart fixes — when the upstream chart pins its pre-delete hook, the workaround can be removed without code surgery.

---

### H4 — First API call in poll loops isn't context-bounded <a id="h4"></a>

**Severity:** 🟠 High (apparent hang on bad cluster connectivity)
**Where:** `internal/kube/client.go:99-116, 162-184`

`WaitForNamespaceGone` and `WaitForPodsReady` both compute `deadline := time.Now().Add(timeout)` and pass the *caller's* `ctx` (unbounded) into the first `Get`/`List`. If the API server is unreachable (DNS hang, TLS handshake stall), the very first call can run *longer than the loop's deadline* and the timeout looks like it doesn't fire.

**Fix.** Bind a derived context up front:

```go
func (c *clientsetWrapper) WaitForPodsReady(ctx context.Context, namespace string, timeout time.Duration) (PodSummary, error) {
    ctx, cancel := context.WithTimeout(ctx, timeout)
    defer cancel()
    // ... loop now relies on ctx.Done() for both pacing and absolute deadline ...
}
```

While here, consider switching the polling pattern to `wait.PollUntilContextTimeout` from `k8s.io/apimachinery/pkg/util/wait` — it's the canonical client-go idiom and handles cancellation correctly out of the box.

---

### H5 — `cluster %q:` wrapping doubles up <a id="h5"></a>

**Severity:** 🟠 High (log clarity)
**Where:** `internal/cli/deploy.go:114-115`, `internal/cli/cleanup.go:97-98`, and the deployer/cleaner errors they wrap

The CLI loop wraps every per-cluster error:

```go
if err := run(cmd.Context(), req, out); err != nil {
    return fmt.Errorf("cluster %q: %w", cluster.Name, err)
}
```

…but the underlying deployer/cleaner errors already include the cluster (via `targeting cluster %q` log lines and per-step wrapping like `install %s: %w`). After main.go prefixes with `error:`, an install failure surfaces as:

```
error: cluster "local": install mural: helm install "mural": ...
```

That's tolerable — but combined with [C3](#c3) and any future re-wrap, the layering bloats fast. Either:

1. Drop the CLI-layer wrap (one ID is enough — the deployer log line `targeting cluster "local"` already says where).
2. Or keep it but make it the *only* identification, and drop the `targeting cluster` log line.

Pick one — currently you have both.

---

## Medium

### M1 — `make ci` doesn't run `govulncheck` <a id="m1"></a>

**Severity:** 🟡 Medium
**Where:** `Makefile:135`

The CI pipeline today: `check-versions fmt vet lint coverage-check build`. Excellent baseline, but no vuln scan. Given [C1](#c1), CI is the only thing that will keep dependency drift from re-introducing CVEs over the project's lifetime.

**Fix.**

```makefile
.PHONY: vulncheck
vulncheck: tools-image
	$(DOCKER_RUN) sh -c '\
	  go install golang.org/x/vuln/cmd/govulncheck@v1.1.4 && \
	  $$GOPATH/bin/govulncheck ./...'

ci: check-versions fmt vet lint coverage-check vulncheck build
```

Or, since vuln scanning shouldn't block on every commit, run it in a separate `make audit` target and have CI invoke both `make ci` and `make audit` as parallel jobs. Failing the build on *known* vulns of high severity is reasonable; failing on every new advisory in transitive deps is operationally noisy.

---

### M2 — `golangci-lint` config is short of "gold standard" <a id="m2"></a>

**Severity:** 🟡 Medium
**Where:** `.golangci.yml`

Current set: `errcheck, errorlint, gocyclo, gofmt, goimports, gosec, gosimple, govet, ineffassign, misspell, nolintlint, prealloc, revive, staticcheck, unconvert, unparam, unused`. Good, but missing several that would have caught issues in this review:

| Linter | What it would catch here |
|---|---|
| **`contextcheck`** | Functions that take a `ctx` and pass `context.Background()` (or drop it entirely) downstream — would have flagged [C2](#c2) |
| **`bodyclose`** | Unclosed HTTP response bodies (not a current issue but defense for future helm/k8s wrapping) |
| **`noctx`** | HTTP requests without `Context` — same defense |
| **`nilerr`** | Returning `nil` after checking `err != nil` |
| **`nilnil`** | Returning both `nil, nil` |
| **`paralleltest`** | Table-driven tests that don't call `t.Parallel()` (several in this repo loop with `tc := tc` and could parallelize) |
| **`tparallel`** | Validates `t.Parallel` usage in subtests |
| **`gocritic`** | Broad pattern set; cheap to enable |
| **`predeclared`** | Shadowing built-ins like `len`, `new`, `error` |
| **`usestdlibvars`** | e.g. `http.MethodGet` over `"GET"` |
| **`testifylint`** | Useful even though this repo doesn't currently use testify (sets the floor for if it ever does) |

**Recommended additions** (start with `contextcheck` and `nilerr` — both are zero-config and likely to fire on real defects).

---

### M3 — `gosec` G304 on operator-supplied config path <a id="m3"></a>

**Severity:** 🟡 Medium
**Where:** `internal/config/cluster.go:63`

```
[/src/internal/config/cluster.go:63] - G304 (CWE-22): Potential file inclusion via variable (Confidence: HIGH, Severity: MEDIUM)
  > 63:     data, err := os.ReadFile(path)
```

The path is a CLI argument the operator chose — not untrusted user input — so the risk is mitigated. But the finding will recur on every gosec run and the project has no annotation distinguishing accepted vs unaddressed.

**Fix.** Either annotate at the call site (preferred — keeps the gosec output clean and documents the threat model decision):

```go
data, err := os.ReadFile(path) // #nosec G304 -- operator-supplied --config path
```

…or add a project-level `gosec` config to suppress G304 globally (worse — also hides any future call that *does* take a network-sourced path).

While here, consider `filepath.Clean(path)` before `os.ReadFile` — cosmetic, doesn't change behavior, but tightens the static-analysis story.

---

### M4 — Dockerfile not pinned to digests; apk versions unpinned <a id="m4"></a>

**Severity:** 🟡 Medium
**Where:** `Dockerfile`

```
FROM golang:${GO_VERSION}-alpine${ALPINE_VERSION} AS tools       # mutable tag
FROM gcr.io/distroless/static-debian12:nonroot AS runtime         # mutable tag
RUN apk add --no-cache git make ca-certificates gcc musl-dev      # no =version pins
```

Hadolint flagged DL3018 (apk pin). Both base images are also pulled by tag, so a `docker build --no-cache` next month may produce a subtly different binary even with `.versions` unchanged.

**Fix.**

1. Pin both `FROM` lines by digest. The first stage's resolved digest from this session was `sha256:6a84ccdb73e005d0ee7bfff6066f230612ca9dff3e88e31bfc752523c3a271f8` — encode it as `FROM golang:${GO_VERSION}-alpine${ALPINE_VERSION}@sha256:6a84… AS tools`. (Distroless is harder because it doesn't expose stable digests cleanly; at minimum, pin the major release.)
2. Pin apk packages: `apk add --no-cache git=2.45.2-r0 make=4.4.1-r2 ...`. Alpine package versions move; a Renovate config can keep this current.
3. Consider adding `--no-progress` to the apk command, and consider whether `gcc` and `musl-dev` are needed (they're for `-race`-enabled tests; static builds don't need them). Splitting build-time deps out of the runtime stages would shrink the image.

---

### M5 — Binary has no version stamping <a id="m5"></a>

**Severity:** 🟡 Medium (operability)
**Where:** `Makefile:121-130`, `cmd/palette-ai-instance/main.go`

`make build` produces a binary with no embedded version, git SHA, or build date. **Verified live:**

```
$ ./bin/palette-ai-instance --version
error: unknown flag: --version
exit=1
```

For an operator tool that runs against production clusters, this makes incident triage harder than necessary ("what version was this deploy run against?").

**Fix.**

```makefile
GIT_SHA   := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_TS  := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
VERSION   ?= $(shell git describe --tags --always 2>/dev/null || echo dev)

LDFLAGS := -s -w -extldflags=-static \
  -X main.version=$(VERSION) -X main.gitSHA=$(GIT_SHA) -X main.buildTime=$(BUILD_TS)
```

```go
// main.go
var (
    version   = "dev"
    gitSHA    = "unknown"
    buildTime = "unknown"
)
// register a `version` subcommand or `--version` flag on root.
```

---

### M6 — `releaseExists` misclassifies `uninstalled` releases with history kept <a id="m6"></a>

**Severity:** 🟡 Medium (idempotency edge case)
**Where:** `internal/helm/installer.go:208-220`

```go
func releaseExists(cfg *action.Configuration, name string) (bool, error) {
    hist := action.NewHistory(cfg)
    hist.Max = 1
    _, err := hist.Run(name)
    switch {
    case err == nil: return true, nil
    case errors.Is(err, driver.ErrReleaseNotFound): return false, nil
    ...
}
```

`action.NewHistory().Run` returns history rows including releases in status `uninstalled` (when uninstalled with `--keep-history`). `Apply` will then take the upgrade branch and helm upgrade against an uninstalled release fails with `cannot upgrade release ... has status "uninstalled"`.

Not common with the current cleaner (it uninstalls without `KeepHistory`), but it's a footgun for any operator who runs `helm uninstall --keep-history` out-of-band and then re-deploys via this CLI.

**Fix.** Filter the history row's status:

```go
rels, err := hist.Run(name)
switch {
case errors.Is(err, driver.ErrReleaseNotFound):
    return false, nil
case err != nil:
    return false, fmt.Errorf("query release history: %w", err)
}
for _, r := range rels {
    if r.Info != nil && r.Info.Status == release.StatusDeployed {
        return true, nil
    }
}
return false, nil
```

---

### M7 — Inconsistent test package style <a id="m7"></a>

**Severity:** 🟡 Medium (maintainability)
**Where:** every `internal/*/*_test.go`

| Package | Test package | Notes |
|---|---|---|
| `cli` | `package cli` (internal) | Touches unexported `deployFlags`, `pickStr`, etc. |
| `cleaner` | `package cleaner_test` (external) | Could be internal for `releaseSpec` exposure |
| `config` | `package config_test` (external) | OK |
| `deployer` | `package deployer_test` (external) | OK |
| `helm` | **both** — `helm_test.go` is external, `installer_test.go` + `uninstall_test.go` are internal | Inconsistent within one package |
| `kube` | `package kube` (internal) | Needed for `clientsetWrapper` access |
| `postrender` | `package postrender_test` (external) | OK |

**Fix.** Prefer external (`_test`) by default; promote to internal only when a test genuinely needs unexported access. In `helm`, decide one style for the package and move shared helpers (`inMemoryConfig`, `mustStore`) to a shared `*_test.go` file under the chosen package name.

---

### M8 — `mergeDeployRequest` makes `--flag=zero` undistinguishable from `--flag` unset <a id="m8"></a>

**Severity:** 🟡 Medium (subtle UX)
**Where:** `internal/cli/deploy.go:140-197`

The precedence logic uses `cmd.Flags().Changed(name)`. For most flags this works because cobra default ≠ zero. But for `--helm-timeout 0`:

- The CLI value 0 wins (Changed = true) over the config's `helm-timeout: 5m`.
- Then `deployer.Deploy` does `if timeout <= 0 { timeout = defaultTimeout }` (20m) — silently substituting an entirely different value.

Result: `--helm-timeout 0` actually means "20 minutes" — surprising for any operator who explicitly typed 0.

**Fix.** Two options:

1. Have the deployer reject `Timeout <= 0` with an error instead of silently substituting (best for clarity).
2. Or move the default-substitution into `mergeDeployRequest` so the precedence chain is explicit ("CLI 0 → use the cobra default 20m"). Less surprising but harder to audit.

(Lower-priority sibling: the same applies to `--validate-wait 0`.)

---

## Low / nice-to-have

### L1 — Stale comment references nonexistent script

**Where:** `internal/deployer/deployer.go:167`

```go
// post-install pod-readiness wait happens out-of-band via kubectl
// (see examples/deploy-minikube.sh).
```

There's no `examples/deploy-minikube.sh` (the actual playbook is `examples/local_k8s/up.yml`, and `--validate` now handles the wait in-process). Either remove the reference or update it to point at the `validate()` function below.

---

### L2 — `fix-nil-values` post-renderer is regex-based and fragile

**Where:** `internal/postrender/fixnil.go`

- `isZotStatefulSet` scans 10 lines forward looking for `name: zot`. If the chart inserts `labels:` or `annotations:` between `kind: StatefulSet` and `metadata.name`, the check silently misses and the StatefulSet is rejected by the API server with the original "missing serviceName" error.
- `injectZotServiceName` resets `inZotStatefulSet` only on hitting `replicas:`. If a future chart change drops the `replicas:` field (it's optional — defaults to 1), the flag stays true and the next StatefulSet gets `serviceName: zot` injected falsely.

**Fix (when time permits).** Switch to a YAML-AST-based fixer using `gopkg.in/yaml.v3`'s multi-document decoder. Decode → walk → mutate → re-encode. The current regex approach is a faithful port of the Ansible script but the code already has the structure to do this properly.

Better still: get the chart fixed upstream and delete this package entirely.

---

### L3 — Hardcoded ancillary namespace + stale-webhook lists

**Where:** `internal/cleaner/cleaner.go:101-110, 140-146`

The lists `ancillaryNamespaces` and `staleWebhooks` are pinned to the mural chart's current shape. Every chart bump risks drift (namespaces renamed, webhooks added/removed).

**Fix (defense-in-depth).** Once the helm bump in [C1](#c1) lands, prefer a label-selector-based sweep:

```go
opts := metav1.ListOptions{LabelSelector: "app.kubernetes.io/managed-by=Helm,app.kubernetes.io/instance=mural"}
namespaces.List(ctx, opts)
```

Webhooks expose the same labels. Falls back gracefully if labels are missing; documents the intent (clean up *this* release's leftovers, not a hardcoded set).

---

### L4 — `file://` URI passes through without baseDir resolution

**Where:** `internal/config/cluster.go:141-147`

```go
func isURIScheme(s string) bool {
    for _, prefix := range []string{"oci://", "https://", "http://", "file://"} { ... }
}
```

`file://./chart.tgz` is treated as already-resolved, but it's a relative URI — helm will look for it relative to the *process* CWD, not the config-file directory. Either drop `file://` from the schemes list (let it resolve like a path) or document the gotcha.

---

### L5 — `runInstall` and `runUpgrade` are duplicative

**Where:** `internal/helm/installer.go:222-264`

Both functions:
- Build an action with `Namespace`/`Wait`/`Timeout`/`Version`/`PostRenderer`.
- Call `locateAndLoad(&action.ChartPathOptions, env, opts.ChartURI)`.
- Call `action.RunWithContext(ctx, ...)`.
- Wrap the error.

Minor refactor opportunity — split out the shared chart-loading prelude. Low-impact, no behavior change.

---

### L6 — `cleanup` namespace deletions are serial

**Where:** `internal/cleaner/cleaner.go:242-271`

11 namespace deletions in series, each with a `WaitForNamespaceGone` that can take up to 3 minutes. Worst case: 33 minutes for a full teardown. Most deletions are independent (the ancillary namespaces don't depend on each other).

**Fix (when teardown time matters).** Parallelize with an `errgroup.Group` capped at e.g. 4 concurrent deletes. Helm uninstalls have to stay serial (helm's release record is per-namespace), but the namespace-drop phase doesn't.

---

### L7 — Errcheck false-positives mask `fmt.Fprintln/Fprintf` write errors

**Where:** `internal/cli/{deploy,cleanup}.go`, `main.go`

8 errcheck findings for writes to `out`/`errOut`. These are the classic "ignore" class — golangci-lint has them excluded by default and this project follows suit. But in principle, a `Fprintln(errOut, ...)` against a closed pipe (e.g. operator piped `2>/dev/null | head -1`) silently swallows the error. Not a real issue here; flagging only so a future reviewer doesn't re-discover this question.

---

### L8 — `paralleltest` opportunities

Several table-driven tests already do the `tc := tc` capture and would parallelize cleanly: `TestUninstallOptions_Validate`, `TestInstallOptions_Validate`, `TestLoad_Validation`. Adding `t.Parallel()` would shave wall-clock — the kube package tests already take 12s on their own (per the coverage run).

---

## Summary table

| ID | Severity | Title | Effort |
|---|---|---|---|
| [C1](#c1) | 🔴 | Bump Go + helm + indirect deps to clear 43 CVEs | ~2h (incl. k8s.io coordination) |
| [C2](#c2) | 🔴 | Plumb `ctx` through helm Uninstall + cleaner loop | ~1h |
| [C3](#c3) | 🔴 | Remove duplicate `error:` prints | ~15min |
| [H1](#h1) | 🟠 | `WaitForPodsReady` ignore terminal pods | ~30min + test update |
| [H2](#h2) | 🟠 | Gate force-finalize behind `--force-finalize-stuck`; bump default wait | ~1h |
| [H3](#h3) | 🟠 | Make `DisableHooks` per-release, not global | ~45min |
| [H4](#h4) | 🟠 | `context.WithTimeout` upfront in poll loops | ~30min |
| [H5](#h5) | 🟠 | De-dup cluster-ID wrapping | ~15min |
| [M1](#m1) | 🟡 | Add `govulncheck` target to CI | ~30min |
| [M2](#m2) | 🟡 | Enable `contextcheck`, `nilerr`, `paralleltest`, `gocritic` | ~1h + fix new findings |
| [M3](#m3) | 🟡 | Annotate G304 with `#nosec` + rationale | ~5min |
| [M4](#m4) | 🟡 | Pin Dockerfile base images by digest, pin apk versions | ~30min |
| [M5](#m5) | 🟡 | Embed `version`/`gitSHA`/`buildTime` + `version` subcommand | ~45min |
| [M6](#m6) | 🟡 | `releaseExists` should filter by `StatusDeployed` | ~20min |
| [M7](#m7) | 🟡 | Standardize test package style | ~1h |
| [M8](#m8) | 🟡 | Reject `Timeout <= 0` instead of silently substituting | ~15min |
| [L1](#l1) | 🟢 | Remove stale `examples/deploy-minikube.sh` reference | ~2min |
| [L2](#l2) | 🟢 | Replace regex fixnil with YAML-AST walker | ~3h |
| [L3](#l3) | 🟢 | Move cleaner sweeps from hardcoded lists to label selectors | ~2h |
| [L4](#l4) | 🟢 | Drop `file://` from URI scheme list (or document) | ~10min |
| [L5](#l5) | 🟢 | Factor out shared install/upgrade prelude | ~30min |
| [L6](#l6) | 🟢 | Parallelize namespace deletions with capped errgroup | ~1h |
| [L7](#l7) | 🟢 | (note) errcheck false-positives are intentional | — |
| [L8](#l8) | 🟢 | Add `t.Parallel()` to table-driven tests | ~20min |

**Suggested ordering for one focused PR series:**

1. **PR 1 (security):** C1 + M1. Coordinate the k8s.io bump; CI then keeps drift from re-accumulating.
2. **PR 2 (correctness):** C2 + C3 + H1 + H4 + H5. Small, all UX/ctx-related.
3. **PR 3 (destructive defaults):** H2 + H3 + M8. Operator-facing behavior change — call out in changelog.
4. **PR 4 (hygiene):** M2 + M3 + M4 + M5 + M6 + M7.
5. **PR 5+ (polish):** L-bucket as schedule allows.

---

*Generated 2026-05-15 from branch `main` @ `08b851b`, by static analysis (`gofmt`, `go vet`, `golangci-lint` w/ project config, `staticcheck -checks=all`, `errcheck`, `gosec`, `govulncheck`, `hadolint`) + manual review.*
