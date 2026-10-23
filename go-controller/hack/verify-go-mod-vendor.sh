#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
# SPDX-License-Identifier: Apache-2.0

set -o errexit # Nozero exit code of any of the commands below will fail the test.
set -o nounset
set -o pipefail

HERE=$(dirname "$(readlink --canonicalize "$BASH_SOURCE")")
ROOT=$(readlink --canonicalize "$HERE/..")

echo "Checking that go mod / sum and vendor/ is correct"
for modfile in "$ROOT/go.mod" "$ROOT"/../test/*/go.mod; do
    module=${modfile%/go.mod}
    echo "Checking go.mod and go.sum in $module"
    go -C "$module" mod tidy
done

go -C "$ROOT" mod vendor

CHANGES=$(git status --porcelain)
if [ -n "$CHANGES" ] ; then
    echo "ERROR: detected go mod / sum or vendor inconsistency after 'go mod tidy; go mod vendor':"
    echo "$CHANGES"
    exit 1
fi
