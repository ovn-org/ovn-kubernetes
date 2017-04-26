
OUT_DIR = _output
export OUT_DIR

.PHONY: all build

# Example:
#   make
#   make all

all build:
	hack/build-go.sh cmd/ovnkube/ovnkube.go
	cp -f pkg/cluster/bin/ovnkube-setup-* ${OUT_DIR}/go/bin/

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
	go get -u github.com/alecthomas/gometalinter; \
	$(GOPATH)/bin/gometalinter --install;

lint:
	@./hack/lint.sh

gofmt:
	@./hack/verify-gofmt.sh
