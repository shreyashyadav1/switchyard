# Kubernetes manifests

A Kustomize base for running Switchyard on any Kubernetes cluster (kind, GKE, EKS).

| File | What it creates |
| --- | --- |
| `deployment.yaml` | One replica, non-root, read-only root filesystem, all capabilities dropped, `/healthz` readiness and liveness probes, a 5 s pre-stop pause so endpoints drain before shutdown |
| `service.yaml` | `switchyard` (port 80, the API) and `switchyard-metrics` (port 9090), so the API Service never exposes metrics |
| `networkpolicy.yaml` | API reachable from any namespace; metrics only from the `monitoring` namespace |
| `serviceaccount.yaml` | A service account with no API token mounted |

Secrets are not part of the manifests. Create them yourself so neither the config nor provider
keys end up in Git:

```sh
kubectl create namespace switchyard
kubectl -n switchyard create secret generic switchyard-config --from-file=switchyard.yaml
kubectl -n switchyard create secret generic switchyard-provider-keys \
  --from-literal=GROQ_API_KEY="$GROQ_API_KEY"
```

Then deploy, setting the image you built:

```sh
cd deploy/kubernetes
kustomize edit set image switchyard=REGISTRY/switchyard:v0.1.0
kubectl apply -n switchyard -k .
kubectl -n switchyard port-forward svc/switchyard 8080:80
```

On kind, build the image locally and load it instead of pushing it:

```sh
docker build -t switchyard:dev .
kind load docker-image switchyard:dev
kubectl apply -n switchyard -k deploy/kubernetes
```

Notes:

- Keep `replicas: 1`. Rate limits, budgets and the cache are per process, so each extra
  replica gives every key another full set of limits.
- Pods don't restart when a Secret changes. After updating the config, run
  `kubectl -n switchyard rollout restart deploy/switchyard`.
- The NetworkPolicy needs a CNI that enforces it (Calico, Cilium, GKE Dataplane V2).
- `make k8s-check` renders the base and validates it against the Kubernetes 1.33 schemas with
  kubeconform; CI runs the same check.
