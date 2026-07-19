SHELL := /bin/sh

BINARY := bin/lazarus
VERSION ?= dev
COMMIT ?= $(shell git rev-parse HEAD 2>/dev/null || printf unknown)
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
SOURCE ?= https://github.com/jayk56/lazarus
IMAGE ?= lazarus:dev
HELM_CHART := deploy/helm/lazarus
HELM_CI_VALUES := $(HELM_CHART)/ci-values.yaml
HELM_AKS_VALUES := deploy/aks/values.test.yaml
HELM_OCP_VALUES := deploy/openshift/values.production.example.yaml
HELM_TEST_DIGEST := sha256:0000000000000000000000000000000000000000000000000000000000000000
AWX_E2E_DIR := tests/e2e/awx
AWX_E2E_PLAYBOOKS := $(wildcard $(AWX_E2E_DIR)/playbooks/*.yml)

.PHONY: all build test test-race vet fmt-check check validate-source container helm-lint helm-template helm-digest-negative yaml-check openapi-check ansible-check aap-syntax-check awx-syntax-check awx-e2e-contract-check shell-check workflow-check clean

all: check build

build:
	@mkdir -p bin
	CGO_ENABLED=0 go build -trimpath \
		-ldflags "-s -w -buildid= -X main.version=$(VERSION)" \
		-o $(BINARY) ./cmd/lazarus

test:
	go test -count=1 ./...

test-race:
	go test -race -count=1 ./...

vet:
	go vet ./...

fmt-check:
	@test -z "$$(gofmt -l .)" || { gofmt -l .; exit 1; }

check: fmt-check vet test

validate-source: check test-race helm-lint helm-template helm-digest-negative yaml-check openapi-check ansible-check awx-e2e-contract-check shell-check workflow-check

container:
	docker build \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		--build-arg BUILD_DATE=$(BUILD_DATE) \
		--build-arg SOURCE=$(SOURCE) \
		-t $(IMAGE) .

helm-lint:
	helm lint $(HELM_CHART) -f $(HELM_CI_VALUES)
	helm lint $(HELM_CHART) -f $(HELM_AKS_VALUES)
	helm lint $(HELM_CHART) -f $(HELM_OCP_VALUES) \
		--set-string image.digest=$(HELM_TEST_DIGEST)

helm-template:
	helm template lazarus $(HELM_CHART) -f $(HELM_CI_VALUES) | python3 scripts/validate-yaml.py -
	helm template lazarus $(HELM_CHART) -f $(HELM_AKS_VALUES) | python3 scripts/validate-yaml.py -
	helm template lazarus $(HELM_CHART) -f $(HELM_OCP_VALUES) \
		--set-string image.digest=$(HELM_TEST_DIGEST) | python3 scripts/validate-yaml.py -

helm-digest-negative:
	@if helm template lazarus $(HELM_CHART) -f $(HELM_CI_VALUES) \
		--set-string image.digest=sha256:not-a-valid-digest >/dev/null 2>&1; then \
		echo "Helm accepted an invalid image.digest" >&2; \
		exit 1; \
	fi

yaml-check:
	python3 scripts/validate-yaml.py

openapi-check:
	python3 -m openapi_spec_validator api/openapi.yaml

ansible-check:
	@for playbook in examples/ansible/*.yml $(AWX_E2E_PLAYBOOKS); do \
		ansible-playbook --syntax-check -i $(AWX_E2E_DIR)/inventory.example.yml "$$playbook"; \
	done

# Run this additional gate inside an AAP execution environment that contains
# ansible.controller and the collections in examples/aap/collections.
aap-syntax-check:
	ansible-playbook --syntax-check examples/aap/configure.yml
	ansible-playbook --syntax-check -i $(AWX_E2E_DIR)/inventory.example.yml $(AWX_E2E_DIR)/configure.yml

# Install tests/e2e/awx/collections/requirements.yml into .local/awx-collections
# before running this stock-AWX-specific syntax gate.
awx-syntax-check:
	ANSIBLE_COLLECTIONS_PATH=$(CURDIR)/.local/awx-collections ansible-playbook --syntax-check -i $(AWX_E2E_DIR)/inventory.example.yml $(AWX_E2E_DIR)/configure-awx.yml

awx-e2e-contract-check:
	python3 $(AWX_E2E_DIR)/validate_contract.py

shell-check:
	shellcheck scripts/*.sh $(AWX_E2E_DIR)/fixture-image/*.sh

workflow-check:
	actionlint

clean:
	/usr/bin/trash $(BINARY) 2>/dev/null || true
