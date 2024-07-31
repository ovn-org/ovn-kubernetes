#!/usr/bin/env bash

set -o errexit
set -o nounset
set -o pipefail

crds=$(ls pkg/crd 2> /dev/null)
if [ -z "${crds}" ]; then
  exit
fi

SCRIPT_ROOT=$(dirname ${BASH_SOURCE})/..
olddir="${PWD}"
builddir="$(mktemp -d)"
cd "${builddir}"
GO111MODULE=on go install sigs.k8s.io/controller-tools/cmd/controller-gen@v0.14.0
BINS=(
    deepcopy-gen
    applyconfiguration-gen
    client-gen
    informer-gen
    lister-gen
)
GO111MODULE=on go install $(printf "k8s.io/code-generator/cmd/%s@release-1.29 " "${BINS[@]}")
cd "${olddir}"
if [[ "${builddir}" == /tmp/* ]]; then #paranoia
    rm -rf "${builddir}"
fi

for crd in ${crds}; do
  echo "Generating deepcopy funcs for $crd"
  deepcopy-gen \
    --go-header-file hack/boilerplate.go.txt \
    --input-dirs github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/$crd/v1 \
    --output-base "${SCRIPT_ROOT}" \
    -O zz_generated.deepcopy \
    --bounding-dirs github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd

  echo "Generating apply configuration for $crd"
  applyconfiguration-gen \
    --go-header-file hack/boilerplate.go.txt \
    --input-dirs github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/$crd/v1 \
    --output-base "${SCRIPT_ROOT}" \
    --output-package github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/$crd/v1/apis/applyconfiguration \
    "$@"

  echo "Generating clientset for $crd"
  client-gen \
    --go-header-file hack/boilerplate.go.txt \
    --clientset-name "${CLIENTSET_NAME_VERSIONED:-versioned}" \
    --input-base "" \
    --input github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/$crd/v1 \
    --output-base "${SCRIPT_ROOT}" \
    --output-package github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/$crd/v1/apis/clientset \
    --apply-configuration-package github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/$crd/v1/apis/applyconfiguration \
    --plural-exceptions="EgressQoS:EgressQoSes,NetworkQoS:NetworkQoSes" \
    "$@"

  echo "Generating listers for $crd"
  lister-gen \
    --go-header-file hack/boilerplate.go.txt \
    --input-dirs github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/$crd/v1 \
    --output-base "${SCRIPT_ROOT}" \
    --output-package github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/$crd/v1/apis/listers \
    --plural-exceptions="EgressQoS:EgressQoSes,NetworkQoS:NetworkQoSes" \
    "$@"

  echo "Generating informers for $crd"
  informer-gen \
    --go-header-file hack/boilerplate.go.txt \
    --input-dirs github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/$crd/v1 \
    --versioned-clientset-package github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/$crd/v1/apis/clientset/versioned \
    --listers-package  github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/$crd/v1/apis/listers \
    --output-base "${SCRIPT_ROOT}" \
    --output-package github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/$crd/v1/apis/informers \
    --plural-exceptions="EgressQoS:EgressQoSes,NetworkQoS:NetworkQoSes" \
    "$@"

  echo "Copying apis for $crd"
  rm -rf $SCRIPT_ROOT/pkg/crd/$crd/v1/apis
  cp -r github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/$crd/v1/apis $SCRIPT_ROOT/pkg/crd/$crd/v1

  echo "Copying zz_generated for $crd"
  cp github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/$crd/v1/zz_generated.deepcopy.go $SCRIPT_ROOT/pkg/crd/$crd/v1

done

rm -rf "${SCRIPT_ROOT}/github.com/"

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

echo "Copying the CRDs to dist/templates as j2 files... Add them to your commit..."
echo "Copying egressFirewall CRD"
cp _output/crds/k8s.ovn.org_egressfirewalls.yaml ../dist/templates/k8s.ovn.org_egressfirewalls.yaml.j2
echo "Copying egressIP CRD"
cp _output/crds/k8s.ovn.org_egressips.yaml ../dist/templates/k8s.ovn.org_egressips.yaml.j2
echo "Copying egressQoS CRD"
cp _output/crds/k8s.ovn.org_egressqoses.yaml ../dist/templates/k8s.ovn.org_egressqoses.yaml.j2
echo "Copying adminpolicybasedexternalroutes CRD"
cp _output/crds/k8s.ovn.org_adminpolicybasedexternalroutes.yaml ../dist/templates/k8s.ovn.org_adminpolicybasedexternalroutes.yaml.j2
echo "Copying egressService CRD"
cp _output/crds/k8s.ovn.org_egressservices.yaml ../dist/templates/k8s.ovn.org_egressservices.yaml.j2
echo "Copying IPAMClaim CRD"
curl -sSL https://raw.githubusercontent.com/k8snetworkplumbingwg/ipamclaims/v0.4.0-alpha/artifacts/k8s.cni.cncf.io_ipamclaims.yaml -o ../dist/templates/k8s.cni.cncf.io_ipamclaims.yaml
echo "Copying networkQoS CRD"
cp _output/crds/k8s.ovn.org_networkqoses.yaml ../dist/templates/k8s.ovn.org_networkqoses.yaml.j2
