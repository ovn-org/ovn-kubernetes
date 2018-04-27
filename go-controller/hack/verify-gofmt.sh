#!/bin/bash

source "$(dirname "${BASH_SOURCE}")/init.sh"

# Check for `go` binary and set ${GOPATH}.
setup_env

set -o errexit
set -o nounset
set -o pipefail

find_files() {
  find . -not \( \
      \( \
        -wholename '*/vendor/*' \
        -o -wholename '*/alecthomas/*' \
      \) -prune \
    \) -name '*.go'
}

GOFMT="gofmt -s"
bad_files=$(find_files | xargs $GOFMT -l)
if [[ -n "${bad_files}" ]]; then
  echo "!!! '$GOFMT' needs to be run on the following files: "
  echo "${bad_files}"
  exit 1
fi
