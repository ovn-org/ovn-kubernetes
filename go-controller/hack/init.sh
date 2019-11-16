#!/bin/bash

OUT_DIR=${OUT_DIR:-_output}

export GO111MODULE=on

# Output Vars:
#   export GOPATH - A modified GOPATH to our created tree along with extra
#     stuff.
#   export GOBIN - This is actively unset if already set as we want binaries
#     placed in a predictable place.
function setup_env() {
    init_source="$( dirname "${BASH_SOURCE}" )/.."
    OVN_KUBE_ROOT="$( absolute_path "${init_source}" )"
    OVN_KUBE_GO_PACKAGE="github.com/ovn-org/ovn-kubernetes/go-controller"
    OVN_KUBE_OUTPUT=${OVN_KUBE_ROOT}/${OUT_DIR}

    if [[ -z "$(command -v go)" ]]; then
        cat <<EOF

Can't find 'go' in PATH, please fix and retry.
See http://golang.org/doc/install for installation instructions.

EOF
        exit 2
    fi

    unset GOBIN

    # create a local GOPATH in _output
    OVN_KUBE_OUTPUT_BINPATH=${OVN_KUBE_OUTPUT}/go/bin
    OVN_KUBE_OUTPUT_BINPATH_WINDOWS=${OVN_KUBE_OUTPUT}/go/bin/windows

}
readonly -f setup_env

# absolute_path returns the absolute path to the directory provided
function absolute_path() {
    local relative_path="$1"
    local absolute_path

    pushd "${relative_path}" >/dev/null
    relative_path="$( pwd )"
    if [[ -h "${relative_path}" ]]; then
            absolute_path="$( readlink "${relative_path}" )"
    else
            absolute_path="${relative_path}"
    fi
    popd >/dev/null

    echo ${absolute_path}
}
readonly -f absolute_path


