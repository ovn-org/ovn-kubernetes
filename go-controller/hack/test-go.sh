#!/bin/bash
set -e

source "$(dirname "${BASH_SOURCE}")/init.sh"

# Check for `go` binary and set ${GOPATH}.
setup_env

cd "${OVN_KUBE_ROOT}"

PKGS="${PKGS:-./cmd/... ./pkg/... ./hybrid-overlay/...}"


for pkg in $(go list -mod vendor ${PKGS}); do
    # This package requires root
    if [[ "$USER" != root && "$pkg" == github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cluster ]]; then
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
