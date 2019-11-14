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
    set -x
    for bin in "$@"; do
        go build -v \
            -mod vendor \
            -gcflags "${GCFLAGS}" \
            -ldflags "-B ${BUILDID}" \
            -o "${OVN_KUBE_OUTPUT_BINPATH}/${bin}"\
            "./cmd/${bin}"
    done
}

build_windows_binaries() {
    setup_env
    cd "${OVN_KUBE_ROOT}"

    mkdir -p "${OVN_KUBE_OUTPUT_BINPATH_WINDOWS}"

    BUILDID=${BUILDID:-0x$(head -c20 /dev/urandom|od -An -tx1|tr -d ' \n')}
    set -x
    for bin in "$@"; do
        GOOS=windows go build -v \
            -mod vendor \
            -gcflags "${GCFLAGS}" \
            -ldflags "-B ${BUILDID}" \
            -o "${OVN_KUBE_OUTPUT_BINPATH_WINDOWS}/${bin}"\
            "./cmd/${bin}"
    done
}

if [ -z "${WINDOWS_BUILD}" ]; then
    build_binaries "$@"
else
    build_windows_binaries "$@"
fi
