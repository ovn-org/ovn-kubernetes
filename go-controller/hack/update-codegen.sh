#!/usr/bin/env bash
# make sure you added GOPATH/bin to your PATH
# this is required to find apps installed with 'go install'
set -o errexit
set -o nounset
set -o pipefail

# generate ovsdb bindings
if  ! ( command -v modelgen > /dev/null ); then
  echo "modelgen not found, installing github.com/ovn-org/libovsdb/cmd/modelgen"
  olddir="${PWD}"
  builddir="$(mktemp -d)"
  cd "${builddir}"
  GO111MODULE=on go install github.com/ovn-org/libovsdb/cmd/modelgen@2cbe2d093e1247d42050306dd5c9a2d6c11f2460
  cd "${olddir}"
  if [[ "${builddir}" == /tmp/* ]]; then #paranoia
      rm -rf "${builddir}"
  fi
fi

go generate ./pkg/nbdb
go generate ./pkg/sbdb

crds=$(ls pkg/crd 2> /dev/null)
if [ -z "${crds}" ]; then
  exit
fi

if  ! ( command -v controller-gen > /dev/null ); then
  echo "controller-gen not found, installing sigs.k8s.io/controller-tools"
  olddir="${PWD}"
  builddir="$(mktemp -d)"
  cd "${builddir}"
  GO111MODULE=on go install sigs.k8s.io/controller-tools/cmd/controller-gen@latest
  cd "${olddir}"
  if [[ "${builddir}" == /tmp/* ]]; then #paranoia
      rm -rf "${builddir}"
  fi
fi

if  ! ( command -v deepcopy-gen > /dev/null ); then
  echo "deepcopy-gen not found, installing k8s.io/code-generator/cmd/{client-gen,lister-gen,informer-gen,deepcopy-gen}"
  olddir="${PWD}"
  builddir="$(mktemp -d)"
  cd "${builddir}"
  GO111MODULE=on go install k8s.io/code-generator/cmd/{client-gen,lister-gen,informer-gen,deepcopy-gen}@latest
  cd "${olddir}"
  if [[ "${builddir}" == /tmp/* ]]; then #paranoia
      rm -rf "${builddir}"
  fi
fi

for crd in ${crds}; do
  echo "Generating deepcopy funcs for $crd"
  deepcopy-gen \
    --go-header-file hack/boilerplate.go.txt \
    --input-dirs github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/$crd/v1 \
    -O zz_generated.deepcopy \
    --bounding-dirs github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd

  echo "Generating clientset for $crd"
  client-gen \
    --go-header-file hack/boilerplate.go.txt \
    --clientset-name "${CLIENTSET_NAME_VERSIONED:-versioned}" \
    --input-base "" \
    --input github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/$crd/v1 \
    --output-package github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/$crd/v1/apis/clientset \
    "$@"

  echo "Generating listers for $crd"
  lister-gen \
    --go-header-file hack/boilerplate.go.txt \
    --input-dirs github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/$crd/v1 \
    --output-package github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/$crd/v1/apis/listers \
    "$@"

  echo "Generating informers for $crd"
  informer-gen \
    --go-header-file hack/boilerplate.go.txt \
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

echo "Editing EgressQoS CRD"
## We desire that only EgressQoS with the name "default" are accepted by the apiserver.
sed -i -e':begin;$!N;s/.*metadata:\n.*type: object/&\n            properties:\n              name:\n                type: string\n                pattern: ^default$/;P;D' \
	_output/crds/k8s.ovn.org_egressqoses.yaml