#!/usr/bin/env bash

echo $GOPATH

set -o errexit
set -o nounset
set -o pipefail

PKGS=${PKGS:-.}

for d in $(find ${PKGS} -type d -not -iwholename '*.git*' -a -not -iname '.tool' -a -not -iwholename '*vendor*' -a -not -iwholename '*_output*'); do
	echo "Linting ${d}"
	${GOPATH}/bin/gometalinter.v1 \
		 --exclude='error return value not checked.*(Close|Log|Print).*\(errcheck\)$' \
		 --exclude='.*_test\.go:.*error return value not checked.*\(errcheck\)$' \
		 --exclude='duplicate of.*_test.go.*\(dupl\)$' \
		 --exclude='cmd\/client\/.*\.go.*\(dupl\)$' \
		 --exclude='vendor\/.*' \
		 --exclude='server\/seccomp\/.*\.go.*$' \
		 --disable=aligncheck \
		 --disable=gotype \
		 --disable=gas \
		 --cyclo-over=60 \
		 --dupl-threshold=100 \
		 --tests \
		 --deadline=240s "${d}"
done
