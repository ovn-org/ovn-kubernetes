ovn container construction and operation

NOTES/LIMITS:
- Uses node-role.kubernetes.io/master and compute labels. 
  Eventually there will be ovn specific labels.
- currently building in ../go_controller and copying files to
  ../images and put in an image. This will likely change.
NOTES/LIMITS:

There are two daemonsets that support ovn. ovnkube-master runs
on the cluster master node, ovnkube runs on the remaining nodes.
The daemonsets run with hostNetwork: true.

The both daemonsets run the node daemons, ovn-controller and ovn-node.
In addition the daemonset runs ovn-northd and ovn-master.

The startup sequence requires this startup order:
- ovs 
- ovnkube-master on the master node
- ovnkube on the rest of the nodes.

===============================

There is one docker image that is used by both the ovnkube-master
and ovnkube daemonsets. The master sets environmet variable
OVN_MASTER="true".

When this is present the image runs ovn-northd and ovn-master
before running ovn-controller and ovn-node
 
The image entrypoint is /root/ovnkube.sh
This script sequences through the operations to bring up networking.
Configuration is passed in via environment variables.

===============================

- ovnkube-master.yaml

    metadata:
      labels:
        app: ovnkube-master
        node-role.kubernetes.io/master: "true"

- ovnkube.yaml

    metadata:
      labels:
        app: ovnkube
        node-role.kubernetes.io/compute: "true"

===========================
How networking is set up.

The ovn cni plugin, ovn-k8s-cni-overlay, must be visible to kubelet so that 
kubelet can use it. The ovn plugin and setup are in the container image so 
they need to be moved to the host. This is done by mounting the following
directories and copying files from the contianer to the mounted directories.

/opt/cni/bin mounted on /host/opt/cni/bin
/etc/cni/net.d
/var/lib/cni/networks/ovn-k8s-cni-overlay

/etc/cni/net.d and /opt/cni/bin are cleared and ovn-k8s-cni-overlay and
loopback are copied into /opt/cni/bin. This makes the plugin available to
kubelet.

