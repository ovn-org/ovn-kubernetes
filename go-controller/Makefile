
OUT_DIR = _output
export OUT_DIR

# Example:
#   make
#   make all
all build:
	hack/build-go.sh cmd/ovnkube/ovnkube.go
	cp -f pkg/cluster/bin/ovnkube-setup-* ${OUT_DIR}/go/bin/

clean:
	rm -rf ${OUT_DIR}
.PHONY: all build
