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
  GO111MODULE=on go install github.com/ovn-org/libovsdb/cmd/modelgen@v0.7.0
  cd "${olddir}"
  if [[ "${builddir}" == /tmp/* ]]; then #paranoia
      rm -rf "${builddir}"
  fi
fi

go generate ./pkg/nbdb
go generate ./pkg/sbdb
go generate ./pkg/vswitchd
