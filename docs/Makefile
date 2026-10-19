# SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
# SPDX-License-Identifier: Apache-2.0

# Docs versioning (mike). See developer-guide/documentation.md and hack/build-docs.sh.
#
# Examples (from the docs/ directory):
#   make build-docs
#   make build-docs PUSH=1
#   make build-docs RELEASES=release-1.1
#   make build-docs RELEASES=release-1.1,master PUSH=1
#
# Or from the repository root:
#   make -C docs build-docs PUSH=1

RELEASES ?= all
PUSH ?= 0

SHELL = /bin/bash

.PHONY: build-docs test-install-gh-pages-404
build-docs:
	../hack/build-docs.sh $(if $(filter 1 true YES yes,$(PUSH)),--push) --releases "$(RELEASES)"

test-install-gh-pages-404:
	../hack/test-install-gh-pages-404.sh

CRD_REF_DOCS_VERSION ?= v0.3.0

# go install honors GOBIN when set, otherwise falls back to $GOPATH/bin.
GO_BIN_DIR := $(shell go env GOBIN)
ifeq ($(GO_BIN_DIR),)
# GOPATH may hold multiple ':'-separated entries; go install uses the first.
GO_BIN_DIR := $(shell go env GOPATH | cut -d: -f1)/bin
$(warning GOBIN is not set; installing crd-ref-docs into $(GO_BIN_DIR))
endif

.PHONY: crd-ref-docs
crd-ref-docs:
	@test -s $(GO_BIN_DIR)/crd-ref-docs && $(GO_BIN_DIR)/crd-ref-docs --version | grep ${CRD_REF_DOCS_VERSION} || \
 	$$(go env GOROOT)/bin/go install -v github.com/elastic/crd-ref-docs@${CRD_REF_DOCS_VERSION}

.PHONY: generate-api-reference
generate-api-reference: crd-ref-docs
	@declare -A api_ref_docs=( \
		[adminpolicybasedroute]=admin-epbr-api-spec.md \
		[clusternetworkconnect]=clusternetworkconnect-api-spec.md \
		[egressfirewall]=egress-firewall-api-spec.md \
		[egressip]=egress-ip-api-spec.md \
		[egressqos]=egress-qos-api-spec.md \
		[egressservice]=egress-service-api-spec.md \
		[observabilityconfig]=observabilityconfig-api-spec.md \
		[routeadvertisements]=routeadvertisements-api-spec.md \
		[uplink]=uplink-api-spec.md \
		[userdefinednetwork]=userdefinednetwork-api-spec.md \
		[vtep]=vtep-api-spec.md \
	); \
	for src in "$${!api_ref_docs[@]}"; do \
		out="$${api_ref_docs[$$src]}"; \
		echo "Generating docs/api-reference/$$out from go-controller/pkg/crd/$$src"; \
		$(GO_BIN_DIR)/crd-ref-docs --source-path ../go-controller/pkg/crd/$$src \
			--config=../crd-docs-config.yaml \
			--renderer=markdown \
			--output-path=./api-reference/$$out \
			|| exit 1; \
	done
