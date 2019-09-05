# OVN Kubernetes CNI FAQ

Q: How do I configure a non-default location for CNI configuration and bin directory?

By default the CNI configuration directory is set to /etc/cni/net.d and the
CNI bin directory is set to /opt/cni/bin. One can change these default locations by
passing the new directory locations to `kubelet` using --cni-conf-dir and --cni-bin-dir
option. If non-default directory location is used in a K8s deployment, then OVN CNI
need to be made aware of it by following the instructions below.

1. Define volumes, for both configuration and bin, in the ovnkube-node pod spec with
   `hostPath` set to the CNI directory on the host machine.
2. Define volumeMounts for CNI configuration directory in the ovnkube-node container
   spec with mountPath set to '/etc/cni/net.d'.
3. Define volumeMounts for CNI bin directory in the ovnkube-node container spec with
   mountPath set to '/opt/cni/bin'