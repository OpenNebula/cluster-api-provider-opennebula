SELF := $(patsubst %/,%,$(dir $(abspath $(firstword $(MAKEFILE_LIST)))))
PATH := $(SELF)/bin:$(PATH)

# Setting SHELL to bash allows bash commands to be executed by recipes.
# Options are set to exit when a recipe line exits non-zero or a piped command fails.
SHELL := /usr/bin/env bash -o pipefail
.SHELLFLAGS := -ec

ARTIFACTS_DIR := $(SELF)/_artifacts
BACKUPS_DIR   := $(SELF)/_backups
RELEASES_DIR  := $(SELF)/_releases
CHARTS_DIR    := $(SELF)/_charts

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN := $(shell go env GOPATH)/bin
else
GOBIN := $(shell go env GOBIN)
endif

CLUSTERCTL_VERSION       ?= 1.9.6
CONTROLLER_TOOLS_VERSION ?= 0.17.1
CTLPTL_VERSION           ?= 0.8.38
ENVSUBST_VERSION         ?= 1.4.2
GOLANGCI_LINT_VERSION    ?= 1.63.4
HELM_VERSION             ?= 3.17.3
KIND_VERSION             ?= 0.25.0
KUBECTL_VERSION          ?= 1.31.4
KUSTOMIZE_VERSION        ?= 5.6.0

CLUSTERCTL     := $(SELF)/bin/clusterctl
CONTROLLER_GEN := $(SELF)/bin/controller-gen
CTLPTL         := $(SELF)/bin/ctlptl
ENVSUBST       := $(SELF)/bin/envsubst
GOLANGCI_LINT  := $(SELF)/bin/golangci-lint
HELM           := $(SELF)/bin/helm
KIND           := $(SELF)/bin/kind
KUBECTL        := $(SELF)/bin/kubectl
KUSTOMIZE      := $(SELF)/bin/kustomize

CLOSEST_TAG ?= $(shell git -C $(SELF) describe --tags --abbrev=0)

# Image URL to use for building/pushing image targets
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

# Failsafe in case user doesn't provide it.
CLUSTER_NAME ?= one

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
	$(KUSTOMIZE) build kustomize/v1beta1/rke2-e2e \
	| install -m u=rw,go=r -D /dev/fd/0 $(ARTIFACTS_DIR)/infrastructure/cluster-template.yaml
	go test ./test/e2e/rke2 -v -ginkgo.v

test-e2e-rke2-no-cleanup: docker-build docker-build-e2e $(KUSTOMIZE)
	$(KUSTOMIZE) build kustomize/v1beta1/rke2-e2e \
	| install -m u=rw,go=r -D /dev/fd/0 $(ARTIFACTS_DIR)/infrastructure/cluster-template.yaml
	go test ./test/e2e/rke2 -v -ginkgo.v --args -e2e.skip-resource-cleanup=true

lint: $(GOLANGCI_LINT)
	$(GOLANGCI_LINT) run

lint-fix: $(GOLANGCI_LINT)
	$(GOLANGCI_LINT) run --fix

# Build

.PHONY: build run docker-build docker-push docker-build-e2e docker-release

build: manifests generate fmt vet
	go build -o bin/manager cmd/main.go

run: manifests generate fmt vet
	go run cmd/main.go

docker-build:
	$(CONTAINER_TOOL) build -t $(IMG) .

docker-push: docker-build
	$(CONTAINER_TOOL) push $(IMG)

docker-build-e2e:
	$(CONTAINER_TOOL) build -t $(E2E_IMG) .

# _PLATFORMS defines the target platforms for the manager image be built to provide support to multiple architectures.
# To use this option you need to:
# - be able to use docker buildx (https://docs.docker.com/reference/cli/docker/buildx/)
# - have enabled BuildKit (https://docs.docker.com/build/buildkit/)
# - be able to push the image to your registry
_PLATFORMS ?= linux/amd64,linux/arm64

docker-release:
	-$(CONTAINER_TOOL) buildx create --name cluster-api-provider-opennebula-builder
	$(CONTAINER_TOOL) buildx use cluster-api-provider-opennebula-builder
	$(CONTAINER_TOOL) buildx build --push --platform=$(_PLATFORMS) -t $(IMG_URL):$(CLOSEST_TAG) -t $(IMG_URL):latest -f Dockerfile .
	-$(CONTAINER_TOOL) buildx rm cluster-api-provider-opennebula-builder

# Release

.PHONY: release

release: charts $(KUSTOMIZE)
	# Manifests
	cd config/manager && $(KUSTOMIZE) edit set image controller=$(IMG_URL):$(CLOSEST_TAG)
	$(KUSTOMIZE) build config/default \
	| install -m u=rw,go= -D /dev/fd/0 $(RELEASES_DIR)/$(CLOSEST_TAG)/infrastructure-components.yaml
	# Templates
	## default template (kubeadm)
	$(KUSTOMIZE) build kustomize/v1beta1/default \
	| install -m u=rw,go= -D /dev/fd/0 $(RELEASES_DIR)/$(CLOSEST_TAG)/cluster-template.yaml
	## rke2 template
	$(KUSTOMIZE) build kustomize/v1beta1/rke2 \
	| install -m u=rw,go= -D /dev/fd/0 $(RELEASES_DIR)/$(CLOSEST_TAG)/cluster-template-rke2.yaml
	# Metadata
	install -m u=rw,go= -D metadata.yaml $(RELEASES_DIR)/$(CLOSEST_TAG)/metadata.yaml

# Helm

.PHONY: charts

define chart-generator-tool
charts: $(CHARTS_DIR)/$(CLOSEST_TAG)/$(1)-$(subst v,,$(CLOSEST_TAG)).tgz

$(CHARTS_DIR)/$(CLOSEST_TAG)/$(1)-$(subst v,,$(CLOSEST_TAG)).tgz: $(CHARTS_DIR)/$(CLOSEST_TAG)/$(1) $(HELM)
	$(HELM) package -d $(CHARTS_DIR)/$(CLOSEST_TAG) $(CHARTS_DIR)/$(CLOSEST_TAG)/$(1)

$(CHARTS_DIR)/$(CLOSEST_TAG)/$(1):                     CCM_IMG := {{ .Values.CCM_IMG }}
$(CHARTS_DIR)/$(CLOSEST_TAG)/$(1):              CLUSTER_IMAGES := {{ tpl (toYaml .Values.CLUSTER_IMAGES) . | nindent 4 }}
$(CHARTS_DIR)/$(CLOSEST_TAG)/$(1):           CLUSTER_TEMPLATES := {{ tpl (toYaml .Values.CLUSTER_TEMPLATES) . | nindent 4 }}
$(CHARTS_DIR)/$(CLOSEST_TAG)/$(1):                CLUSTER_NAME := {{ tpl .Values.CLUSTER_NAME . }}
$(CHARTS_DIR)/$(CLOSEST_TAG)/$(1):          CONTROL_PLANE_HOST := {{ .Values.CONTROL_PLANE_HOST }}
$(CHARTS_DIR)/$(CLOSEST_TAG)/$(1): CONTROL_PLANE_MACHINE_COUNT := {{ .Values.CONTROL_PLANE_MACHINE_COUNT }}
$(CHARTS_DIR)/$(CLOSEST_TAG)/$(1):          KUBERNETES_VERSION := {{ .Values.KUBERNETES_VERSION }}
$(CHARTS_DIR)/$(CLOSEST_TAG)/$(1):        MASTER_TEMPLATE_NAME := {{ tpl .Values.MASTER_TEMPLATE_NAME . }}
$(CHARTS_DIR)/$(CLOSEST_TAG)/$(1):                    ONE_AUTH := {{ .Values.ONE_AUTH }}
$(CHARTS_DIR)/$(CLOSEST_TAG)/$(1):                  ONE_XMLRPC := {{ .Values.ONE_XMLRPC }}
$(CHARTS_DIR)/$(CLOSEST_TAG)/$(1):        PRIVATE_NETWORK_NAME := {{ .Values.PRIVATE_NETWORK_NAME }}
$(CHARTS_DIR)/$(CLOSEST_TAG)/$(1):         PUBLIC_NETWORK_NAME := {{ .Values.PUBLIC_NETWORK_NAME }}
$(CHARTS_DIR)/$(CLOSEST_TAG)/$(1):        ROUTER_TEMPLATE_NAME := {{ tpl .Values.ROUTER_TEMPLATE_NAME . }}
$(CHARTS_DIR)/$(CLOSEST_TAG)/$(1):        WORKER_MACHINE_COUNT := {{ .Values.WORKER_MACHINE_COUNT }}
$(CHARTS_DIR)/$(CLOSEST_TAG)/$(1):        WORKER_TEMPLATE_NAME := {{ tpl .Values.WORKER_TEMPLATE_NAME . }}

$(CHARTS_DIR)/$(CLOSEST_TAG)/$(1): $(KUSTOMIZE) $(ENVSUBST)
	install -m u=rwx,go=rx -d $(CHARTS_DIR)/$(CLOSEST_TAG)/$(1)
	cp -rf helm/v1beta1/$(1) $(CHARTS_DIR)/$(CLOSEST_TAG)/.
	$(KUSTOMIZE) build kustomize/v1beta1/$(2) | $(ENVSUBST) \
	| install -m u=rw,go=r -D /dev/fd/0 $(CHARTS_DIR)/$(CLOSEST_TAG)/$(1)/templates/cluster-template.yaml
endef

$(eval $(call chart-generator-tool,capone-kadm,default))
$(eval $(call chart-generator-tool,capone-rke2,rke2))

# Deployment

ifndef ignore-not-found
ignore-not-found = false
endif

.PHONY: install uninstall deploy undeploy logs ctlptl-apply ctlptl-delete

install: manifests $(KUSTOMIZE) $(KUBECTL) # Install CRDs into the K8s cluster specified in ~/.kube/config.
	$(KUSTOMIZE) build config/crd-dev | $(KUBECTL) apply -f-

uninstall: manifests $(KUSTOMIZE) $(KUBECTL) # Uninstall CRDs from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	$(KUSTOMIZE) build config/crd-dev | $(KUBECTL) delete --ignore-not-found=$(ignore-not-found) -f-

deploy: manifests $(KUSTOMIZE) $(KUBECTL) # Deploy controller to the K8s cluster specified in ~/.kube/config.
	cd config/manager && $(KUSTOMIZE) edit set image controller=$(IMG)
	$(KUSTOMIZE) build config/default-dev | $(KUBECTL) apply -f-

undeploy: $(KUSTOMIZE) $(KUBECTL) # Undeploy controller from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	$(KUSTOMIZE) build config/default-dev | $(KUBECTL) delete --ignore-not-found=$(ignore-not-found) -f-

logs: $(KUBECTL)
	$(KUBECTL) -n capone-system logs -f pod/$$($(KUBECTL) -n capone-system get pods -o json \
	| jq -r '[.items[].metadata.name|select(startswith("capone-controller-manager-"))]|first')

ctlptl-apply: $(CTLPTL) $(KIND)
	@kind --version
	$(CTLPTL) apply -f- <<< "$$CTLPTL_CLUSTER_YAML"

ctlptl-delete: $(CTLPTL) $(KIND)
	$(CTLPTL) delete -f- <<< "$$CTLPTL_CLUSTER_YAML"

.PHONY: clusterctl-init clusterctl-init-full clusterctl-init-rke2 clusterctl-init-full-rke2

clusterctl-init: $(CLUSTERCTL)
	$(CLUSTERCTL) --config=clusterctl-config.yaml init

clusterctl-init-full: $(CLUSTERCTL)
	$(CLUSTERCTL) --config=clusterctl-config.yaml init --infrastructure=opennebula:$(CLOSEST_TAG)

clusterctl-init-rke2: _CAPRKE2 := $(if $(CAPRKE2_VERSION),rke2:$(CAPRKE2_VERSION),rke2)
clusterctl-init-rke2: $(CLUSTERCTL)
	$(CLUSTERCTL) --config=clusterctl-config.yaml init  \
	--bootstrap=$(_CAPRKE2) --control-plane=$(_CAPRKE2)

clusterctl-init-full-rke2: _CAPRKE2 := $(if $(CAPRKE2_VERSION),rke2:$(CAPRKE2_VERSION),rke2)
clusterctl-init-full-rke2: $(CLUSTERCTL)
	$(CLUSTERCTL) --config=clusterctl-config.yaml init \
	--bootstrap=$(_CAPRKE2) --control-plane=$(_CAPRKE2) --infrastructure=opennebula:$(CLOSEST_TAG)

.PHONY: $(CLUSTER_NAME)-apply $(CLUSTER_NAME)-apply-vip $(CLUSTER_NAME)-apply-rke2 $(CLUSTER_NAME)-apply-rke2-vip

$(CLUSTER_NAME)-apply: $(KUSTOMIZE) $(ENVSUBST) $(KUBECTL)
	$(KUSTOMIZE) build kustomize/v1beta1/default-dev | $(ENVSUBST) | $(KUBECTL) apply -f-

$(CLUSTER_NAME)-apply-vip: $(KUSTOMIZE) $(ENVSUBST) $(KUBECTL)
	$(KUSTOMIZE) build kustomize/v1beta1/default-vip | $(ENVSUBST) | $(KUBECTL) apply -f-

$(CLUSTER_NAME)-apply-rke2: $(KUSTOMIZE) $(ENVSUBST) $(KUBECTL)
	$(KUSTOMIZE) build kustomize/v1beta1/rke2-dev | $(ENVSUBST) | $(KUBECTL) apply -f-

$(CLUSTER_NAME)-apply-rke2-vip: $(KUSTOMIZE) $(ENVSUBST) $(KUBECTL)
	$(KUSTOMIZE) build kustomize/v1beta1/rke2-vip | $(ENVSUBST) | $(KUBECTL) apply -f-

.PHONY: $(CLUSTER_NAME)-delete $(CLUSTER_NAME)-flannel

$(CLUSTER_NAME)-delete: $(KUBECTL)
	$(KUBECTL) delete cluster/$(CLUSTER_NAME)

$(CLUSTER_NAME)-flannel: $(KUBECTL) $(CLUSTERCTL)
	$(KUBECTL) --kubeconfig <($(CLUSTERCTL) get kubeconfig $(CLUSTER_NAME)) apply -f test/e2e/kubeadm/data/cni/kube-flannel.yml

.PHONY: $(CLUSTER_NAME)-backup $(CLUSTER_NAME)-restore

$(CLUSTER_NAME)-backup: $(CLUSTERCTL)
	rm --preserve-root -rf '$(BACKUPS_DIR)/$(CLUSTER_NAME)'
	install -d $(BACKUPS_DIR)/$(CLUSTER_NAME)
	$(CLUSTERCTL) -v=4 --config=clusterctl-config.yaml move --to-directory=$(BACKUPS_DIR)/$(CLUSTER_NAME)

$(CLUSTER_NAME)-restore: $(BACKUPS_DIR)/$(CLUSTER_NAME)
	$(CLUSTERCTL) -v=4 --config=clusterctl-config.yaml move --from-directory=$(BACKUPS_DIR)/$(CLUSTER_NAME)

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

helm: $(HELM)
$(HELM):
	@[ -f $@-v$(HELM_VERSION) ] || \
	{ curl -fsSL https://get.helm.sh/helm-v$(HELM_VERSION)-linux-amd64.tar.gz \
	| tar -xzO -f- linux-amd64/helm \
	| install -m u=rwx,go= -o $(USER) -D /dev/fd/0 $@-v$(HELM_VERSION); }
	@ln -sf $@-v$(HELM_VERSION) $@

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
