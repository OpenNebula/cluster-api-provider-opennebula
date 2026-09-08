# OneKS application controller packaging

The OneKS application controller is built and released independently from the
CAPONE manager and capone-monitor. Its release tag is
`application-controller-vX.Y.Z`, its image is
`ghcr.io/opennebula/oneks-application-controller:vX.Y.Z`, and its Helm chart is
`oneks-application-controller-X.Y.Z.tgz`.

The chart requires the workload cluster identifier explicitly:

```console
helm upgrade --install oneks-application-controller \
  oci-or-local/oneks-application-controller-X.Y.Z.tgz \
  --namespace oneks-system --create-namespace \
  --set-string clusterID=<workload-cluster-id>
```

OneKS must pass the actual workload cluster ID as the Helm value `clusterID`.
Releases contain only the Helm chart; no standalone manifest containing a
placeholder cluster ID is published.

Before uninstalling the controller, delete every `OneKSApplication` in
`oneks-system` and wait for its controller finalizer and managed children to be
cleaned up. Helm owns creation of the `oneks-system` release namespace through
`--create-namespace`; the chart itself renders no Namespace objects. After
successful application cleanup, an operator may remove the controller namespace
explicitly if nothing else uses it.

If controller removal or an upgrade is interrupted while application roots
remain, reinstall or roll back the chart with the same `clusterID`. The CRD
preserves the roots so the controller can resume
ownership-checked reconciliation and finalizer cleanup.

## Protected input handoff

`oneks.opennebula.io/plan-v1beta1` allows OneKS to submit an immutable Opaque
Secret and its root `OneKSApplication` together. The plan references only the
Secret namespace and random name; the UID is bound in status. Before it
creates any child, the controller acquires its cleanup finalizer, validates the
Secret type, immutability, exact input keys and correlation labels, and stores
the observed UID in `status.secretInputUID`. Every later read and deletion is
guarded by that UID, so a replacement Secret is never consumed or deleted.

Repository targets:

```console
make build-application-controller
make docker-build-application-controller
make application-controller-package-check
make application-controller-release APPLICATION_CONTROLLER_VERSION=X.Y.Z
make docker-release-application-controller APPLICATION_CONTROLLER_VERSION=X.Y.Z
```

`application-controller-package-check` performs Helm lint/render checks without
publishing an image or artifact. The release workflow is
triggered only by `application-controller-v*.*.*` tags and does not invoke or
retag the monitor image.

## v1beta1 contract

The controller and OneKS compiler use `oneks.opennebula.io/v1beta1` and
`oneks.opennebula.io/plan-v1beta1`. This is a direct replacement for the alpha
contract, intended for fresh installations; no alpha conversion is provided.
All applications execute their plan. The API has no `executionMode` or managed
resource `apiResource` fields; resources are addressed by API version and kind.
The Go API lives in `api/application/v1beta1` and the Helm chart in
`helm/v1beta1/oneks-application-controller`.

The digest covers the API spec JSON except the top-level `planDigest`.
Object fields that are null, empty strings, empty arrays or empty objects are
omitted recursively. Array order, booleans (including false), numbers (including
zero), and non-empty strings remain significant. Embedded manifest JSON and
Helm YAML are opaque strings. Object keys are sorted, strings use ASCII JSON
escapes, and the hash remains SHA-256 with unpadded URL-safe Base64.

Ruby and Go use shared fixtures to verify this contract:

```console
go test ./internal/application ./internal/monitor ./cmd/...
ruby hack/check-oneks-canonical.rb /path/to/one-ee
```
