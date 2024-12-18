#!/bin/bash
set -o errexit # Nozero exit code of any of the commands below will fail the test.
set -o nounset
set -o pipefail

HERE=$(dirname "$(readlink --canonicalize "$BASH_SOURCE")")
ROOT=$(readlink --canonicalize "$HERE/..")

echo "Checking that go mod / sum and vendor/ is correct"
cd "$ROOT/"
go mod tidy
go mod vendor
cd -
CHANGES=$(git status --porcelain)
if [ -n "$CHANGES" ] ; then
    echo "ERROR: detected go mod / sum or vendor inconsistency after 'go mod tidy; go mod vendor':"
    echo "$CHANGES"
    exit 1
fi
