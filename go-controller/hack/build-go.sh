#!/bin/bash

source "$(dirname "${BASH_SOURCE}")/init.sh"

OUT_DIR=${OUT_DIR:-_output}

init_source="$( dirname "${BASH_SOURCE}" )/.."
OVN_KUBE_ROOT="$( os::util::absolute_path "${init_source}" )"
export OVN_KUBE_ROOT
cd ${OVN_KUBE_ROOT}
OVN_KUBE_GO_PACKAGE="github.com/openvswitch/ovn-kubernetes/go-controller"
OVN_KUBE_OUTPUT=${OVN_KUBE_ROOT}/${OUT_DIR}

# Output Vars:
#   export GOPATH - A modified GOPATH to our created tree along with extra
#     stuff.
#   export GOBIN - This is actively unset if already set as we want binaries
#     placed in a predictable place.
function setup_env() {
  if [[ -z "$(which go)" ]]; then
    cat <<EOF

Can't find 'go' in PATH, please fix and retry.
See http://golang.org/doc/install for installation instructions.

EOF
    exit 2
  fi

  # Travis continuous build uses a head go release that doesn't report
  # a version number, so we skip this check on Travis.  It's unnecessary
  # there anyway.
  if [[ "${TRAVIS:-}" != "true" ]]; then
    local go_version
    go_version=($(go version))
    if [[ "${go_version[2]}" < "go1.5" ]]; then
      cat <<EOF

Detected Go version: ${go_version[*]}.
ovn-kube builds require Go version 1.6 or greater.

EOF
      exit 2
    fi
  fi

  unset GOBIN

  # create a local GOPATH in _output
  GOPATH="${OVN_KUBE_OUTPUT}/go"
  OVN_KUBE_OUTPUT_BINPATH=${GOPATH}/bin
  local go_pkg_dir="${GOPATH}/src/${OVN_KUBE_GO_PACKAGE}"
  local go_pkg_basedir=$(dirname "${go_pkg_dir}")

  mkdir -p "${go_pkg_basedir}"
  rm -f "${go_pkg_dir}"

  # TODO: This symlink should be relative.
  ln -s "${OVN_KUBE_ROOT}" "${go_pkg_dir}"

  # lots of tools "just don't work" unless we're in the GOPATH
  cd "${go_pkg_dir}"

  export GOPATH
}
readonly -f setup_env

# Build binary targets specified. Should always be run in a sub-shell so we don't leak GOBIN
#
# Input:
#   $@ - targets and go flags.  If no targets are set then all binaries targets
#     are built.
build_binaries() {
    # Check for `go` binary and set ${GOPATH}.
    setup_env

    mkdir -p "${OVN_KUBE_OUTPUT_BINPATH}"
    export GOBIN="${OVN_KUBE_OUTPUT_BINPATH}"

    go install "${OVN_KUBE_BINARIES[@]}"
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

