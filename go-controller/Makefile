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
GO_VERSION ?= 1.22.0
GO_DOCKER_IMG = quay.io/giantswarm/golang:${GO_VERSION}
# CONTAINER_RUNNABLE determines if the tests can be run inside a container. It checks to see if
# podman/docker is installed on the system.
PODMAN ?= $(shell podman -v > /dev/null 2>&1; echo $$?)
ifeq ($(PODMAN), 0)
CONTAINER_RUNTIME=podman
else
CONTAINER_RUNTIME=docker
endif
CONTAINER_RUNNABLE ?= $(shell $(CONTAINER_RUNTIME) -v > /dev/null 2>&1; echo $$?)
# FIXME(tssurya): In one week when OVN 24.09 is released change the schema version
OVN_SCHEMA_VERSION ?= 8efac26f6637fc
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
MOCKERY_VERSION ?= v2.43.2

export NOROOT

.PHONY: all build check test

# Example:
#   make
#   make all
#   make all GCFLAGS=all="-N -l"
#       (disables compiler optimization and inlining to aid source debugging tools
#        like delve)
#   make all LDFLAGS="-w -s"
#       (disables symbol table and DWARF generation when building ovnk binaries)

all build:
	hack/build-go.sh cmd/ovnkube cmd/ovn-k8s-cni-overlay cmd/ovn-kube-util hybrid-overlay/cmd/hybrid-overlay-node cmd/ovndbchecker cmd/ovnkube-trace cmd/ovnkube-identity cmd/ovnkube-observ

windows:
	WINDOWS_BUILD="yes" hack/build-go.sh hybrid-overlay/cmd/hybrid-overlay-node

# Note: `--security-opt label=disable` option is required on systems where SELinux is enabled for `podman` to successfully run the
# tests in a container. Refer: https://www.projectatomic.io/blog/2016/03/dwalsh_selinux_containers/ for additional context
check test:
ifeq ($(CONTAINER_RUNNABLE), 0)
	$(CONTAINER_RUNTIME) run -it --rm --security-opt label=disable ${C_ARGS} -v $(shell dirname $(PWD)):/go/src/github.com/ovn-org/ovn-kubernetes -w /go/src/github.com/ovn-org/ovn-kubernetes/go-controller -e COVERALLS=${COVERALLS} -e GINKGO_FOCUS="${GINKGO_FOCUS}" $(GO_DOCKER_IMG) sh -c "RACE=1 DOCKER_TEST=1 COVERALLS=${COVERALLS} PKGS="${PKGS}" hack/test-go.sh focus \"${GINKGO_FOCUS}\" "
else
	RACE=1 hack/test-go.sh focus "${GINKGO_FOCUS}"
endif

modelgen: pkg/nbdb/ovn-nb.ovsschema pkg/sbdb/ovn-sb.ovsschema pkg/vswitchd/vswitch.ovsschema
	hack/update-modelgen.sh

codegen:
	hack/update-codegen.sh

.PHONY: mocksgen
mocksgen: ${MOCKERY}
	${MOCKERY}

install:
	install -D -m 755 ${OUT_DIR}/go/bin/ovnkube ${BINDIR}/
	install -D -m 755 ${OUT_DIR}/go/bin/ovn-kube-util ${BINDIR}/
	install -D -m 755 ${OUT_DIR}/go/bin/ovn-k8s-cni-overlay -t ${CNIBINDIR}/
	install -D -m 755 ${OUT_DIR}/go/bin/ovndbchecker ${BINDIR}/

clean:
	rm -rf ${OUT_DIR}
	rm -rf ${TEST_REPORT_DIR}
	rm -f ./pkg/nbdb/ovn-nb.ovsschema
	rm -f ./pkg/sbdb/ovn-sb.ovsschema
	rm -f ./pkg/vswitchd/vswitch.ovsschema

.PHONY: lint gofmt verify-go-mod-vendor

lint:
ifeq ($(CONTAINER_RUNNABLE), 0)
	@GOPATH=${GOPATH} ./hack/lint.sh $(CONTAINER_RUNTIME)
else
	echo "linter can only be run within a container since it needs a specific golangci-lint version"
endif

gofmt:
ifeq ($(CONTAINER_RUNNABLE), 0)
	$(CONTAINER_RUNTIME) run -it --rm --security-opt label=disable -v $(shell dirname $(PWD)):/go/src/github.com/ovn-org/ovn-kubernetes -w /go/src/github.com/ovn-org/ovn-kubernetes/go-controller $(GO_DOCKER_IMG) hack/verify-gofmt.sh
else
	@./hack/verify-gofmt.sh
endif

verify-go-mod-vendor:
ifeq ($(CONTAINER_RUNNABLE), 0)
	@GOPATH=${GOPATH} ./hack/verify-go-mod-vendor.sh
else
	@./hack/verify-go-mod-vendor.sh
endif

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


# ================================================================================
# Unit tests

# Function to retrieve all packages with test files from $1
go-get-test-pkg = $(shell go list -buildvcs=false -mod vendor -f '{{if len .TestGoFiles}} {{.Dir}} {{end}}' $1 | xargs)
ifndef PKGS
PKGS = $(call go-get-test-pkg,./cmd/... ./pkg/... ./hybrid-overlay/...)
else
PKGS := $(call go-get-test-pkg,$(PKGS))
endif

remove-package-prefix = $(subst $(CURDIR)/,,$1)
make-pkg-name = $(subst /,_,$(call remove-package-prefix,$1))
make-ci-test-target = $(call make-pkg-name,$1)-ci_check
make-ci-test-binary = $(CURDIR)/_output/$(call make-pkg-name,$1).test

# List of all unit test targets
all-package-ci-check-tagets = $(foreach pkg,$(PKGS),$(call make-ci-test-target,$(pkg)))
all-package-ci-check-binaries = $(foreach pkg,$(PKGS),$(call make-ci-test-binary,$(pkg)))

.PHONY: ci-check
ci-check: $(all-package-ci-check-tagets)

# packages that relays on order of execution:
seed-pkg_node = 1

# packages requires root to run test:
ci-requires-root = pkg/controllermanager \
                   pkg/node \
                   pkg/node/iptables \
                   pkg/node/rulemanager \
	           pkg/node/routemanager \
                   pkg/node/vrfmanager \
                   pkg/node/controllers/egressip

ci-gotest = pkg/allocator/bitmap \
            pkg/allocator/ip \
            pkg/csrapprover \
            pkg/generator/udn \
            pkg/kube/healthcheck \
	    pkg/libovsdb/ops \
            pkg/libovsdb/util \
            pkg/networkmanager \
            pkg/node/ovspinning \
            pkg/ovn/controller/udnenabledsvc \
            pkg/ovn/gateway \
            pkg/ovn/gatewayrouter \
            pkg/util/errors \
            pkg/util/batching \
            pkg/ovndbmanager \
            pkg/ovnwebhook

ci-parallel-run = pkg/ovn \
                  pkg/ovn/address_set \
                  pkg/clustermanager \
                  pkg/clustermanager/status_manager/zone_tracker \
                  pkg/clustermanager/endpointslicemirror \
                  pkg/factory \
                  pkg/informer \
                  pkg/config

check-ci-non-parallel = $(filter $(call remove-package-prefix,$1),$(ci-non-parallel))
check-ci-requires-root = $(filter $(call remove-package-prefix,$1),$(ci-requires-root))
check-ci-parallel = $(filter $(call remove-package-prefix,$1),$(ci-parallel-run))

user-is-not-root = $(filter-out root,$1)

maybe-add-seed = $(if $(seed-$(call make-pkg-name,$1)),--seed $(seed-$(call make-pkg-name,$1)))
ifeq ($(NOROOT),TRUE)
maybe-add-sudo =
else
maybe-add-sudo = $(if $(and $(call check-ci-requires-root,$1),$(call user-is-not-root,$(USER))),sudo )
endif
maybe-add-parallel =  $(if $(call check-ci-parallel,$1),--nodes 4)

ginkgo := $(shell whereis ginkgo | cut -f 2 -d ' ')
$(if $(ginkgo),,$(error Cannot find ginkgo whereis output: $(shell wereis ginkgo)))

junit-report-filename = $(TEST_REPORT_DIR)/junit-$(call make-pkg-name,$1).xml
coverprofile-filename = $(CURDIR)/$(call make-pkg-name,$1).coverprofile
ginkgo-check-package = $(call maybe-add-sudo,$1)$(ginkgo)           \
                    run                                             \
                    --junit-report $(call junit-report-filename,$1) \
	            --coverprofile $(call coverprofile-filename,$1) \
                    --flake-attempts 4                              \
		    $(call maybe-add-parallel,$1)                   \
                    $(call maybe-add-seed,$1)                       \
                    $(call make-ci-test-binary,$1)
gotest-check-package = $(call maybe-add-sudo)$(call make-ci-test-binary,$1) \
                    --test.v \
                    -test.coverprofile $(call coverprofile-filename,$1)


is-gotest = $(filter $(call remove-package-prefix,$1),$(ci-gotest))

check-package = cd $1 && $(if $(call is-gotest,$1),$(call gotest-check-package,$1),$(call ginkgo-check-package,$1))

build-ci-test-binary = go test -buildvcs=false -mod=vendor -covermode atomic -race -c $1 -o $2

# Define rules for check individual package ($1)
define define-package-rules

$(info Define $1)
.PHONY: $(call make-ci-test-binary,$1)
$(call make-ci-test-binary,$1):
	$(call build-ci-test-binary,$1,$$@)

.PHONY: $(call make-ci-test-target,$1)
$(call make-ci-test-target,$1): $(call make-ci-test-binary,$1)
	$(call check-package,$1)

endef
# Instantiate rule for each PKG
$(foreach pkg,$(PKGS),$(eval $(call define-package-rules,$(pkg))))
