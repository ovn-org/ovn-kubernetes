# Release Documentation

## Overview

Each new release of OVN-Kubernetes is defined with a "version" that represents the Git tag of a release, such as v1.0.0. This contains the following:

* Source Code
* Binaries
  * `ovnkube`: which is our main single all-in-one binary executable used to launch the ovnkube control plane and data plane pods in a kubernetes deployment
  * `ovn-k8s-cni-overlay`: is the cni executable to be placed in /opt/cni/bin (or another directory in which kubernetes will look for the plugin) so that it can be invoked for each pod event by kubernetes
  * `hybrid-overlay-node`
  * `ovn-kube-util`: contains the Utils for ovn-kubernetes
  * `ovndbchecker`
  * `ovnkube-trace`: is the binary that contains ovnkube-trace which is an abstraction used to invoke OVN/OVS packet tracing utils
  * `ovnkube-identity`: is the executable that is invoked to run ovn-kubernetes identity manager, which includes the admission webhook and the CertificateSigningRequest approver
* `ovnkube` API configuration 
* scripts used to deploy OVN-Kubernetes including helm charts
