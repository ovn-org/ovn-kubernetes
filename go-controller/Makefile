# SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
# SPDX-License-Identifier: Apache-2.0

OUT_DIR = _output
export OUT_DIR
PREFIX ?= ${DESTDIR}/usr
BINDIR ?= ${PREFIX}/bin
CNIBINDIR ?= ${DESTDIR}/opt/cni/bin
GCFLAGS ?=
export GCFLAGS
LDFLAGS ?=
export LDFLAGS
PKGS ?=
GOPATH ?= $(shell go env GOPATH)
TEST_REPORT_DIR?=$(CURDIR)/_artifacts
export TEST_REPORT_DIR
GO_VERSION ?= 1.26
GO_DOCKER_IMG = quay.io/projectquay/golang:${GO_VERSION}
# CONTAINER_RUNNABLE determines if the tests can be run inside a container. It checks to see if
# podman/docker is installed on the system.
PODMAN ?= $(shell podman -v > /dev/null 2>&1; echo $$?)
ifeq ($(PODMAN), 0)
CONTAINER_RUNTIME?=podman
else
CONTAINER_RUNTIME?=docker
endif
CONTAINER_RUNNABLE ?= $(shell $(CONTAINER_RUNTIME) -v > /dev/null 2>&1; echo $$?)
OVN_SCHEMA_VERSION ?= v26.03.1
OVS_VERSION ?= v2.17.0
ifeq ($(NOROOT),TRUE)
C_ARGS = -e NOROOT=TRUE
else
C_ARGS = --cap-add=NET_ADMIN --cap-add=SYS_ADMIN --privileged
endif

## Tool Binaries
TOOLS_OUTPUT_DIR = ${CURDIR}/${OUT_DIR}
MOCKERY = ${TOOLS_OUTPUT_DIR}/mockery-${MOCKERY_VERSION}

## Tool Versions
MOCKERY_VERSION ?= v2.53.4

export NOROOT

.PHONY: all build check test bench

# Example:
#   make
#   make all
#   make all GCFLAGS=all="-N -l"
#       (disables compiler optimization and inlining to aid source debugging tools
#        like delve)
#   make all LDFLAGS="-w -s"
#       (disables symbol table and DWARF generation when building ovnk binaries)

all build:
	hack/build-go.sh cmd/ovnkube cmd/ovn-k8s-cni-overlay cmd/ovn-kube-util hybrid-overlay/cmd/hybrid-overlay-node cmd/ovnkube-trace cmd/ovnkube-identity cmd/ovnkube-observ

windows:
	WINDOWS_BUILD="yes" hack/build-go.sh hybrid-overlay/cmd/hybrid-overlay-node

# Run the Go benchmarks. Packages are discovered rather than listed so a new
# benchmark is picked up without touching this file.
#
# BENCHTIME=10x keeps a CI smoke run fast; leave it unset locally to get
# meaningful numbers.
BENCH_PKGS = $(shell grep -rl '^func Benchmark' --include='*_test.go' pkg/ hybrid-overlay/ 2>/dev/null | xargs -r -n1 dirname | sort -u | sed 's|^|./|')
BENCHTIME ?=
bench:
	@test -n "$(BENCH_PKGS)" || { echo "no benchmarks found"; exit 1; }
	go test -run='^$$' -bench=. -benchmem $(if $(BENCHTIME),-benchtime=$(BENCHTIME),) $(BENCH_PKGS)

# Note: `--security-opt label=disable` option is required on systems where SELinux is enabled for `podman` to successfully run the
# tests in a container. Refer: https://www.projectatomic.io/blog/2016/03/dwalsh_selinux_containers/ for additional context
check test:
ifeq ($(CONTAINER_RUNNABLE), 0)
	$(CONTAINER_RUNTIME) run -it --rm --security-opt label=disable ${C_ARGS} -v $(shell dirname $(PWD)):/go/src/github.com/ovn-kubernetes/ovn-kubernetes -w /go/src/github.com/ovn-kubernetes/ovn-kubernetes/go-controller -e COVERALLS=${COVERALLS} -e GINKGO_FOCUS="${GINKGO_FOCUS}" $(GO_DOCKER_IMG) sh -c "RACE=1 DOCKER_TEST=1 COVERALLS=${COVERALLS} PKGS="${PKGS}" hack/test-go.sh focus \"${GINKGO_FOCUS}\" "
else
	RACE=1 hack/test-go.sh
endif

modelgen: pkg/nbdb/ovn-nb.ovsschema pkg/sbdb/ovn-sb.ovsschema pkg/vswitchd/vswitch.ovsschema
	hack/update-modelgen.sh

codegen:
	hack/update-codegen.sh

.PHONY: verify-codegen
verify-codegen:
	@GOPATH=${GOPATH} ./hack/verify-codegen.sh || { echo "codegen verification failed! Run 'make codegen' and commit the result"; exit 1; }

.PHONY: mocksgen
mocksgen: ${MOCKERY}
	${MOCKERY}

.PHONY: verify-mocksgen
verify-mocksgen:
	@GOPATH=${GOPATH} ./hack/verify-mocksgen.sh || { echo "mocksgen verification failed! Run 'make mocksgen' and commit the result"; exit 1; }

install:
	install -D -m 755 ${OUT_DIR}/go/bin/ovnkube ${BINDIR}/
	install -D -m 755 ${OUT_DIR}/go/bin/ovn-kube-util ${BINDIR}/
	install -D -m 755 ${OUT_DIR}/go/bin/ovn-k8s-cni-overlay -t ${CNIBINDIR}/

clean:
	rm -rf ${OUT_DIR}
	rm -rf ${TEST_REPORT_DIR}
	rm -f ./pkg/nbdb/ovn-nb.ovsschema
	rm -f ./pkg/sbdb/ovn-sb.ovsschema
	rm -f ./pkg/vswitchd/vswitch.ovsschema

.PHONY: lint gofmt verify-go-mod-vendor third-party-licenses verify-third-party-licenses

# Absolute path to this Makefile's directory (go-controller/), so lint works when invoked
# via `make -f <path-to-go-controller-dir>/Makefile lint` from another directory.
GO_CONTROLLER_ROOT := $(dir $(abspath $(lastword $(MAKEFILE_LIST))))
LINT_SH := $(GO_CONTROLLER_ROOT)hack/lint.sh

lint:
ifeq ($(CONTAINER_RUNNABLE), 0)
	@GOPATH=${GOPATH} $(LINT_SH) $(CONTAINER_RUNTIME) || { echo "lint failed! Try running 'make lint-fix'"; exit 1; }
else
	echo "no container runtime. attempting to run natively";
	@GOPATH=${GOPATH} $(LINT_SH) run-natively || { echo "running lint locally failed!"; exit 1; }
endif

lint-fix:
ifeq ($(CONTAINER_RUNNABLE), 0)
	@GOPATH=${GOPATH} $(LINT_SH) $(CONTAINER_RUNTIME) fix || { echo "ERROR: lint fix failed! There is a bug that changes file ownership to root \
	when this happens. To fix it, simply run 'chown -R <user>:<group> *' from the repo root."; exit 1; }
else
	echo "no container runtime. attempting to run natively w/ -fix flag";
	@GOPATH=${GOPATH} $(LINT_SH) run-natively fix || { echo "running lint fix locally failed! There is a bug that changes file ownership to root \
	when this happens. To fix it, simply run 'chown -R <user>:<group> *' from the repo root."; exit 1; }
endif

gofmt:
ifeq ($(CONTAINER_RUNNABLE), 0)
	$(CONTAINER_RUNTIME) run -it --rm --security-opt label=disable -v $(shell dirname $(PWD)):/go/src/github.com/ovn-kubernetes/ovn-kubernetes -w /go/src/github.com/ovn-kubernetes/ovn-kubernetes/go-controller $(GO_DOCKER_IMG) hack/verify-gofmt.sh
else
	@./hack/verify-gofmt.sh
endif

verify-go-mod-vendor:
ifeq ($(CONTAINER_RUNNABLE), 0)
	@GOPATH=${GOPATH} ./hack/verify-go-mod-vendor.sh
else
	@./hack/verify-go-mod-vendor.sh
endif

third-party-licenses:
	@GOPATH=${GOPATH} ./hack/update-third-party-licenses.sh || { echo "third-party license generation failed!"; exit 1; }

verify-third-party-licenses:
	@GOPATH=${GOPATH} ./hack/verify-third-party-licenses.sh || { echo "third-party license verification failed!"; exit 1; }

pkg/nbdb/ovn-nb.ovsschema:
	curl -sSL https://raw.githubusercontent.com/ovn-org/ovn/$(OVN_SCHEMA_VERSION)/ovn-nb.ovsschema -o $@

pkg/sbdb/ovn-sb.ovsschema:
	curl -sSL https://raw.githubusercontent.com/ovn-org/ovn/$(OVN_SCHEMA_VERSION)/ovn-sb.ovsschema -o $@

pkg/vswitchd/vswitch.ovsschema:
	curl -sSL https://raw.githubusercontent.com/openvswitch/ovs/${OVS_VERSION}/vswitchd/vswitch.ovsschema -o $@

${TOOLS_OUTPUT_DIR}:
	mkdir -p ${TOOLS_OUTPUT_DIR}

.PHONY: mockery
mockery: ${MOCKERY} ## Download mockery locally if necessary.
${MOCKERY}: ${TOOLS_OUTPUT_DIR}
	$(call go-install-tool,${MOCKERY},github.com/vektra/mockery/v2,${MOCKERY_VERSION})


# go-install-tool will 'go install' any package with custom target and name of binary, if it doesn't exist
# $1 - target path with name of binary (ideally with version)
# $2 - package url which can be installed
# $3 - specific version of package
define go-install-tool
@[ -f $(1) ] || { \
set -xe; \
package=$(2)@$(3) ;\
echo "Downloading $${package}" ;\
GOBIN=${TOOLS_OUTPUT_DIR} go install $${package} ;\
mv "$$(echo "$(1)" | sed "s/-$(3)$$//")" $(1) ;\
}
endef
