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
    go install -ldflags "-B 0x$(head -c20 /dev/urandom|od -An -tx1|tr -d ' \n')" -v -x "${OVN_KUBE_BINARIES[@]}";
}

test() {
    for test in "${tests[@]:+${tests[@]}}"; do
      local outfile="${OVN_KUBE_OUTPUT_BINPATH}/${platform}/$(basename ${test})"
      # disabling cgo allows use of delve
      CGO_ENABLED=0 go test \
        -i -c -o "${outfile}" \
        "${goflags[@]:+${goflags[@]}}" \
        "$(dirname ${test})"
    done
}

OVN_KUBE_BINARIES=("$@")
build_binaries

