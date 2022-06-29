ovn container construction and operation

NOTES/LIMITS:
- Uses node-role.kubernetes.io/control-plane and compute labels.
  Eventually there will be ovn specific labels.
- When the ovn-master comes up it annotates each node with
  the subnet cidr for the node. This MUST be done before 
  the daemonset on the other nodes is started.
- OVN requires cluster admin to annotate nodes
- if ovs is stopped or fails, host networking is broken
  rebooting doesn't fix always the problem.
- only one network cidr is supported.
- only one ovn-master is supported.
- the image build builds whichever commit is currently checkedout
  Not a specified version (v0.4.0)
- the rpm build builds a specified version.
- the cluster install installs ovs and multitenant. The daemonsets
  must be deleted before running ovn daemonsets.
NOTES/LIMITS:

The ovs daemonset runs
/usr/share/openvswitch/scripts/ovs-ctl start
to start openvswitch. it must be running for the ovn-daemonsets to
run.

There are two daemonsets that support ovn. ovnkube-master runs
on the cluster masters, ovnkube runs on all nodes.
The daemonsets run with hostNetwork: true.

The both daemonsets run the node daemons, ovn-controller and ovn-node.
In addition the daemonset runs ovn-northd and ovn-master.

The startup sequence requires this startup order:
- ovs
- ovnkube-master on the masters
- ovnkube on all nodes.

===============================

There is one docker image that is used by sdn-ovs, ovnkube-master
and ovnkube daemonsets. The ovnkube-master daemonset sets environmet
variable OVN_MASTER="true".

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
        node-role.kubernetes.io/control-plane: "true"

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

===========================

$ cd images && make
builds the default centos based image from  Dockerfile
$  make fedora
builds the image with the fedora 

Once the image is built it must be tagged and pushed to a docker image repo
that is availabe for all nodes to download the image. A local docker repo,
netdev31:5000 is used in the build. This will need to be changed when building
for different clusters.

It is convient to set up a docker registry for the cluster and add it to
the /etc/containers/registries.conf file on each node in both the
"registries:" and "insecure_registries:" sections.
