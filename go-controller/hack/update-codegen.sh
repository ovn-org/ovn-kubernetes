#!/usr/bin/env bash

set -o errexit
set -o nounset
set -o pipefail

crds=$(ls pkg/crd 2> /dev/null)
if [ -z "${crds}" ]; then
  exit
fi

controller_gen_version="v0.11.3"
if  ! ( command -v controller-gen > /dev/null ); then
  echo "controller-gen not found, installing sigs.k8s.io/controller-tools"
  olddir="${PWD}"
  builddir="$(mktemp -d)"
  cd "${builddir}"
  GO111MODULE=on go install sigs.k8s.io/controller-tools/cmd/controller-gen@$controller_gen_version
  cd "${olddir}"
  if [[ "${builddir}" == /tmp/* ]]; then #paranoia
      rm -rf "${builddir}"
  fi
fi

gens_version="v0.27.1"
gen_cmds=( "deepcopy-gen" "client-gen" "lister-gen" "informer-gen" )
for cmd in "${gen_cmds[@]}"; do
  if ! ( command -v $cmd > /dev/null ); then
    echo "$cmd not found, installing k8s.io/code-generator/cmd/$cmd@$gens_version"
    GO111MODULE=on go install k8s.io/code-generator/cmd/$cmd@$gens_version
  fi
done

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
    --plural-exceptions="EgressQoS:EgressQoSes" \
    "$@"

  echo "Generating listers for $crd"
  lister-gen \
    --go-header-file hack/boilerplate.go.txt \
    --input-dirs github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/$crd/v1 \
    --output-package github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/$crd/v1/apis/listers \
    --plural-exceptions="EgressQoS:EgressQoSes" \
    "$@"

  echo "Generating informers for $crd"
  informer-gen \
    --go-header-file hack/boilerplate.go.txt \
    --input-dirs github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/$crd/v1 \
    --versioned-clientset-package github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/$crd/v1/apis/clientset/versioned \
    --listers-package  github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/$crd/v1/apis/listers \
    --output-package github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/$crd/v1/apis/informers \
    --plural-exceptions="EgressQoS:EgressQoSes" \
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

echo "Editing EgressQoS CRD"
## We desire that only EgressQoS with the name "default" are accepted by the apiserver.
sed -i -e':begin;$!N;s/.*metadata:\n.*type: object/&\n            properties:\n              name:\n                type: string\n                pattern: ^default$/;P;D' \
	_output/crds/k8s.ovn.org_egressqoses.yaml

echo "Copying generated code.. Add them to your commit..."
for f in github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/*; do
  crdname=$(basename $f)
  echo "Copying $crdname generated code"
  cp -rf github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/$crdname/v1/* pkg/crd/$crdname/v1/
done
rm -rf github.com
echo "Copying the CRDs to dist/templates as j2 files... Add them to your commit..."
for f in _output/crds/*.yaml; do
  crdname=$(basename $f)
  echo "Copying $crdname CRD"
  cp -v "$f" "../dist/templates/$crdname.j2"
done
