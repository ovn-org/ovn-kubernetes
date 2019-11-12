OUT_DIR = _output
export OUT_DIR
PREFIX ?= ${DESTDIR}/usr
BINDIR ?= ${PREFIX}/bin
CNIBINDIR ?= ${DESTDIR}/opt/cni/bin
GCFLAGS ?=
export GCFLAGS
PKGS ?=

.PHONY: all build check test

# Example:
#   make
#   make all
#   make all GCFLAGS="-N -l"
#       (disables compiler optimization and inlining to aid source debugging tools
#        like delve)


all build:
	hack/build-go.sh ovnkube ovn-k8s-cni-overlay ovn-kube-util

windows:
	WINDOWS_BUILD="yes" hack/build-go.sh ovnkube ovn-k8s-cni-overlay

check test:
	hack/test-go.sh ${PKGS}

install:
	install -D -m 755 ${OUT_DIR}/go/bin/ovnkube ${BINDIR}/
	install -D -m 755 ${OUT_DIR}/go/bin/ovn-kube-util ${BINDIR}/
	install -D -m 755 ${OUT_DIR}/go/bin/ovn-k8s-cni-overlay -t ${CNIBINDIR}/

clean:
	rm -rf ${OUT_DIR}

.PHONY: check-gopath install.tools lint gofmt

check-gopath:
ifndef GOPATH
	$(error GOPATH is not set)
endif

install.tools: check-gopath
	curl -sfL https://install.goreleaser.com/github.com/golangci/golangci-lint.sh | bash -s -- -b ${GOPATH}/bin
lint:
	@./hack/lint.sh

gofmt:
	@./hack/verify-gofmt.sh
