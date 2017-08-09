
OUT_DIR = _output
export OUT_DIR

.PHONY: all build

# Example:
#   make
#   make all

all build:
	hack/build-go.sh cmd/ovnkube/ovnkube.go
	hack/build-go.sh cmd/ovn-kube-util/ovn-kube-util.go

install:
	cp ${OUT_DIR}/go/bin/* /usr/bin/

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
	@./hack/lint.sh

gofmt:
	@./hack/verify-gofmt.sh
