-include .env
export

# Image URL to use all building/pushing image targets
IMG ?= localhost:5005/capi-opennebula-controller:latest
# ENVTEST_K8S_VERSION refers to the version of kubebuilder assets to be downloaded by envtest binary.
ENVTEST_K8S_VERSION = 1.31.1

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

# CONTAINER_TOOL defines the container tool to be used for building images.
# Be aware that the target commands are only tested with Docker which is
# scaffolded by default. However, you might want to replace it to use other
# tools. (i.e. podman)
CONTAINER_TOOL ?= docker

# Setting SHELL to bash allows bash commands to be executed by recipes.
# Options are set to exit when a recipe line exits non-zero or a piped command fails.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

.PHONY: all
all: build

##@ General

# The help target prints out all targets with their descriptions organized
# beneath their categories. The categories are represented by '##@' and the
# target descriptions by '##'. The awk command is responsible for reading the
# entire set of makefiles included in this invocation, looking for lines of the
# file as xyz: ## something, and then pretty-format the target and help. Then,
# if there's a line with ##@ something, that gets pretty-printed as a category.
# More info on the usage of ANSI control characters for terminal formatting:
# https://en.wikipedia.org/wiki/ANSI_escape_code#SGR_parameters
# More info on the awk command:
# http://linuxcommand.org/lc3_adv_awk.php

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: manifests
manifests: controller-gen ## Generate WebhookConfiguration, ClusterRole and CustomResourceDefinition objects.
	$(CONTROLLER_GEN) rbac:roleName=manager-role crd webhook paths="./..." output:crd:artifacts:config=config/crd/bases

.PHONY: generate
generate: controller-gen ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./..."

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: test
test: manifests generate fmt vet envtest ## Run tests.
	KUBEBUILDER_ASSETS="$(shell $(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path)" go test $$(go list ./... | grep -v /e2e) -coverprofile cover.out

# Utilize Kind or modify the e2e tests to load the image locally, enabling compatibility with other vendors.
.PHONY: test-e2e  # Run the e2e tests against a Kind k8s instance that is spun up.
test-e2e:
	go test ./test/e2e/ -v -ginkgo.v

.PHONY: lint
lint: golangci-lint ## Run golangci-lint linter
	$(GOLANGCI_LINT) run

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint linter and perform fixes
	$(GOLANGCI_LINT) run --fix

##@ Build

.PHONY: build
build: manifests generate fmt vet ## Build manager binary.
	go build -o bin/manager cmd/main.go

.PHONY: run
run: manifests generate fmt vet ## Run a controller from your host.
	go run ./cmd/main.go

# If you wish to build the manager image targeting other platforms you can use the --platform flag.
# (i.e. docker build --platform linux/arm64). However, you must enable docker buildKit for it.
# More info: https://docs.docker.com/develop/develop-images/build_enhancements/
.PHONY: docker-build
docker-build: ## Build docker image with the manager.
	$(CONTAINER_TOOL) build -t ${IMG} .

.PHONY: docker-push
docker-push: ## Push docker image with the manager.
	$(CONTAINER_TOOL) push ${IMG}

# PLATFORMS defines the target platforms for the manager image be built to provide support to multiple
# architectures. (i.e. make docker-buildx IMG=myregistry/mypoperator:0.0.1). To use this option you need to:
# - be able to use docker buildx. More info: https://docs.docker.com/build/buildx/
# - have enabled BuildKit. More info: https://docs.docker.com/develop/develop-images/build_enhancements/
# - be able to push the image to your registry (i.e. if you do not set a valid value via IMG=<myregistry/image:<tag>> then the export will fail)
# To adequately provide solutions that are compatible with multiple platforms, you should consider using this option.
PLATFORMS ?= linux/arm64,linux/amd64,linux/s390x,linux/ppc64le
.PHONY: docker-buildx
docker-buildx: ## Build and push docker image for the manager for cross-platform support
	# copy existing Dockerfile and insert --platform=${BUILDPLATFORM} into Dockerfile.cross, and preserve the original Dockerfile
	sed -e '1 s/\(^FROM\)/FROM --platform=\$$\{BUILDPLATFORM\}/; t' -e ' 1,// s//FROM --platform=\$$\{BUILDPLATFORM\}/' Dockerfile > Dockerfile.cross
	- $(CONTAINER_TOOL) buildx create --name cluster-api-provider-opennebula-builder
	$(CONTAINER_TOOL) buildx use cluster-api-provider-opennebula-builder
	- $(CONTAINER_TOOL) buildx build --push --platform=$(PLATFORMS) --tag ${IMG} -f Dockerfile.cross .
	- $(CONTAINER_TOOL) buildx rm cluster-api-provider-opennebula-builder
	rm Dockerfile.cross

.PHONY: build-installer
build-installer: manifests generate kustomize ## Generate a consolidated YAML with CRDs and deployment.
	install -d dist
	cd config/manager && $(KUSTOMIZE) edit set image controller=${IMG}
	$(KUSTOMIZE) build config/default > dist/install.yaml

##@ Deployment

ifndef ignore-not-found
  ignore-not-found = false
endif

.PHONY: install
install: manifests kustomize ## Install CRDs into the K8s cluster specified in ~/.kube/config.
	$(KUSTOMIZE) build config/crd | $(KUBECTL) apply -f -

.PHONY: uninstall
uninstall: manifests kustomize ## Uninstall CRDs from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	$(KUSTOMIZE) build config/crd | $(KUBECTL) delete --ignore-not-found=$(ignore-not-found) -f -

.PHONY: deploy
deploy: manifests kustomize ## Deploy controller to the K8s cluster specified in ~/.kube/config.
	cd config/manager && $(KUSTOMIZE) edit set image controller=${IMG}
	$(KUSTOMIZE) build config/default | $(KUBECTL) apply -f -

.PHONY: undeploy
undeploy: kustomize ## Undeploy controller from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	$(KUSTOMIZE) build config/default | $(KUBECTL) delete --ignore-not-found=$(ignore-not-found) -f -

define CTLPTL_CLUSTER_YAML
---
apiVersion: ctlptl.dev/v1alpha1
kind: Registry
name: ctlptl-registry
port: 5005
---
apiVersion: ctlptl.dev/v1alpha1
kind: Cluster
product: kind
registry: ctlptl-registry
kindV1Alpha4Cluster:
  nodes:
  - role: control-plane
    extraMounts:
    - hostPath: /var/run/docker.sock
      containerPath: /var/run/docker.sock
endef

.PHONY: ctlptl-apply
ctlptl-apply: export CTLPTL_CLUSTER_YAML := $(CTLPTL_CLUSTER_YAML)
ctlptl-apply: ctlptl
	$(CTLPTL) apply -f- <<< "$$CTLPTL_CLUSTER_YAML"

.PHONY: ctlptl-delete
ctlptl-delete: export CTLPTL_CLUSTER_YAML := $(CTLPTL_CLUSTER_YAML)
ctlptl-delete: ctlptl
	$(CTLPTL) delete -f- <<< "$$CTLPTL_CLUSTER_YAML"

.PHONY: clusterctl-init
clusterctl-init: clusterctl
	$(CLUSTERCTL) init

.PHONY: one-apply
one-apply: kustomize envsubst kubectl
	$(KUSTOMIZE) build kustomize/v1beta1/default/ | $(ENVSUBST) | $(KUBECTL) apply -f-

.PHONY: one-delete
one-delete: kubectl
	$(KUBECTL) delete cluster/$(CLUSTER_NAME)

.PHONY: one-calico
one-calico: kubectl clusterctl
	$(KUBECTL) --kubeconfig <($(CLUSTERCTL) get kubeconfig $(CLUSTER_NAME)) apply -f \
	https://raw.githubusercontent.com/projectcalico/calico/v$(CALICO_VERSION)/manifests/calico.yaml

##@ Dependencies

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	install -d $(LOCALBIN)

## Tool Binaries
CLUSTERCTL ?= $(LOCALBIN)/clusterctl
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
CTLPTL ?= $(LOCALBIN)/ctlptl
ENVSUBST ?= $(LOCALBIN)/envsubst
ENVTEST ?= $(LOCALBIN)/setup-envtest
GOLANGCI_LINT = $(LOCALBIN)/golangci-lint
KIND ?= $(LOCALBIN)/kind
KUBECTL ?= $(LOCALBIN)/kubectl
KUSTOMIZE ?= $(LOCALBIN)/kustomize

## Tool Versions
CALICO_VERSION ?= 3.26.1
CLUSTERCTL_VERSION ?= 1.8.4
CONTROLLER_TOOLS_VERSION ?= 0.16.1
CTLPTL_VERSION ?= 0.8.34
ENVSUBST_VERSION ?= 1.4.2
ENVTEST_VERSION ?= release-0.19
GOLANGCI_LINT_VERSION ?= 1.59.1
KIND_VERSION ?= 0.24.0
KUBECTL_VERSION ?= 1.31.1
KUSTOMIZE_VERSION ?= 5.4.3

.PHONY: clusterctl
clusterctl: $(CLUSTERCTL)
$(CLUSTERCTL): $(LOCALBIN)
	@[ -f $@-v$(CLUSTERCTL_VERSION) ] || \
	{ curl -fsSL https://github.com/kubernetes-sigs/cluster-api/releases/download/v$(CLUSTERCTL_VERSION)/clusterctl-linux-amd64 \
	| install -m u=rwx,go= -o $(USER) -D /dev/fd/0 $@-v$(CLUSTERCTL_VERSION); }
	@ln -sf $@-v$(CLUSTERCTL_VERSION) $@

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN)
$(CONTROLLER_GEN): $(LOCALBIN)
	$(call go-install-tool,$(CONTROLLER_GEN),sigs.k8s.io/controller-tools/cmd/controller-gen,v$(CONTROLLER_TOOLS_VERSION))

.PHONY: ctlptl
ctlptl: $(CTLPTL)
$(CTLPTL): $(LOCALBIN)
	@[ -f $@-v$(CTLPTL_VERSION) ] || \
	{ curl -fsSL https://github.com/tilt-dev/ctlptl/releases/download/v$(CTLPTL_VERSION)/ctlptl.$(CTLPTL_VERSION).linux.x86_64.tar.gz \
	| tar -xzO -f- ctlptl \
	| install -m u=rwx,go= -o $(USER) -D /dev/fd/0 $@-v$(CTLPTL_VERSION); }
	@ln -sf $@-v$(CTLPTL_VERSION) $@

.PHONY: envsubst
envsubst: $(ENVSUBST)
$(ENVSUBST): $(LOCALBIN)
	$(call go-install-tool,$(ENVSUBST),github.com/a8m/envsubst/cmd/envsubst,v$(ENVSUBST_VERSION))

.PHONY: envtest
envtest: $(ENVTEST)
$(ENVTEST): $(LOCALBIN)
	$(call go-install-tool,$(ENVTEST),sigs.k8s.io/controller-runtime/tools/setup-envtest,$(ENVTEST_VERSION))

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT)
$(GOLANGCI_LINT): $(LOCALBIN)
	$(call go-install-tool,$(GOLANGCI_LINT),github.com/golangci/golangci-lint/cmd/golangci-lint,v$(GOLANGCI_LINT_VERSION))

.PHONY: kind
kind: $(KIND)
$(KIND): $(LOCALBIN)
	@[ -f $@-v$(KIND_VERSION) ] || \
	{ curl -fsSL https://github.com/kubernetes-sigs/kind/releases/download/v$(KIND_VERSION)/kind-linux-amd64 \
	| install -m u=rwx,go= -o $(USER) -D /dev/fd/0 $@-v$(KIND_VERSION); }
	@ln -sf $@-v$(KIND_VERSION) $@

.PHONY: kubectl
kubectl: $(KUBECTL)
$(KUBECTL): $(LOCALBIN)
	@[ -f $@-v$(KUBECTL_VERSION) ] || \
	{ curl -fsSL https://dl.k8s.io/release/v$(KUBECTL_VERSION)/bin/linux/amd64/kubectl \
	| install -m u=rwx,go= -o $(USER) -D /dev/fd/0 $@-v$(KUBECTL_VERSION); }
	@ln -sf $@-v$(KUBECTL_VERSION) $@

.PHONY: kustomize
kustomize: $(KUSTOMIZE)
$(KUSTOMIZE): $(LOCALBIN)
	$(call go-install-tool,$(KUSTOMIZE),sigs.k8s.io/kustomize/kustomize/v5,v$(KUSTOMIZE_VERSION))

# go-install-tool will 'go install' any package with custom target and name of binary, if it doesn't exist
# $1 - target path with name of binary
# $2 - package url which can be installed
# $3 - specific version of package
define go-install-tool
@[ -f "$(1)-$(3)" ] || { \
set -e; \
package=$(2)@$(3) ;\
echo "Downloading $${package}" ;\
rm -f $(1) || true ;\
GOBIN=$(LOCALBIN) go install $${package} ;\
mv $(1) $(1)-$(3) ;\
} ;\
ln -sf $(1)-$(3) $(1)
endef
