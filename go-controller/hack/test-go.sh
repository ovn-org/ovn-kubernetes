#!/bin/bash

source "$(dirname "${BASH_SOURCE}")/init.sh"

# Check for `go` binary and set ${GOPATH}.
setup_env

# Test targets specified. Should always be run in a sub-shell so we don't leak GOBIN
#
# Input:
#   $@ - targets and go flags.  If no targets are set then all binaries targets
#     are built.
test() {
    CGO_ENABLED=0 go test \
        "${goflags[@]:+${goflags[@]}}" \
        ${PKGS}
}

pushd ${GOPATH}/src/${OVN_KUBE_GO_PACKAGE}

if [ -z "$PKGS" ]; then
  # by default, test everything that's not in vendor
  PKGS="$(go list ./... | grep -v vendor | xargs echo)"
fi

test ${PKGS}

popd
