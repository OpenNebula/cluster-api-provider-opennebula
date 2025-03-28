SELF := $(patsubst %/,%,$(dir $(abspath $(firstword $(MAKEFILE_LIST)))))
PATH := $(SELF)/bin:$(PATH)

# Setting SHELL to bash allows bash commands to be executed by recipes.
# Options are set to exit when a recipe line exits non-zero or a piped command fails.
SHELL := /usr/bin/env bash -o pipefail
.SHELLFLAGS := -ec

ARTIFACTS_DIR := $(SELF)/_artifacts
RELEASES_DIR  := $(SELF)/_releases

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN := $(shell go env GOPATH)/bin
else
GOBIN := $(shell go env GOBIN)
endif

CLUSTERCTL_VERSION       ?= 1.9.4
CONTROLLER_TOOLS_VERSION ?= 0.17.1
CTLPTL_VERSION           ?= 0.8.38
ENVSUBST_VERSION         ?= 1.4.2
GOLANGCI_LINT_VERSION    ?= 1.63.4
KIND_VERSION             ?= 0.25.0
KUBECTL_VERSION          ?= 1.31.4
KUSTOMIZE_VERSION        ?= 5.6.0

CLUSTERCTL     := $(SELF)/bin/clusterctl
CONTROLLER_GEN := $(SELF)/bin/controller-gen
CTLPTL         := $(SELF)/bin/ctlptl
ENVSUBST       := $(SELF)/bin/envsubst
GOLANGCI_LINT  := $(SELF)/bin/golangci-lint
KIND           := $(SELF)/bin/kind
KUBECTL        := $(SELF)/bin/kubectl
KUSTOMIZE      := $(SELF)/bin/kustomize

CLOSEST_TAG ?= $(shell git -C $(SELF) describe --tags --abbrev=0)

# Image URL to use all building/pushing image targets
IMG_URL ?= ghcr.io/opennebula/cluster-api-provider-opennebula
IMG     ?= $(IMG_URL):latest
E2E_IMG ?= $(IMG_URL):e2e

# CONTAINER_TOOL defines the container tool to be used for building images.
# Be aware that the target commands are only tested with Docker which is
# scaffolded by default. However, you might want to replace it to use other
# tools. (i.e. podman)
CONTAINER_TOOL ?= docker

define CTLPTL_CLUSTER_YAML
---
apiVersion: ctlptl.dev/v1alpha1
kind: Registry
name: ctlptl-registry
port: 5005
listenAddress: 0.0.0.0
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

-include .env
export

.PHONY: all clean

all: build

clean:
	rm --preserve-root -rf '$(SELF)/bin/'
	rm --preserve-root -rf '$(ARTIFACTS_DIR)'
	rm --preserve-root -rf '$(RELEASES_DIR)'

# Development

.PHONY: manifests generate fmt vet test-e2e test-e2e-no-cleanup test-e2e-rke2 test-e2e-rke2-no-cleanup lint lint-fix

manifests: $(CONTROLLER_GEN) # Generate WebhookConfiguration, ClusterRole and CustomResourceDefinition objects.
	$(CONTROLLER_GEN) rbac:roleName=manager-role crd webhook paths="./..." output:crd:artifacts:config=config/crd/bases

generate: $(CONTROLLER_GEN) # Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./..."

fmt:
	go fmt ./...

vet:
	go vet ./...

test-e2e: docker-build docker-build-e2e $(KUSTOMIZE)
	$(KUSTOMIZE) build kustomize/v1beta1/default-e2e \
	| install -m u=rw,go=r -D /dev/fd/0 $(ARTIFACTS_DIR)/infrastructure/cluster-template.yaml
	go test ./test/e2e/kubeadm -v -ginkgo.v

test-e2e-no-cleanup: docker-build docker-build-e2e $(KUSTOMIZE)
	$(KUSTOMIZE) build kustomize/v1beta1/default-e2e \
	| install -m u=rw,go=r -D /dev/fd/0 $(ARTIFACTS_DIR)/infrastructure/cluster-template.yaml
	go test ./test/e2e/kubeadm -v -ginkgo.v --args -e2e.skip-resource-cleanup=true

test-e2e-rke2: docker-build docker-build-e2e $(KUSTOMIZE)
	$(KUSTOMIZE) build kustomize/v1beta1/default-e2e-rke2 \
	| install -m u=rw,go=r -D /dev/fd/0 $(ARTIFACTS_DIR)/infrastructure/cluster-template.yaml
	go test ./test/e2e/rke2 -v -ginkgo.v

test-e2e-rke2-no-cleanup: docker-build docker-build-e2e $(KUSTOMIZE)
	$(KUSTOMIZE) build kustomize/v1beta1/default-e2e-rke2 \
	| install -m u=rw,go=r -D /dev/fd/0 $(ARTIFACTS_DIR)/infrastructure/cluster-template.yaml
	go test ./test/e2e/rke2 -v -ginkgo.v --args -e2e.skip-resource-cleanup=true

lint: $(GOLANGCI_LINT)
	$(GOLANGCI_LINT) run

lint-fix: $(GOLANGCI_LINT)
	$(GOLANGCI_LINT) run --fix

# Build

.PHONY: build run docker-build docker-push docker-build-e2e

build: manifests generate fmt vet
	go build -o bin/manager cmd/main.go

run: manifests generate fmt vet
	go run cmd/main.go

docker-build:
	$(CONTAINER_TOOL) build -t $(IMG) .

docker-push:
	$(CONTAINER_TOOL) push $(IMG)

docker-build-e2e:
	$(CONTAINER_TOOL) build -t $(E2E_IMG) .

# Release

.PHONY: release

release: $(KUSTOMIZE)
	# Manifests
	cd config/manager && $(KUSTOMIZE) edit set image controller=$(IMG_URL):$(CLOSEST_TAG)
	$(KUSTOMIZE) build config/default \
	| install -m u=rw,go= -D /dev/fd/0 $(RELEASES_DIR)/$(CLOSEST_TAG)/infrastructure-components.yaml
	# Templates
	$(KUSTOMIZE) build kustomize/v1beta1/default \
	| install -m u=rw,go= -D /dev/fd/0 $(RELEASES_DIR)/$(CLOSEST_TAG)/cluster-template.yaml
	# Metadata
	install -m u=rw,go= -D metadata.yaml $(RELEASES_DIR)/$(CLOSEST_TAG)/metadata.yaml

# Deployment

ifndef ignore-not-found
ignore-not-found = false
endif

.PHONY: install uninstall deploy undeploy logs ctlptl-apply ctlptl-delete \
        clusterctl-init clusterctl-init-full one-apply one-delete one-flannel

install: manifests $(KUSTOMIZE) $(KUBECTL) # Install CRDs into the K8s cluster specified in ~/.kube/config.
	$(KUSTOMIZE) build config/crd | $(KUBECTL) apply -f-

uninstall: manifests $(KUSTOMIZE) $(KUBECTL) # Uninstall CRDs from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	$(KUSTOMIZE) build config/crd | $(KUBECTL) delete --ignore-not-found=$(ignore-not-found) -f-

deploy: manifests $(KUSTOMIZE) $(KUBECTL) # Deploy controller to the K8s cluster specified in ~/.kube/config.
	cd config/manager && $(KUSTOMIZE) edit set image controller=$(IMG)
	$(KUSTOMIZE) build config/default | $(KUBECTL) apply -f-

undeploy: $(KUSTOMIZE) $(KUBECTL) # Undeploy controller from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	$(KUSTOMIZE) build config/default | $(KUBECTL) delete --ignore-not-found=$(ignore-not-found) -f-

logs: $(KUBECTL)
	$(KUBECTL) -n capone-system logs -f pod/$$($(KUBECTL) -n capone-system get pods -o json \
	| jq -r '[.items[].metadata.name|select(startswith("capone-controller-manager-"))]|first')

ctlptl-apply: $(CTLPTL) $(KIND)
	@kind --version
	$(CTLPTL) apply -f- <<< "$$CTLPTL_CLUSTER_YAML"

ctlptl-delete: $(CTLPTL) $(KIND)
	$(CTLPTL) delete -f- <<< "$$CTLPTL_CLUSTER_YAML"

clusterctl-init: $(CLUSTERCTL)
	$(CLUSTERCTL) init

clusterctl-init-full: $(CLUSTERCTL)
	$(CLUSTERCTL) init --config=clusterctl-config.yaml --infrastructure=opennebula:$(CLOSEST_TAG)

clusterctl-init-full-rke2: $(CLUSTERCTL)
	$(CLUSTERCTL) init --config=clusterctl-config.yaml --bootstrap=rke2:v0.12.0 --control-plane=rke2:v0.12.0 --infrastructure=opennebula:$(CLOSEST_TAG)

one-apply: $(KUSTOMIZE) $(ENVSUBST) $(KUBECTL)
	$(KUSTOMIZE) build kustomize/v1beta1/default-dev | $(ENVSUBST) | $(KUBECTL) apply -f-

one-apply-rke2: $(KUSTOMIZE) $(ENVSUBST) $(KUBECTL)
	$(KUSTOMIZE) build kustomize/v1beta1/default-dev-rke2 | $(ENVSUBST) | $(KUBECTL) apply -f-

one-delete: $(KUBECTL)
	$(KUBECTL) delete cluster/$(CLUSTER_NAME)

one-flannel: $(KUBECTL) $(CLUSTERCTL)
	$(KUBECTL) --kubeconfig <($(CLUSTERCTL) get kubeconfig $(CLUSTER_NAME)) apply -f test/e2e/data/cni/kube-flannel.yml

# Dependencies

.PHONY: clusterctl controller-gen ctlptl envsubst golangci-lint kind kubectl kustomize

clusterctl: $(CLUSTERCTL)
$(CLUSTERCTL):
	@[ -f $@-v$(CLUSTERCTL_VERSION) ] || \
	{ curl -fsSL https://github.com/kubernetes-sigs/cluster-api/releases/download/v$(CLUSTERCTL_VERSION)/clusterctl-linux-amd64 \
	| install -m u=rwx,go= -o $(USER) -D /dev/fd/0 $@-v$(CLUSTERCTL_VERSION); }
	@ln -sf $@-v$(CLUSTERCTL_VERSION) $@

controller-gen: $(CONTROLLER_GEN)
$(CONTROLLER_GEN):
	$(call go-install-tool,$(CONTROLLER_GEN),sigs.k8s.io/controller-tools/cmd/controller-gen,v$(CONTROLLER_TOOLS_VERSION))

ctlptl: $(CTLPTL)
$(CTLPTL):
	@[ -f $@-v$(CTLPTL_VERSION) ] || \
	{ curl -fsSL https://github.com/tilt-dev/ctlptl/releases/download/v$(CTLPTL_VERSION)/ctlptl.$(CTLPTL_VERSION).linux.x86_64.tar.gz \
	| tar -xzO -f- ctlptl \
	| install -m u=rwx,go= -o $(USER) -D /dev/fd/0 $@-v$(CTLPTL_VERSION); }
	@ln -sf $@-v$(CTLPTL_VERSION) $@

envsubst: $(ENVSUBST)
$(ENVSUBST):
	$(call go-install-tool,$(ENVSUBST),github.com/a8m/envsubst/cmd/envsubst,v$(ENVSUBST_VERSION))

golangci-lint: $(GOLANGCI_LINT)
$(GOLANGCI_LINT):
	$(call go-install-tool,$(GOLANGCI_LINT),github.com/golangci/golangci-lint/cmd/golangci-lint,v$(GOLANGCI_LINT_VERSION))

kind: $(KIND)
$(KIND):
	@[ -f $@-v$(KIND_VERSION) ] || \
	{ curl -fsSL https://github.com/kubernetes-sigs/kind/releases/download/v$(KIND_VERSION)/kind-linux-amd64 \
	| install -m u=rwx,go= -o $(USER) -D /dev/fd/0 $@-v$(KIND_VERSION); }
	@ln -sf $@-v$(KIND_VERSION) $@

kubectl: $(KUBECTL)
$(KUBECTL):
	@[ -f $@-v$(KUBECTL_VERSION) ] || \
	{ curl -fsSL https://dl.k8s.io/release/v$(KUBECTL_VERSION)/bin/linux/amd64/kubectl \
	| install -m u=rwx,go= -o $(USER) -D /dev/fd/0 $@-v$(KUBECTL_VERSION); }
	@ln -sf $@-v$(KUBECTL_VERSION) $@

kustomize: $(KUSTOMIZE)
$(KUSTOMIZE):
	$(call go-install-tool,$(KUSTOMIZE),sigs.k8s.io/kustomize/kustomize/v5,v$(KUSTOMIZE_VERSION))

# go-install-tool will 'go install' any package with custom target and name of binary, if it doesn't exist
# $1 - target path with name of binary
# $2 - package url which can be installed
# $3 - specific version of package
define go-install-tool
@[ -f "$(1)-$(3)" ] || { \
set -e; \
package=$(2)@$(3); \
echo "Downloading $${package}"; \
rm -f $(1) ||:; \
GOBIN=$(SELF)/bin go install $${package}; \
mv $(1) $(1)-$(3); \
}; \
ln -sf $(1)-$(3) $(1)
endef
