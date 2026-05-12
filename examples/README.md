# examples/

Self-contained day-1 deploy of PaletteAI onto a local minikube cluster.

Everything is in this folder — chart tarballs, values files, the minikube
playbooks that create the cluster, and the kubeconfig those playbooks write.
From the project root:

```bash
make build                                                                   # produces ./bin/palette-ai-instance

# 1. Bring up minikube (writes examples/local_k8s/artifacts/kubeconfig)
( cd examples/local_k8s && ansible-playbook up.yml && cd ../../ )

# 2. Install PaletteAI
./bin/palette-ai-instance deploy  --config examples/cluster_config.yaml

# 3. (later) tear it back down
./bin/palette-ai-instance cleanup --config examples/cluster_config.yaml --level full
( cd examples/local_k8s && ansible-playbook down.yml && cd ../../ )
```

## Layout

| File                          | Purpose                                                    |
|-------------------------------|------------------------------------------------------------|
| `cluster_config.yaml`         | One-cluster config with every `deploy:` flag pre-populated. All paths in here resolve from `examples/`. |
| `local_k8s/`                  | Ansible playbooks that bring a minikube cluster up + tear it back down. `up.yml` writes the kubeconfig at `local_k8s/artifacts/kubeconfig` (gitignored — embeds cluster credentials). `cluster_config.yaml` references that exact path. |
| `mural-1.1.0-rc.0.tgz`        | Local mural helm chart.                                    |
| `mural-crds-0.7.1.tgz`        | Local mural-crds helm chart.                               |
| `values-mural-minikube.yaml`  | Full mural values with domain pinned at `192.168.49.2.sslip.io` and dev secrets baked in (rotate before sharing). |
| `values-cert-manager.yaml`    | Enables `crds.enabled: true` for the upstream Jetstack chart. |
| `values-rabbitmq.yaml`        | Forces image registry to `docker.io/bitnamilegacy/*` (post-2025 Bitnami split) and enables `default_queue_type = quorum`. |

## Conventions

- **kubeconfig is gitignored** — see `../.gitignore` (the whole
  `local_k8s/artifacts/` directory). `local_k8s/up.yml` writes it in place;
  drop a different kubeconfig at the same path to retarget the deploy at a
  non-minikube cluster.
- **Domain pinned at `192.168.49.2.sslip.io:31443`** — change in
  `values-mural-minikube.yaml` if your minikube IP differs. The `iss` /
  `redirectURIs` claims must match what the browser actually reaches.
- **Cleanup is separate**:
  ```bash
  ./bin/palette-ai-instance cleanup --config examples/cluster_config.yaml --level full
  ./bin/palette-ai-instance cleanup --config examples/cluster_config.yaml --level reset
  ```
  Cleanup flags are intentionally CLI-only — there is no `cleanup:` block
  in `cluster_config.yaml` because destructive operations must be re-typed
  on every invocation.
