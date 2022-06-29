# OVN overlay network on Openshift

This section describes how an OVN overlay network is setup on Openshift 3.10 and later.
It explains the various components and how they come together to establish the OVN overlay network.
People that are interested in understatnding how the ovn cni plugin is installed will find this useful.

As of OCP-3.10, the Openshift overlay network is managed using daemonsets. The ovs/ovn components are
built into a Docker image that has all of the needed packages. Daemonsets are deployed on the nodes
in the cluster to provide the network. The daemonsets use a common Docker image when creating the needed pods.

## Overview

Ovn has a master service that runs on a compute node in the cluster and node controllers
that run on all nodes in the cluster. The master service runs ovn north and ovn south
daemons that work with the ovs north and south databases. All of the nodes run the ovn controller
which accesses the ovn north and south databases.

There are two daemonsets, ovnkube-master.yaml and ovnkube.yaml that mangage the parts of
the ovn architecture. The ovnkube-master daemonset runs both master and node services, ovnkube
daemonset just runs the node service. The daemonset uses selector labels in the cluster's
nodes to specify where the daemonsets run.

Both daemonsets use the same docker image. Configuration is passed from the daemonset to the
image using environment variables and mounting directories from the host. The environment
variables at set from a configmap that is created during installation. The image includes
an control script that is the entrypoint for the container, ovnkube and the openvswitch and
ovn rpms.

The ovnkube program (in this repository) provides the interface between Openshift and Ovn.
The arguments used when starting it determine its role in the configuration which are master
or node.

The ovn cni service is configured and installed after Openshift is installed and running.
Openshift comes up with no network support. One of many networking choices, including ovn,
is selected, installed, configured and started to complete Openshift installation.

The installer creates a configmap that contains the configuration information. The two daemonsets
use the configuration information.

The configmap is mounted in the daemonsets and is used to initialize environment variables that
are passed to the Docker image running in the pod.

## Daemonset

The daemonsets govern where the components run, what is run, and how they interface with other components.
The ovnkube-master.yaml and ovnkube.yaml files bring together the needed pieces and set up the context
for running the pods. Here is some of how it is done.

The nodes are chosen based on labels:

```
spec:
  selector:
    matchLabels:
      node-role.kubernetes.io/compute: "true"
             or
      node-role.kubernetes.io/control-plane: "true"
  template:
    spec:
      nodeSelector:
        node-role.kubernetes.io/compute: "true"
             or
        node-role.kubernetes.io/control-plane: "true"

```
The daemonset will start a pod on every nodes that has the desired labels.

NOTE: The above labels are openshift labels created when openshift is installed.
As this evolves ovn specific labels will be used.

The docker image is configured as follows:
```
spec:
  template:
    spec:
      containers:
      - name: ovnkube-master
        image: "netdev22:5000/ovn-kube:latest"
```
The image is in a docker repository that all nodes can access.

NOTE: The above uses a local development docker repository, netdev22:5000

The openshift service account is created during installation and configured here:
```
spec:
  template:
    spec:
      serviceAccountName: ovn
```

The daemonsets use host networking and provide the cni plugin when they startup.
Docker uses the ovn-cni-plugin to access networking. Docker must be able to access the cni
plugin that is in the image. To do this the host /opt/cni/bin directory
is mounted in the pod and the pod startup script copies the plugin to the host.
Since the host operating environment and the pod are different, the installed
plugin is just a shim that passes requests to the server that is running in the pod.

```
spec:
  template:
    spec:
      hostNetwork: true
      hostPID: true
      containers:
        volumeMounts:
        - mountPath: /host/opt/cni/bin
          name: host-opt-cni-bin
        - mountPath: /etc/cni/net.d
          name: host-etc-cni-netd
        - mountPath: /var/lib/cni/networks/ovn-k8s-cni-overlay
          name: host-var-lib-cni-networks-openshift-ovn
      volumes:
      - name: host-opt-cni-bin
        hostPath:
          path: /opt/cni/bin
      - name: host-etc-cni-netd
        hostPath:
          path: /etc/cni/net.d
      - name: host-var-lib-cni-networks-openshift-ovn
        hostPath:
          path: /var/lib/cni/networks/ovn-k8s-cni-overlay
```

## Docker Image

The Dockerfile directs construction of the image. It installs a base OS, rpms,
and local scripts into the image. It also sets up directories in the image and
copies files into them.  In particular it copies in the entrypoint script, ovnkube.sh.

```
# docker build -t ovn-kube .
# docker tag ovn-kube netdev22:5000/ovn-kube:latest
# docker push netdev22:5000/ovn-kube:latest
```
The image is taged and pushed to a docker repository. This example uses the
local development docker repo, netdev22:5000

NOTE: At present go_controller is built and the resultant files are copied
to the docker build directory. In the future, these files will be installed
from openvswitch-ovn-kubernetes rpm.


## Configuration

There are three sources of configuration information in the daemonsets:

1. /etc/origin/node/ files
These files are installed by the Openshift install and are in the node daemonset
which mounts them on the host. The ovn deamonsets mount them in the pod.

```
spec:
  template:
    spec:
      containers:
        volumeMounts:
        # Directory which contains the host configuration.
        - mountPath: /etc/origin/node/
          name: host-config
          readOnly: true
        - mountPath: /etc/sysconfig/origin-node
          name: host-sysconfig-node
          readOnly: true
      volumes:
      - name: host-config
        hostPath:
          path: /etc/origin/node
      - name: host-sysconfig-node
        hostPath:
          path: /etc/sysconfig/origin-node
```

2. /etc/openvswitch/ovn_k8s.conf

This file is used by ovn daemons and ovnkube to set needed values.
This file is build into the image. It holds configuration constants.

3. ovn-config configmap

This configmap is created after Openshift is installed and running and before
the network is installed. It contains information that is specific to the cluster
that is needed for ovn to access the cluster apiserver.

```
# oc get configmap ovn-config -o yaml
apiVersion: v1
data:
  OvnNorth: tcp:10.19.188.22:6641
  OvnSouth: tcp:10.19.188.22:6642
  k8s_apiserver: https://wsfd-netdev22.ntdv.lab.eng.bos.redhat.com:8443
  net_cidr: 10.128.0.0/14
  svc_cidr: 172.30.0.0/16
kind: ConfigMap
```
OvnNorth and OvnSouth are used by ovn to access its daemons. They are host IP address of the daemons.

k8s_apiserver allows access to Openshift's api server.

net_cidr and svc_cidr are the configuration configuration cidrs

The configmap keys become environment variable values in the daemonset yaml files.

```
spec:
  template:
    spec:
      containers:
        env:
        - name: OVN_NORTH
          valueFrom:
            configMapKeyRef:
              name: ovn-config
              key: OvnNorth
```

