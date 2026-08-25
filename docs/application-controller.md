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
  --set-string clusterID=<workload-cluster-id>
```

OneKS must pass the actual workload cluster ID as the Helm value `clusterID`.
Releases contain only the Helm chart; no standalone manifest containing a
placeholder cluster ID is published. The static Kustomize base deliberately
retains `replace-me` as a development template and is not deployable until its
ConfigMap is replaced with the actual workload cluster ID.

Before uninstalling the controller, delete every `OneKSApplication` in
`oneks-system` and wait for its controller finalizer and managed children to be
cleaned up. Both `oneks-system` and `oneks-poc-workloads` carry Helm's `keep`
resource policy, so Helm uninstall cannot remove them and strand application
finalizers. After successful application cleanup, an operator may remove the
kept namespaces explicitly if nothing else uses them.

If controller removal or an upgrade is interrupted while application roots
remain, reinstall or roll back the chart with the same `clusterID`. The kept
namespaces and CRD preserve the roots so the controller can resume
ownership-checked reconciliation and finalizer cleanup.

## Protected input handoff

`oneks.opennebula.io/plan-v1alpha5` allows OneKS to submit an immutable Opaque
Secret and its root `OneKSApplication` together. The plan references only the
Secret namespace and random name; `secretInputRef.uid` is empty. Before it
creates any child, the controller acquires its cleanup finalizer, validates the
Secret type, immutability, exact input keys and correlation labels, and stores
the observed UID in `status.secretInputUID`. Every later read and deletion is
guarded by that UID, so a replacement Secret is never consumed or deleted.

`plan-v1alpha4` remains readable for existing applications and continues to
require the UID in `spec.secretInputRef.uid`. New protected plans should use
v1alpha5.

Repository targets:

```console
make build-application-controller
make docker-build-application-controller
make application-controller-package-check
make application-controller-release APPLICATION_CONTROLLER_VERSION=X.Y.Z
make docker-release-application-controller APPLICATION_CONTROLLER_VERSION=X.Y.Z
```

`application-controller-package-check` performs Helm lint/render and Kustomize
render checks without publishing an image or artifact. The release workflow is
triggered only by `application-controller-v*.*.*` tags and does not invoke or
retag the monitor image.
