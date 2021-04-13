#!/bin/bash
set -e

source "$(dirname "${BASH_SOURCE}")/init.sh"

# Check for `go` binary and set ${GOPATH}.
setup_env

cd "${OVN_KUBE_ROOT}"

if [[ "$1" == "focus" && "$2" != "" ]]; then
    gingko_focus="-ginkgo.focus="${2}""
    shift; shift
fi

PKGS="$(go list -mod vendor -f '{{if len .TestGoFiles}} {{.ImportPath}} {{end}}' ${PKGS:-./cmd/... ./pkg/... ./hybrid-overlay/...} | xargs)"

TEST_REPORT_DIR=${TEST_REPORT_DIR:="./_artifacts"}
function testrun {
    local idx="${1}"
    local pkg="${2}"
    local go_test="go test -mod=vendor"
    local args=""
    local ginkgoargs=
    echo pkg=$pkg
    # enable go race detector
    # FIXME race detector fails with hybrid-overlay tests
    if [[ -n "${RACE}" && "${pkg}" != "github.com/ovn-org/ovn-kubernetes/go-controller/hybrid-overlay/pkg/controller" ]]; then
        args="-race "
    fi
    if [[ "$USER" != root && " ${root_pkgs[@]} " =~ " $pkg " && -z "${DOCKER_TEST:-}" ]]; then
        testfile=$(mktemp --tmpdir ovn-test.XXXXXXXX)
        echo "sudo required for ${pkg}, compiling test to ${testfile}"
        if [[ ! -z "${RACE:-}" && "${pkg}" != "github.com/ovn-org/ovn-kubernetes/go-controller/hybrid-overlay/pkg/controller" ]]; then
            go test -mod=vendor -race  -c "${pkg}" -o "${testfile}"
        else
            go test -mod=vendor -c "${pkg}" -o "${testfile}"
        fi
        args=""
        go_test="sudo ${testfile}"
    fi

    # set gomaxprocs to 1 in CI, because actions are run in containers and we don't know what cpu
    # limits are being imposed
    if [ -n "${CI}" ]; then
        args="${args}-test.cpu 1 "
    fi
    if [[ -n "$gingko_focus" ]]; then
        ginkgoargs=${ginkgo_focus:-}
    fi
    local path=${pkg#github.com/ovn-org/ovn-kubernetes/go-controller}
    if [ ! -z "${COVERALLS:-}" ]; then
        args="$args -test.coverprofile=${idx}.coverprofile "
    fi
    if grep -q -r "ginkgo" ."${path}"; then
	    prefix=$(echo "${path}" | cut -c 2- | sed 's,/,_,g')
        ginkgoargs="-ginkgo.v -ginkgo.reportFile ${TEST_REPORT_DIR}/junit-${prefix}.xml"
        if [[ -n "$gingko_focus" ]]; then
            ginkgoargs="-ginkgo.v ${gingko_focus} -ginkgo.reportFile ${TEST_REPORT_DIR}/junit-${prefix}.xml"
        fi
    fi
    if [ "$go_test" == "go test -mod=vendor" ]; then
        args="${args}${pkg}"
    fi
    echo "${go_test}  ${args} ${ginkgoargs}"
    ${go_test}  ${args} ${ginkgoargs} 2>&1
}

# These packages requires root for network namespace maniuplation in unit tests
root_pkgs=("github.com/ovn-org/ovn-kubernetes/go-controller/pkg/node" "github.com/ovn-org/ovn-kubernetes/go-controller/hybrid-overlay/pkg/controller")

i=0
for pkg in ${PKGS}; do
    testrun "${i}" "${pkg}"
    i=$((i+1))
done

rm -f /tmp/ovn-test.* || true
