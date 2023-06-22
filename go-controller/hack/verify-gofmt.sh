#!/bin/bash

set -o errexit
set -o nounset
set -o pipefail

PKGS=${PKGS:-.}

find_files="find ${PKGS} -not \( \
      \( \
        -wholename '*/vendor/*' \
        -o -wholename './pkg/crd/*/register.go' \
        -o -wholename './pkg/crd/*/factory.go' \
        -o -wholename '*/_output/*' \
      \) -prune \
    \) -name '*.go'"

GOFMT="gofmt -s"
bad_files=$(eval $find_files | xargs $GOFMT -l)
if [[ -n "${bad_files}" ]]; then
  echo "!!! '$GOFMT' needs to be run on the following files: "
  echo "${bad_files}"
  exit 1
fi

rectify_errors=$(echo $find_files \| xargs sed -i -e \''s/\(fmt\.Errorf.*"\)\([A-Z][a-z]\+\s\)/\1\l\2/g'\')
rectify_logs=$(echo $find_files \| xargs sed -i -e \''s/\(klog\..*"\)\([a-z]\+\s\)/\1\u\2/g'\')

malformatted_errors=$(echo $find_files \| xargs sed -n -e \''s/\(fmt\.Errorf.*"\)\([A-Z][a-z]\+\s\)/\1\l\2/p'\')
malformatted_logs=$(echo $find_files \| xargs sed -n -e \''s/\(klog\..*"\)\([a-z]\+\s\)/\1\u\2/p'\')

exit_code=0
if [[ -n `eval ${malformatted_errors}` ]]; then
       echo "!!! There are fmt.Errorf lines that do not adhere to the style guidelines"
       echo "Copy the following in your terminal, run and commit: \"$rectify_errors\""
       echo "If your OS is a BSD derivative: manually fix the following lines to start with an lowercase letter:"
       eval ${malformatted_errors}
       exit_code=1
fi
if [[ -n `eval ${malformatted_logs}` ]]; then
       echo "!!! There are klog log lines that do not adhere to the style guidelines"
       echo "Copy the following in your terminal, run and commit: \"$rectify_logs\""
       echo "If your OS is a BSD derivative: manually fix the following lines to start with an uppercase letter:"
       eval ${malformatted_logs}
       exit_code=1
fi
exit $exit_code
