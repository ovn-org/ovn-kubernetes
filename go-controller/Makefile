OUT_DIR = _output
export OUT_DIR
PREFIX ?= ${DESTDIR}/usr
BINDIR ?= ${PREFIX}/bin
CNIBINDIR ?= ${DESTDIR}/opt/cni/bin
GCFLAGS ?=
export GCFLAGS

.PHONY: all build check test

# Example:
#   make
#   make all
#   make all GCFLAGS="-N -l"
#       (disables compiler optimization and inlining to aid source debugging tools
#        like delve)


all build:
	hack/build-go.sh cmd/ovnkube/ovnkube.go
	hack/build-go.sh cmd/ovn-kube-util/ovn-kube-util.go
	hack/build-go.sh cmd/ovn-k8s-cni-overlay/ovn-k8s-cni-overlay.go

windows:
	export WINDOWS_BUILD="yes"; \
	hack/build-go.sh cmd/ovnkube/ovnkube.go || exit 1; \
	hack/build-go.sh cmd/ovn-k8s-cni-overlay/ovn-k8s-cni-overlay.go || exit 1; \
	hack/build-go.sh hybrid-overlay/cmd/hybrid-overlay.go || exit 1;

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
	go get -u gopkg.in/alecthomas/gometalinter.v1; \
	$(GOPATH)/bin/gometalinter.v1 --install;

lint:
	./hack/lint.sh

gofmt:
	@./hack/verify-gofmt.sh
