**List of open features**


 - Use daemonset to install ovn-kubernetes
    - RPMs needed?
    - what to do with gateway installation? Can we containerize it?
 - Implement egress/cidr part of network policies
 - subnet per project
    - how to solve host to pod network connectivity (e.g for health checks by kubelet)
 - re-using k8s certificates for OVN
 - OVN master HA
    - See how to make it work with re-using k8s certificates
 - CI/CD for PRs
    - facilitate creating/enriching a test suite within this repository
 - ovsdb go binding
 - Explore how we can have a gateway per node
 
 
**Community meeting:** Every alternate Wednesday starting Jan 17, 2018 at 1pm - 2pm Pacfic time. Location: https://bluejeans.com/8928249476

**Last updated:** Jan 3, 2018
