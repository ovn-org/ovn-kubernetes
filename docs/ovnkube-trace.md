
# ovnkube-trace

A tool to trace packet simulations between points in an ovn-kubernetes driven cluster.

### Usage:

Given the command-line arguments, ovnkube-trace would inspect the cluster to determine the addresses (MAC and IP) of the source and destination and perform ovn-trace, ovs-appctl ofproto/trace, and ovn-detraces from both directions.

```
Usage of ovnkube-trace:
  -dst string
        dest: destination pod name
  -dst-namespace string
        k8s namespace of dest pod (default "default")
  -dst-port string
        dst-port: destination port (default "80")
  -kubeconfig string
        absolute path to the kubeconfig file
  -loglevel string
        loglevel: klog level (default "0")
  -noSSL
        do not use SSL with OVN/OVS
  -ovn-config-namespace string
        namespace used by ovn-config itself (default "openshift-ovn-kubernetes")
  -service string
        service: destination service name
  -src string
        src: source pod name
  -src-namespace string
        k8s namespace of source pod (default "default")
  -tcp
        use tcp transport protocol
  -udp
        use udp transport protocol

```
