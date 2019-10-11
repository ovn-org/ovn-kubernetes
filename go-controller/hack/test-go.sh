#!/bin/bash
set -e

source "$(dirname "${BASH_SOURCE}")/init.sh"

# Check for `go` binary and set ${GOPATH}.
setup_env

cd "${OVN_KUBE_ROOT}"

PKGS=$(go list -mod vendor -f '{{if len .TestGoFiles}} {{.ImportPath}} {{end}}' ${PKGS:-./cmd/... ./pkg/... ./hybrid-overlay/...} | xargs)

function testrun {
    local idx="${1}"
    local pkg="${2}"
    local otherargs="${@:3} "
    local args=
    if [ ! -z "${COVERALLS:-}" ]; then
        args="-covermode set -coverprofile ${idx}.coverprofile "
    fi
    args="${args}${otherargs}${pkg}"

    go test -mod vendor ${args}
}

i=0
for pkg in ${PKGS}; do
    # This package requires root
    if [[ "$USER" != root && "$pkg" == github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node ]]; then
        testfile=$(mktemp --tmpdir ovn-test.XXXXXXXX)
        echo "sudo required for ${pkg}, compiling test to ${testfile}"
        testrun "${i}" "${pkg}" -c -o "${testfile}"
        echo sudo "${testfile}"
        sudo "${testfile}"
    else
        testrun "${i}" "${pkg}"
    fi
    i=$((i+1))
done

rm -f /tmp/ovn-test.* || true
