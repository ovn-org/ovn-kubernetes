#!/bin/bash
set -e

source "$(dirname "${BASH_SOURCE}")/init.sh"

# Check for `go` binary and set ${GOPATH}.
setup_env

cd "${OVN_KUBE_ROOT}"

PKGS=$(go list -mod vendor -f '{{if len .TestGoFiles}} {{.ImportPath}} {{end}}' ${PKGS:-./cmd/... ./pkg/...})

for pkg in ${PKGS}; do
    # This package requires root
    if [[ "$USER" != root && "$pkg" == github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node ]]; then
        testfile=$(mktemp --tmpdir ovn-test.XXXXXXXX)
        echo "sudo required for ${pkg}, compiling test to ${testfile}"
        go test -mod vendor -c -o "${testfile}" "${pkg}"
        echo sudo "${testfile}"
        sudo "${testfile}"
    else
        go test -mod vendor "${pkg}"
    fi
done

rm -f /tmp/ovn-test.* || true
