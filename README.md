# palette-ai-instance

Go CLI that drives a full PaletteAI install — `cert-manager` → `mural-crds`
→ `mural` (+ optional Bitnami RabbitMQ) — onto any Kubernetes cluster via
the helm SDK, with a post-install validator that prints the public URL it
derives from the deployed Ingress + Service.

---

## Quick start — PaletteAI on minikube

Prereqs on the host: `docker`, `make`, `kubectl`, `helm`, `minikube`,
`ansible`, `python3` (with the `bcrypt` module).

```bash
# 1. Build the static CLI (runs inside docker — no Go on the host needed).
make build

# 2. Bring up a single-node minikube + write its kubeconfig in place.
( cd examples/local_k8s && ansible-playbook up.yml && cd ../../ )

# 3. Install PaletteAI (cert-manager → CRDs → RabbitMQ → mural)
#    with --validate from the cluster_config.yaml's deploy: block.
./bin/palette-ai-instance deploy --config examples/cluster_config.yaml

# 4. (later) tear PaletteAI down — destructive, must be re-typed each time.
./bin/palette-ai-instance cleanup --config examples/cluster_config.yaml --level full

# 5. Drop the minikube too.
( cd examples/local_k8s && ansible-playbook down.yml && cd ../../ )
```

After step 3 the validator prints the URL and login:

```
=== PaletteAI connection details ===
  Canvas UI       https://192.168.49.2.sslip.io:31443/ai
  Dex login       https://192.168.49.2.sslip.io:31443/dex
  Cluster         local (kubeconfig: examples/local_k8s/artifacts/kubeconfig)
  Namespace       mural-system
```

Login: `admin@paletteai.local` / `admin` (the bcrypt hash in
[`examples/values-mural-minikube.yaml`](examples/values-mural-minikube.yaml)
corresponds to `admin` — rotate before sharing the cluster).

---

## CLI commands

```
palette-ai-instance deploy   --config FILE [flags]
palette-ai-instance cleanup  --config FILE [flags]
```

`--config FILE` is the **only required flag** on either command. The file
defines clusters and per-cluster `deploy:` defaults; everything else is
optional. If `--cluster-name` is omitted, the command runs against every
cluster declared in the file, in order, failing fast on the first error.

### `deploy`

Drives helm in this order, skipping each step whose chart URI is empty:

1. **`cert-manager`** → `cert-manager` namespace
2. **messaging queue** (e.g. Bitnami RabbitMQ) → `messaging` namespace
3. **`mural-crds`** → cluster namespace
4. **`mural`** → cluster namespace, with the `fix-nil-values` post-renderer
   automatically applied. It rewrites two known chart bugs in flight: empty
   `stringData` fields that render as YAML `nil` (API server rejects them)
   and the missing `serviceName` on the zot StatefulSet.

When `--validate` is on, it then waits for pods Ready, reads the deployed
`dex` Ingress + `ingress-nginx-controller` Service to compute the public
URL, and prints connection details.

Override precedence for any optional flag, lowest to highest:

1. Cobra default (e.g. `--helm-timeout 20m`).
2. Per-cluster `deploy:` default in `cluster_config.yaml`.
3. CLI value (only if `--flag` was actually set on the command line).

### `cleanup`

Reverses what `deploy` set up. **Cleanup options are CLI-only** — there is
intentionally no `cleanup:` block in `cluster_config.yaml` so a destructive
operation can never sneak in via config drift.

Three teardown levels via `--level`:

| Level | What it does |
|---|---|
| `uninstall` | Helm-uninstalls `mural` + `mural-crds` (+ queue / cert-manager unless `--keep-*`). Leaves namespaces, cluster-scoped CRDs/ClusterRoles, and webhooks. |
| `full` (default) | Above + deletes the namespaces (with a force-finalize escape hatch for stuck-`Terminating` ones — clears `spec.finalizers` via the `/finalize` subresource when a normal delete times out) + clears the three known stale admission webhooks. |
| `reset` | Skip helm entirely; force-delete namespaces + webhooks. Use when a half-failed release has helm itself jammed mid-rollback. |

Per-component keep flags: `--keep-cert-manager`, `--keep-queue`.

---

## `cluster_config.yaml` schema

Required per cluster: `name`, `kubeconfig`, `namespace`. Optional:
`context`, plus a `deploy:` block whose keys mirror the deploy CLI flags
(kebab-case). Paths inside the file resolve from the file's directory; `oci://`
and `https://` URIs pass through verbatim.

```yaml
clusters:
  - name: local
    kubeconfig: local_k8s/artifacts/kubeconfig
    context: local
    namespace: mural-system
    deploy:
      chart-uri:                mural-1.1.0-rc.0.tgz
      values-file:              values-mural-minikube.yaml
      crds-chart-uri:           mural-crds-0.7.1.tgz
      cert-manager-chart-uri:   https://charts.jetstack.io/charts/cert-manager-v1.16.2.tgz
      cert-manager-values-file: values-cert-manager.yaml
      queue-chart-uri:          oci://registry-1.docker.io/bitnamicharts/rabbitmq
      queue-version:            14.6.6
      queue-values-file:        values-rabbitmq.yaml
      validate:                 true
      validate-wait:            10m
```

A complete, working version of this is at
[`examples/cluster_config.yaml`](examples/cluster_config.yaml).

---

## Hacking on the CLI

All Go work happens inside the `tools` stage of the project Dockerfile so
the host only needs `docker` + `make`:

```bash
make ci         # check-versions → fmt → vet → lint → test (≥95% coverage) → build
make build      # just the static binary → ./bin/palette-ai-instance
make tidy       # go mod tidy + keep go.mod's `go` directive in sync with .versions
```

Toolchain versions (Go, alpine base, golangci-lint) live in
[`.versions`](.versions) and flow into both the Makefile and Dockerfile.
Bump in one place; `make ci` errors loudly if `go.mod`'s `go` directive
drifts from it.

Package layout:

| Package | Responsibility |
|---|---|
| `cmd/palette-ai-instance` | thin `main.go` |
| `internal/cli` | cobra commands (`deploy`, `cleanup`) + flag-vs-config merge logic |
| `internal/config` | `cluster_config.yaml` loader with per-cluster defaults |
| `internal/deployer` | orchestrates the helm install sequence |
| `internal/cleaner` | orchestrates teardown (helm uninstall + namespace + webhook cleanup) |
| `internal/helm` | helm SDK wrapper behind an `Installer` / `Uninstaller` interface |
| `internal/kube` | client-go wrapper used by `--validate` and the cleaner |
| `internal/postrender` | the `fix-nil-values` post-renderer that patches the rendered mural manifests in flight |

## License

[MIT](LICENSE) © 2026 Martin Forde <mforde84@gmail.com>, [Blik Labs](https://bliklabs.com).
