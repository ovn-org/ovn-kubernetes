#!/bin/bash
set -e

source "$(dirname "${BASH_SOURCE}")/init.sh"

# Input:
#   $@ - targets
build_binaries() {
    # Check for `go` binary and set ${GOPATH}.
    setup_env
    cd "${OVN_KUBE_ROOT}"

    mkdir -p "${OVN_KUBE_OUTPUT_BINPATH}"

    # Add a buildid to the executable - needed by rpmbuild
    BUILDID=${BUILDID:-0x$(head -c20 /dev/urandom|od -An -tx1|tr -d ' \n')}
    GIT_COMMIT=$(git rev-parse HEAD)
    GIT_BRANCH=$(git rev-parse --symbolic-full-name --abbrev-ref HEAD)
    BUILD_USER=$(whoami)
    BUILD_DATE=$(date +"%Y-%m-%d")

    set -x
    for bin in "$@"; do
        binbase=$(basename ${bin})
        go build -v \
            -mod vendor \
            -gcflags "${GCFLAGS}" \
            -ldflags "-B ${BUILDID} \
                -X ${OVN_KUBE_GO_PACKAGE}/pkg/metrics.Commit=${GIT_COMMIT} \
                -X ${OVN_KUBE_GO_PACKAGE}/pkg/metrics.Branch=${GIT_BRANCH} \
                -X ${OVN_KUBE_GO_PACKAGE}/pkg/metrics.BuildUser=${BUILD_USER} \
                -X ${OVN_KUBE_GO_PACKAGE}/pkg/metrics.BuildDate=${BUILD_DATE}" \
            -o "${OVN_KUBE_OUTPUT_BINPATH}/${binbase}"\
            "./${bin}"
    done
}

build_windows_binaries() {
    setup_env
    cd "${OVN_KUBE_ROOT}"

    mkdir -p "${OVN_KUBE_OUTPUT_BINPATH_WINDOWS}"

    BUILDID=${BUILDID:-0x$(head -c20 /dev/urandom|od -An -tx1|tr -d ' \n')}
    set -x
    for bin in "$@"; do
        binbase=$(basename ${bin})
        GOOS=windows GOARCH=amd64 go build -v \
            -mod vendor \
            -gcflags "${GCFLAGS}" \
            -ldflags "-B ${BUILDID}" \
            -o "${OVN_KUBE_OUTPUT_BINPATH_WINDOWS}/${binbase}.exe"\
            "./${bin}"
    done
}

if [ -z "${WINDOWS_BUILD}" ]; then
    build_binaries "$@"
else
    build_windows_binaries "$@"
fi
