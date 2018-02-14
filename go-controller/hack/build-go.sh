#!/bin/bash

source "$(dirname "${BASH_SOURCE}")/init.sh"


# Build binary targets specified. Should always be run in a sub-shell so we don't leak GOBIN
#
# Input:
#   $@ - targets and go flags.  If no targets are set then all binaries targets
#     are built.
build_binaries() {
    # Check for `go` binary and set ${GOPATH}.
    setup_env

    local go_pkg_dir="${GOPATH}/src/${OVN_KUBE_GO_PACKAGE}"
    cd ${go_pkg_dir}
    mkdir -p "${OVN_KUBE_OUTPUT_BINPATH}"
    export GOBIN="${OVN_KUBE_OUTPUT_BINPATH}"

    # Add a buildid to the executable - needed by rpmbuild
    go install -ldflags "-B 0x$(head -c20 /dev/urandom|od -An -tx1|tr -d ' \n')" "${OVN_KUBE_BINARIES[@]}";
}

build_windows_binaries() {
    # Check for `go` binary and set ${GOPATH}.
    setup_env

    local go_pkg_dir="${GOPATH}/src/${OVN_KUBE_GO_PACKAGE}"
    cd ${go_pkg_dir}
    mkdir -p "${OVN_KUBE_OUTPUT_BINPATH_WINDOWS}"

    GOOS=windows GOARCH=amd64 go build "${OVN_KUBE_BINARIES[0]}"

    _output_filename=$(basename ${OVN_KUBE_BINARIES[0]})
    output_exe=${_output_filename/go/exe}
    mv $output_exe "${OVN_KUBE_OUTPUT_BINPATH_WINDOWS}"
}

OVN_KUBE_BINARIES=("$@")
if [ -z "${WINDOWS_BUILD}" ]; then
    build_binaries
else
    build_windows_binaries
fi
