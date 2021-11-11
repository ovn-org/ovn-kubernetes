#!/bin/bash
set -e

source "$(dirname "${BASH_SOURCE}")/init.sh"

# Check for `go` binary and set ${GOPATH}.
setup_env

cd "${OVN_KUBE_ROOT}"

PKGS=$(go list -mod vendor -f '{{if len .TestGoFiles}} {{.ImportPath}} {{end}}' ${PKGS:-./cmd/... ./pkg/... ./hybrid-overlay/...} | xargs)

if [[ "$1" == "focus" && "$2" != "" ]]; then
    gingko_focus="-ginkgo.focus="${2}""
fi

TEST_REPORT_DIR=${TEST_REPORT_DIR:="./_artifacts"}
function testrun {
    local idx="${1}"
    local pkg="${2}"
    local go_test="go test -mod=vendor"
    local otherargs="${@:3} "
    local args=""
    local ginkgoargs=
    # enable go race detector
    # FIXME race detector fails with hybrid-overlay tests
    if [[ ! -z "${RACE:-}" && "${pkg}" != "github.com/ovn-org/ovn-kubernetes/go-controller/hybrid-overlay/pkg/controller" ]]; then
        args="-race "
    fi
    if [[ "$USER" != root && " ${root_pkgs[@]} " =~ " $pkg " && -z "${DOCKER_TEST:-}" ]]; then
        testfile=$(mktemp --tmpdir ovn-test.XXXXXXXX)
        echo "sudo required for ${pkg}, compiling test to ${testfile}"
        if [[ ! -z "${RACE:-}" && "${pkg}" != "github.com/ovn-org/ovn-kubernetes/go-controller/hybrid-overlay/pkg/controller" ]]; then
            go test -mod=vendor -race -covermode atomic -c "${pkg}" -o "${testfile}"
        else
            go test -mod=vendor -covermode set -c "${pkg}" -o "${testfile}"
        fi
        args=""
        go_test="sudo ${testfile}"
    fi
    if [[ -n "$gingko_focus" ]]; then
        local ginkgoargs=${ginkgo_focus:-}
    fi
    local path=${pkg#github.com/ovn-org/ovn-kubernetes/go-controller}
    if [ ! -z "${COVERALLS:-}" ]; then
        args="${args} -test.coverprofile=${idx}.coverprofile "
    fi
    if grep -q -r "ginkgo" ."${path}"; then
	    prefix=$(echo "${path}" | cut -c 2- | sed 's,/,_,g')
        ginkgoargs="-ginkgo.v ${ginkgo_focus} -ginkgo.reportFile ${TEST_REPORT_DIR}/junit-${prefix}.xml"
    fi
    args="${args}${otherargs}"
    if [ "$go_test" == "go test -mod=vendor" ]; then
        args=${args}${pkg}
    fi
    echo "${go_test} -test.v ${args} ${ginkgoargs}"
    ${go_test} -test.v ${args} ${ginkgoargs} 2>&1
}

# These packages requires root for network namespace maniuplation in unit tests
root_pkgs=("github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node" "github.com/ovn-org/ovn-kubernetes/go-controller/hybrid-overlay/pkg/controller")

i=0
for pkg in ${PKGS}; do
    testrun "${i}" "${pkg}"
    i=$((i+1))
done

rm -f /tmp/ovn-test.* || true
