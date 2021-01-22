OUT_DIR = _output
export OUT_DIR
PREFIX ?= ${DESTDIR}/usr
BINDIR ?= ${PREFIX}/bin
CNIBINDIR ?= ${DESTDIR}/opt/cni/bin
GCFLAGS ?=
export GCFLAGS
PKGS ?=
GOPATH ?= $(shell go env GOPATH)
TEST_REPORT_DIR?=$(CURDIR)/_artifacts
export TEST_REPORT_DIR
GO_DKR_IMG = golang:1.15.7-buster
# DOCKER_RUNNABLE determines if the tests can be run inside a docker container. It checks to see if
# docker is installed on the system and also confirms that it is not running in a CI environment by checking that the
# GITHUB_ACTIONS environment variable is not set. NOTE: Running tests in a docker container inside of a CI environment
# fails some of the tests needing mount options inspite of starting the container with the right linux capabilities.
DOCKER_RUNNABLE ?= $(shell docker -v > /dev/null 2>&1 && ! printenv GITHUB_ACTIONS > /dev/null 2>&1; echo $$?)

.PHONY: all build check test

# Example:
#   make
#   make all
#   make all GCFLAGS="-N -l"
#       (disables compiler optimization and inlining to aid source debugging tools
#        like delve)


all build:
	hack/build-go.sh cmd/ovnkube cmd/ovn-k8s-cni-overlay cmd/ovn-kube-util hybrid-overlay/cmd/hybrid-overlay-node cmd/ovndbchecker cmd/ovnkube-trace

windows:
	WINDOWS_BUILD="yes" hack/build-go.sh hybrid-overlay/cmd/hybrid-overlay-node

check test:
ifeq ($(DOCKER_RUNNABLE), 0)
	docker run --cap-add=NET_ADMIN --cap-add=SYS_ADMIN -v $(shell dirname $(PWD)):/go/src/github.com/ovn-org/ovn-kubernetes -w /go/src/github.com/ovn-org/ovn-kubernetes/go-controller $(GO_DKR_IMG) sh -c "RACE=1 DOCKER_TEST=1 hack/test-go.sh ${PKGS}"
else
	RACE=1 hack/test-go.sh ${PKGS}
endif

codegen:
	hack/update-codegen.sh

install:
	install -D -m 755 ${OUT_DIR}/go/bin/ovnkube ${BINDIR}/
	install -D -m 755 ${OUT_DIR}/go/bin/ovn-kube-util ${BINDIR}/
	install -D -m 755 ${OUT_DIR}/go/bin/ovn-k8s-cni-overlay -t ${CNIBINDIR}/
	install -D -m 755 ${OUT_DIR}/go/bin/ovndbchecker ${BINDIR}/

clean:
	rm -rf ${OUT_DIR}
	rm -rf ${TEST_REPORT_DIR}

.PHONY: install.tools lint gofmt

install.tools:
	curl -sfL https://install.goreleaser.com/github.com/golangci/golangci-lint.sh | bash -s -- -b ${GOPATH}/bin

lint:
	@GOPATH=${GOPATH} ./hack/lint.sh

gofmt:
ifeq ($(DOCKER_RUNNABLE), 0)
	docker run --cap-add=NET_ADMIN --cap-add=SYS_ADMIN -v $(shell dirname $(PWD)):/go/src/github.com/ovn-org/ovn-kubernetes -w /go/src/github.com/ovn-org/ovn-kubernetes/go-controller $(GO_DKR_IMG) hack/verify-gofmt.sh
else
	@./hack/verify-gofmt.sh
endif
