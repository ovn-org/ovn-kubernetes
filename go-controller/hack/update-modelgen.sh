set -o errexit
set -o nounset
set -o pipefail

# generate ovsdb bindings
if  ! ( command -v modelgen > /dev/null ); then
  echo "modelgen not found, installing github.com/ovn-org/libovsdb/cmd/modelgen"
  olddir="${PWD}"
  builddir="$(mktemp -d)"
  cd "${builddir}"
  # ensure the hash value is not outdated, if wrong bindings are being generated re-install modelgen
  GO111MODULE=on go install github.com/ovn-org/libovsdb/cmd/modelgen@a4f2602f585a3c2995a0abea7010b333631341d9
  cd "${olddir}"
  if [[ "${builddir}" == /tmp/* ]]; then #paranoia
      rm -rf "${builddir}"
  fi
fi

go generate ./pkg/nbdb
go generate ./pkg/sbdb
