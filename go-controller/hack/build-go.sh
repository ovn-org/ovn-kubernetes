#!/bin/bash
set -e

GO=${GO:-go}
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
    K8S_CLIENT_VERSION=$(grep 'k8s.io/client-go' ${OVN_KUBE_GO_PACKAGE}/go.sum | head -1 |cut -f2 -d' ')

    set -x
    for bin in "$@"; do
        binbase=$(basename ${bin})
        env CGO_ENABLED=0 "$GO" build -v \
            -mod vendor \
            -gcflags "${GCFLAGS}" \
            -ldflags "-B ${BUILDID} \
                -X ${OVN_KUBE_GO_PACKAGE}/pkg/config.Commit=${GIT_COMMIT} \
                -X ${OVN_KUBE_GO_PACKAGE}/pkg/config.Branch=${GIT_BRANCH} \
                -X ${OVN_KUBE_GO_PACKAGE}/pkg/config.BuildUser=${BUILD_USER} \
                -X ${OVN_KUBE_GO_PACKAGE}/pkg/config.BuildDate=${BUILD_DATE} \
                -X k8s.io/client-go/pkg/version.gitVersion=${K8S_CLIENT_VERSION} \
		`if [ "$binbase" != "ovnkube" ]; then echo ${LDFLAGS}; fi`" \
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
            -ldflags "-B ${BUILDID} \
		`if [ "$binbase" != "ovnkube" ]; then echo ${LDFLAGS}; fi`" \
            -o "${OVN_KUBE_OUTPUT_BINPATH_WINDOWS}/${binbase}.exe"\
            "./${bin}"
    done
}

if [ -z "${WINDOWS_BUILD}" ]; then
    build_binaries "$@"
else
    build_windows_binaries "$@"
fi
