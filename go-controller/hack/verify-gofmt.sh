#!/bin/bash

set -o errexit
set -o nounset
set -o pipefail

PKGS=${PKGS:-.}

find_files() {
  find ${PKGS} -not \( \
      \( \
        -wholename '*/vendor/*' \
        -o -wholename '*/_output/*' \
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
