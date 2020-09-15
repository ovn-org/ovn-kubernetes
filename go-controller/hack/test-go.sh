#!/bin/bash
set -e

source "$(dirname "${BASH_SOURCE}")/init.sh"

# Check for `go` binary and set ${GOPATH}.
setup_env

cd "${OVN_KUBE_ROOT}"

PKGS=$(go list -mod vendor -f '{{if len .TestGoFiles}} {{.ImportPath}} {{end}}' ${PKGS:-./cmd/... ./pkg/... ./hybrid-overlay/...} | xargs)

if [[ "$1" == "focus" && "$2" != "" ]]; then
    gingko_focus="-ginkgo.focus=\"$2\""
fi

function testrun {
    local idx="${1}"
    local pkg="${2}"
    local otherargs="${@:3} "
    local args=
    local ginkgoargs=
    if [[ -n "$gingko_focus" ]]; then
        local ginkgoargs=${ginkgo_focus:-}
    fi
    local path=${pkg#github.com/ovn-org/ovn-kubernetes/go-controller}
    # enable go race detector
    if [ ! -z "${RACE:-}" ]; then
    # FIXME race detector fails with hybrid-overlay tests
        if [[ "${pkg}" != github.com/ovn-org/ovn-kubernetes/go-controller/hybrid-overlay/pkg/controller ]]; then
            args="-race "
        fi
    fi
    # coverage is incompatible with the race detector
    if [ ! -z "${COVERALLS:-}" ]; then
        args="-covermode set -coverprofile ${idx}.coverprofile "
    fi
    if [ -n "${TEST_REPORT_DIR}" ] && grep -q -r "ginkgo" .${path}; then
	    prefix=$( echo ${path} | cut -c 2- | sed 's,/,_,g')
        ginkgoargs="-ginkgo.v -ginkgo.reportFile ${TEST_REPORT_DIR}/junit-${prefix}.xml"
        if [[ -n "$gingko_focus" ]]; then
            ginkgoargs="-ginkgo.v ${gingko_focus} -ginkgo.reportFile ${TEST_REPORT_DIR}/junit-${prefix}.xml"
        fi
    fi
    args="${args}${otherargs}${pkg}"

    go test -v -mod vendor ${args} ${ginkgoargs}
}

# These packages requires root for network namespace maniuplation in unit tests
root_pkgs=("github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node" "github.com/ovn-org/ovn-kubernetes/go-controller/hybrid-overlay/pkg/controller")

i=0
for pkg in ${PKGS}; do
    if [[ "$USER" != root && " ${root_pkgs[@]} " =~ " $pkg " ]]; then
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
