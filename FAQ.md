# OVN Kubernetes CNI FAQ

## 1. How do I configure a non-default location for CNI configuration and bin directory?

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


## 2. What TCP and UDP ports to open on the host for a functional OVN CNI?

OVN CNI requires several TCP and UDP ports to be opened on each of the node
that is part of the K8s cluster.

 1. The node on which ovnkube-master or ovnkube-network-controller-manager runs, open following ports:
    ```text
    TCP:
      port 9409 (prometheus port to export ovnkube-master metrics)
    ```

 2. The node on which ovnkube-node runs, open following ports:
    ```text
    TCP:
      port 9410 (prometheus port to export ovn and ovnkube-node metrics)
    
    UDP:
      port 6081 (for GENEVE traffic)
      port 4789 (when using Hybrid overlay mode)
    ```

 3. The node on which ovnkube-cluster-manager runs, open following ports:
    ```text
    TCP:
      port 9411 (prometheus port to export ovnkube-cluster-manager metrics)
    ```

 4. The node on which ovnkube-db runs, open following ports:
    ```text
    TCP:
      port 6641 (for OVN Northbound OVSDB Server)
      port 6642 (for OVN Southbound OVSDB Server)
      port 6643 (when using RAFT and is required for NB RAFT control plane)
      port 6644 (when using RAFT and is required for SB RAFT control plane)
      port 9476 (prometheus port to export ovn DB metrics)
    ```
 