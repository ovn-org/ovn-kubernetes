#!/usr/bin/env bash

set -o errexit
set -o nounset
set -o pipefail

source "$(dirname "${BASH_SOURCE}")/init.sh"

# Check for `go` binary and set ${GOPATH}.
setup_env

function check_or_install() {
  cmd=$1
  url=$2

  if ! ( command -v ${1} > /dev/null ); then
    echo "${1} not found, installing ${2}"
    olddir="${PWD}"
    builddir="$(mktemp -d)"
    cd "${builddir}"
    GO111MODULE=on go install ${2}
    cd "${olddir}"
    if [[ "${builddir}" == /tmp/* ]]; then #paranoia
      rm -rf "${builddir}"
    fi
  fi
}
readonly -f check_or_install

check_or_install modelgen "github.com/ovn-org/libovsdb/cmd/modelgen@8b93f8d269af"
check_or_install controller-gen "sigs.k8s.io/controller-tools/cmd/controller-gen@v0.7.0"
check_or_install deepcopy-gen "k8s.io/code-generator/cmd/deepcopy-gen@v0.22.2"
check_or_install client-gen "k8s.io/code-generator/cmd/client-gen@v0.22.2"
check_or_install lister-gen "k8s.io/code-generator/cmd/lister-gen@v0.22.2"
check_or_install informer-gen "k8s.io/code-generator/cmd/informer-gen@v0.22.2"

go generate ./pkg/nbdb
go generate ./pkg/sbdb

crds=$(ls pkg/crd 2> /dev/null)
if [ -z "${crds}" ]; then
  exit
fi

for crd in ${crds}; do
  echo "Generating deepcopy funcs for $crd"
  deepcopy-gen \
    --go-header-file /dev/null \
    --input-dirs github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/$crd/v1 \
    -O zz_generated.deepcopy \
    --bounding-dirs github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd

  echo "Generating clientset for $crd"
  client-gen \
    --go-header-file /dev/null \
    --clientset-name "${CLIENTSET_NAME_VERSIONED:-versioned}" \
    --input-base "" \
    --input github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/$crd/v1 \
    --output-package github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/$crd/v1/apis/clientset \
    "$@"

  echo "Generating listers for $crd"
  lister-gen \
    --go-header-file /dev/null \
    --input-dirs github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/$crd/v1 \
    --output-package github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/$crd/v1/apis/listers \
    "$@"

  echo "Generating informers for $crd"
  informer-gen \
    --go-header-file /dev/null \
    --input-dirs github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/$crd/v1 \
    --versioned-clientset-package github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/$crd/v1/apis/clientset/versioned \
    --listers-package  github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/$crd/v1/apis/listers \
    --output-package github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/$crd/v1/apis/informers \
    "$@"
done

echo "Generating CRDs"
mkdir -p _output/crds
controller-gen crd:crdVersions="v1"  paths=./pkg/crd/... output:crd:dir=_output/crds
echo "Editing egressFirewall CRD"
## We desire that only egressFirewalls with the name "default" are accepted by the apiserver. The only
## way that we can put a pattern for validation on the name of the object which is embedded in
## metav1.ObjectMeta it is required that we add it after the generation of the CRD.
sed -i -e':begin;$!N;s/.*metadata:\n.*type: object/&\n            properties:\n              name:\n                type: string\n                pattern: ^default$/;P;D' \
	_output/crds/k8s.ovn.org_egressfirewalls.yaml
## It is also required that we restrict the number of properties on the 'to' section of the egressfirewall
## so that either 'dnsName' or 'cidrSelector is set in the crd and currently kubebuilder does not support
## adding validation to objects only to the fields
sed -i -e ':begin;$!N;s/                          type: string\n.*type: object/&\n                      minProperties: 1\n                      maxProperties: 1/;P;D' \
	_output/crds/k8s.ovn.org_egressfirewalls.yaml
