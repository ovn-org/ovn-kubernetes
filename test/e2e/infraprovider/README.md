# Infra Provider
Infra Provider provides test dependencies using an infrastructure agnostic API.

## Motivation
Previous to this API, our tests relied directly on upstream "KinD" to provision networks and launch external hosts.
This prevented downstream consumption of upstream tests.

## Description
Providers manage resources external to the cluster, including adding external hosts [1], provisioning networks,
and attaching networks.

[1] External hosts are deployed as containers by the KinD provider but may be deployed as host-networked containers downstream.

Known implementations:
- KinD
- OpenShift



