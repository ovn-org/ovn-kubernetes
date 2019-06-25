#!/bin/bash

source "$(dirname "${BASH_SOURCE}")/init.sh"

# Check for `go` binary and set ${GOPATH}.
setup_env

pushd ${GOPATH}/src/${OVN_KUBE_GO_PACKAGE}

if [ -z "$PKGS" ]; then
  # by default, test everything that's not in vendor
  PKGS="$(go list -f '{{if len .TestGoFiles}} {{.ImportPath}} {{end}}' ./... | xargs echo)"
fi

# Work around sudo's PATH handling since Travis puts in Go in the travis user's homedir
GO=`which go`
# sudo is required because some testcases manipulate network namespaces
sudo -E bash -c "umask 0; CGO_ENABLED=0 ${GO} test "${goflags[@]:+${goflags[@]}}" ${PKGS}"
retcode=$?

popd
exit $retcode
