# examples/

Self-contained day-1 deploy of PaletteAI onto a local minikube cluster.

Everything is in this folder — `cluster_config.yaml`, the chart tarballs, the
values files, the minikube playbooks that create the cluster, and the
kubeconfig those playbooks write.

From the project root:

```bash
make build                                                                   # produces ./bin/palette-ai-instance

# 1. Bring up minikube (writes examples/local_k8s/artifacts/kubeconfig)
( cd examples/local_k8s && ansible-playbook up.yml && cd ../../ )

# 2. Install PaletteAI: cert-manager → RabbitMQ → mural-crds → mural
./bin/palette-ai-instance deploy  --config examples/cluster_config.yaml

# 3. (later) tear it back down
./bin/palette-ai-instance cleanup --config examples/cluster_config.yaml --level full
( cd examples/local_k8s && ansible-playbook down.yml && cd ../../ )
```

## What step 2 installs

`cluster_config.yaml` populates all four chart URIs, so a single `deploy`
installs four helm releases **in order** — each is skipped only if its chart
URI is left blank:

| # | Release        | Namespace      | Chart source                                              |
|---|----------------|----------------|-----------------------------------------------------------|
| 1 | `cert-manager` | `cert-manager` | upstream Jetstack chart, pulled over HTTPS                |
| 2 | `queue`        | `messaging`    | Bitnami RabbitMQ, `oci://registry-1.docker.io/bitnamicharts/rabbitmq` |
| 3 | `mural-crds`   | `mural-system` | `mural-crds-0.7.1.tgz` (local)                            |
| 4 | `mural`        | `mural-system` | `mural-1.1.0-rc.0.tgz` (local)                            |

The deploy streams one line per release as it lands, so the install order is
visible in the output:

```
→ deploying cluster "local" (1/1)
release "cert-manager" rev=1 status=deployed
release "queue" rev=1 status=deployed
release "mural-crds" rev=1 status=deployed
release "mural" rev=1 status=deployed

=== PaletteAI connection details ===
  Canvas UI       https://192.168.49.2.sslip.io:31443/ai
  ...
```

## Verify cert-manager and RabbitMQ came up

`--validate` (enabled in `cluster_config.yaml`) waits for pods in the
**`mural-system`** namespace and prints the Canvas URL — it deliberately does
**not** reach into the `cert-manager` or `messaging` namespaces. Confirm those
two prerequisite releases separately:

```bash
export KUBECONFIG=examples/local_k8s/artifacts/kubeconfig

# All four releases, each STATUS=deployed:
helm list --all-namespaces

# cert-manager — controller, webhook, and cainjector pods Ready:
kubectl get pods -n cert-manager

# RabbitMQ — one pod Ready in `messaging`:
kubectl get pods -n messaging

# (optional) confirm RabbitMQ is actually serving — quorum queues, node up:
kubectl -n messaging exec queue-rabbitmq-0 -- rabbitmq-diagnostics status
```

Expected: `helm list` shows `cert-manager`, `queue`, `mural-crds`, and `mural`
all `deployed`; `cert-manager` has 3 Running pods; `messaging` has 1.

## Layout

| File                          | Purpose                                                    |
|-------------------------------|------------------------------------------------------------|
| `cluster_config.yaml`         | One-cluster config with every `deploy:` flag pre-populated. All paths in it resolve from `examples/`. Tracked in git as the documented exception to the `cluster_config.yaml` ignore rule in `../.gitignore`. |
| `local_k8s/`                  | Ansible playbooks that bring a minikube cluster up + tear it back down. `up.yml` writes the kubeconfig at `local_k8s/artifacts/kubeconfig` (gitignored — embeds cluster credentials). `cluster_config.yaml` references that exact path. |
| `mural-1.1.0-rc.0.tgz`        | Local mural helm chart.                                    |
| `mural-crds-0.7.1.tgz`        | Local mural-crds helm chart.                               |
| `values-mural-minikube.yaml`  | Full mural values with domain pinned at `192.168.49.2.sslip.io` and dev secrets baked in (rotate before sharing). |
| `values-cert-manager.yaml`    | Enables `crds.enabled: true` for the upstream Jetstack chart + trims its resource footprint for minikube. |
| `values-rabbitmq.yaml`        | Forces image registry to `docker.io/bitnamilegacy/*` (post-2025 Bitnami split) and enables `default_queue_type = quorum`. |

## Conventions

- **kubeconfig is gitignored** — see `../.gitignore` (the whole
  `local_k8s/artifacts/` directory). `local_k8s/up.yml` writes it in place;
  drop a different kubeconfig at the same path to retarget the deploy at a
  non-minikube cluster.
- **`cluster_config.yaml` is normally gitignored too** — operators' real
  per-cluster configs stay out of git. This `examples/cluster_config.yaml` is
  the one committed exception (`!examples/cluster_config.yaml` in
  `../.gitignore`) so the quick start above actually has a file to point at.
- **Domain pinned at `192.168.49.2.sslip.io:31443`** — change in
  `values-mural-minikube.yaml` if your minikube IP differs (`minikube ip -p
  local`). The `iss` / `redirectURIs` claims must match what the browser
  actually reaches. The host resolves via the public `sslip.io` wildcard
  service — `<ip>.sslip.io` → `<ip>`, no DNS record to configure.
- **Cleanup is separate**:
  ```bash
  ./bin/palette-ai-instance cleanup --config examples/cluster_config.yaml --level full
  ./bin/palette-ai-instance cleanup --config examples/cluster_config.yaml --level reset
  ```
  Cleanup flags are intentionally CLI-only — there is no `cleanup:` block
  in `cluster_config.yaml` because destructive operations must be re-typed
  on every invocation.
