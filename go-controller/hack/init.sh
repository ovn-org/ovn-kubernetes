#!/bin/bash

OUT_DIR=${OUT_DIR:-_output}

# Output Vars:
#   export GOPATH - A modified GOPATH to our created tree along with extra
#     stuff.
#   export GOBIN - This is actively unset if already set as we want binaries
#     placed in a predictable place.
function setup_env() {
  init_source="$( dirname "${BASH_SOURCE}" )/.."
  OVN_KUBE_ROOT="$( absolute_path "${init_source}" )"
  export OVN_KUBE_ROOT
  pushd ${OVN_KUBE_ROOT} >/dev/null
  OVN_KUBE_GO_PACKAGE="github.com/openvswitch/ovn-kubernetes/go-controller"
  OVN_KUBE_OUTPUT=${OVN_KUBE_ROOT}/${OUT_DIR}

  if [[ -z "$(which go)" ]]; then
    cat <<EOF

Can't find 'go' in PATH, please fix and retry.
See http://golang.org/doc/install for installation instructions.

EOF
    exit 2
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

  popd >/dev/null
  # lots of tools "just don't work" unless we're in the GOPATH
  #cd "${go_pkg_dir}"

  export GOPATH
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


