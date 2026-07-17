# In-cluster status monitor

`cmd/monitor` is a separate binary intended to run in each workload cluster. It
uses Kubernetes shared informers (`List` followed by `Watch`) for Nodes and,
within the configured chart namespace, `helm.cattle.io/v1` HelmCharts and their
installer Jobs. It does not read Secrets or Helm release storage.

## Configuration

All settings can be supplied directly as environment variables or through an
`envFrom` ConfigMap. The example deployment is in
`kustomize/v1beta1/monitor`.

| Variable | Required | Default | Meaning |
|---|---:|---|---|
| `MONITOR_ENDPOINT` | yes | | URL receiving HTTP `POST` reports |
| `MONITOR_CLUSTER_ID` | yes | | Stable external identifier for the cluster |
| `MONITOR_CHART_ANNOTATION` | no | `oneks.opennebula.io/chart-id` | Annotation selecting HelmCharts |
| `MONITOR_CHART_NAMESPACE` | no | `kube-system` | Namespace containing the HelmChart resources |
| `MONITOR_RESYNC_PERIOD` | no | `10m` | Safety reconciliation interval; changes are still event-driven |
| `MONITOR_HTTP_TIMEOUT` | no | `10s` | Timeout for one report attempt |

Edit the ConfigMap and apply the manifests:

```sh
kubectl apply -k kustomize/v1beta1/monitor
```

Alternatively, generate and install the dedicated Helm chart. As with the
CAPONE template charts, its Kubernetes manifest is generated from `config`:

```sh
make charts CLOSEST_TAG=v0.1.0
helm upgrade --install capone-monitor _charts/v0.1.0/capone-monitor-0.1.0.tgz \
  --namespace kube-system --create-namespace \
  --set monitor.endpoint=https://monitor.example/v1/status \
  --set monitor.clusterID=production-1
```

Build the dedicated image with `make docker-build-monitor`. Set a deployment
image without editing YAML with:

```sh
cd kustomize/v1beta1/monitor
kustomize edit set image monitor=registry.example/monitor:v1
```

## Endpoint contract

The endpoint must accept a JSON `POST`. A typical Node update is:

```json
{
  "clusterId": "production-1",
  "kind": "Node",
  "name": "worker-1",
  "uid": "f151a3f0-...",
  "resourceVersion": "23852",
  "event": "Updated",
  "observedAt": "2026-07-16T12:00:00Z",
  "status": {
    "state": "warning"
  }
}
```

HelmChart reports use `kind: HelmChart` and expose `chartId` plus `status`.
The status is derived from the HelmChart conditions and the Job named by
`status.jobName`, both in the configured chart namespace. It is one of
`pending`, `deployed`, `failed`, `uninstalling`, or `unknown`. This deliberately
does not distinguish install, upgrade, and rollback operations because doing
so would require reading Helm release storage. Deletions have `event: Deleted`
and `status.deleted: true`.

Every request carries an `Idempotency-Key`, so the receiver should store
updates idempotently. Failed requests remain in a rate-limited work queue, and
a later event for the same object replaces the queued report with the newest
state. A single worker processes all reports serially.

## Delivery semantics

Kubernetes watches are not durable message queues. Informers recover watch
disconnects with `resourceVersion`, handle expired history by listing again,
and the periodic resync repairs the externally stored *current state*. A pod
restart also performs a full initial list, so the current state is eventually
reported again.

This guarantees convergence of current state, not delivery of every transient
intermediate transition. If every transition must be retained while both the
monitor and endpoint are unavailable, add a durable outbox (for example a
small external queue) or consume Kubernetes audit events; an in-memory
informer cannot provide that guarantee.
