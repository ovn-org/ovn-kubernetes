# OVN tech preview on OpenShift/origin/kubernetes

NOTE:
- This is a temporary approach to working with the tech preview. Ultimately
4.0 will install using the network operator.

You can preview ovn overlay networks on an OpenShift 4.0 cluster by replacing
the installed CNI plugin with OVN.
A v3.11 cluster installs with no plugin and 4.0 (currently) installs OpenShiftSDN.

NOTES:
- This is Tech Preview, not Production, and it will change over time.
- There is no upgrade path. New install only.
- This is limited to a single master in the cluster (no high availability).


## Installation:

Install OKD-v4.0 as instructed in the OKD documentation. More TBD.


Install OKD-v3.11 as instructed in the OKD documentation. Make the following change
before running the cluster install playbook:
1. In the ansible host file for the cluster change:
```
os_sdn_network_plugin_name='redhat/openshift-ovs-multitenant'
```
  to
```
os_sdn_network_plugin_name='cni'
openshift_use_openshift_sdn='False'
```

When the cluster install is complete and the cluster is up there will
be no cluster networking.


Clone ovn-kubernetes on a convenient host where you can run ansible-playbook:
```
# git clone https://github.com/ovn-org/ovn-kubernetes
# cd ovn-kubernetes/dist/ansible
```

Edit the hosts file adding the name of the cluster master and select the ovn_image
and ovn_image_pull_policy. The default is the community image.
Also, you can specify a network cidr and service cidr, or just take the default.

Provision the cluster for OVN:
```
# ./run-playbook
```

OVN may be removed from the cluster by running:
```
# ./run-playbook uninstall
```

The yaml/{ovnkube.yaml,ovnkube-master.yaml} files are now created as follows:
```
# cd ../images
# make daemonsetyaml
```
The daemonsets are now in template files that are expanded on the master. The
previous daemonsets in dist/yaml have been deleted and can be reconstructed
using the above make.

```
# oc project
Using project "ovn-kubernetes" on server "https://wsfd-netdev22.ntdv.lab.eng.bos.redhat.com:8443".
# oc get nodes
NAME                                        STATUS    ROLES           AGE       VERSION
wsfd-netdev22.ntdv.lab.eng.bos.redhat.com   Ready     infra,master    43d       v1.10.0+b81c8f8
wsfd-netdev28.ntdv.lab.eng.bos.redhat.com   Ready     compute         43d       v1.10.0+b81c8f8
wsfd-netdev35.ntdv.lab.eng.bos.redhat.com   Ready     compute         43d       v1.10.0+b81c8f8
# oc get all
NAME                                  READY     STATUS    RESTARTS   AGE
pod/ovnkube-master-85bfb958f9-62dmq   4/4       Running   0          1h
pod/ovnkube-node-c5vp6                3/3       Running   0          1h
pod/ovnkube-node-fv589                3/3       Running   0          1h
pod/ovnkube-node-l8pps                3/3       Running   0          1h

NAME                     TYPE        CLUSTER-IP   EXTERNAL-IP   PORT(S)             AGE
service/ovnkube-master   ClusterIP   None         <none>        6641/TCP,6642/TCP   1h

NAME                          DESIRED   CURRENT   READY     UP-TO-DATE   AVAILABLE   NODE SELECTOR                 AGE
daemonset.apps/ovnkube-node   3         3         3         3            3           beta.kubernetes.io/os=linux   1h

NAME                             DESIRED   CURRENT   UP-TO-DATE   AVAILABLE   AGE
deployment.apps/ovnkube-master   1         1         1            1           1h

NAME                                        DESIRED   CURRENT   READY     AGE
replicaset.apps/ovnkube-master-85bfb958f9   1         1         1         1h
```

At this point ovn is providing networking for the cluster.

## Architecture:

OVN has a single master that is run on one node and node support that is
run on every node.

The master, ovnkube-master, selects the cluster master nodes. There can only be one OVN master,
however, the cluster can support multiple cluster masters. The OVN master runs in
a deployment with one replica. It selects from the cluster masters. The deployment
exports a headless service in which the end point "IP-OF-OVN-MASTER" is the running master.
Whenever the master is running the endpoint exists and points to the node running the OVN master.

The OVN node is run from a daemonset, ovnkube-node, with one pod on each node.

### ovnkube-master
The ovnkube-master has a single pod with a container for each process:

- run-ovn-northd
- nb-ovsdb
- sb-ovsdb
- ovnkube-master

This is done so that logs can be streamed to stdout and present with the "oc logs"
command. They also run independently and when on fails it is automatically restarted.

The top three contianers above are the ovn northd daemons. They run on the master and
all the nodes access them through:
```
# North db
tcp://<IP-OF-OVN-MASTER>:6441
# South db
tcp://<IP-OF-OVN-MASTER>:6442
```

The ovnkube-master container runs ovnkube in master mode.

### ovnkube-node
The ovnkube-node pod on each node has containers for:

- ovs-daemons
- ovn-controller
- ovn-node

The ovs-daemons container includes ovs-vswitchd and ovsdb-server for the ovs database.

The ovn-controller container runs ovnkube in --init-controller

The ovn-node container runs ovnkube in --init-node mode.

### Daemonset/Deployment - image dependency

There is a single image that supports all of the OVN pods. The image has a startup
script, ovnkube.sh, that is the entry point for each container. So the image and
daemonset are dependent on each other. The daemonset version environment variable,
OVN_DAEMONSET_VERSION, passes the version to ovnkube.sh.  The script has for support
for some of the daemonset versions (currently 1, 2, and 3).

This is important because the daemonset and image come from different build
streams and they can get out of sync.

## Images:

There is a single container image that is used in all of the ovn daemonsets.
The desired image is entered in the dist/ansible/hosts file.

Built images can be found in docker.io, one of the official OKD repositories or
a user provided image repository.

A development image can be built in the openvswitch/ovn-kubernetes git repo,
taged, and pushed to a private image repo.

The OCP image is built in the openshift/ose-ovn-kubernetes repo from rhel:7
with whatever openvswitch version is in the fastdatapath repo.
The default community image is built from centos:7 with openvswitch 2.9.2 from
http://cbs.centos.org/kojifiles/packages/.  The image can also
be built from fedora:28 with openvswitch from fedora.

The OCP image is available in the following:
```
registry.access.redhat.com/
brew-pulp-docker01.web.prod.ext.phx2.redhat.com:8888/
aws.openshift.com:443/
```
The OKD 4.0 image name includes the build tag:
```
openshift/ose-ovn-kubernetes:v4.0
```

The community image based on current development is:
```
docker.io/ovnkube/ovn-daemonset:latest
```
The the community image is the default. This image is updated from time to time
and may get out of fsync.


Alternatively, the image can be built from the ovn-kubernetes repo.
When doing this, edit the Makefile to tag and push the image to your existing
docker registry.  Edit the ansible/hosts file to reference the image.

1. build the ovn binaries, copy them to the Dockerfile directory and build the image,
tag and push it to your registry:
```
$ cd ovn-kubernetes/dist/images
$ make
```

or

```
$ make fedora
```

## Development Cycle:
In a development cycle, a new image can be built and pushed and the ansible
scripts can uninstall and reinstall ovn on the cluster.

```
# cd ansible
# ./run-playbook uninstall
# ./run-playbook
```

Alternatively once the daemonsets are running, the pods can be deleted. They
will be automatically created using the new image.
```
# oc project ovn-kubernetes
# oc get po
NAME                              READY     STATUS    RESTARTS   AGE
ovnkube-master-85bfb958f9-62dmq   4/4       Running   0          3h
ovnkube-node-c5vp6                3/3       Running   0          3h
ovnkube-node-fv589                3/3       Running   0          3h
ovnkube-node-l8pps                3/3       Running   0          3h
# oc delete po ovnkube-master-85bfb958f9-62dmq ovnkube-node-c5vp6 ovnkube-node-fv589 ovnkube-node-l8pps
```

## Debugging Aids

The ovnkube-node pod has the following containers: ovs-daemons ovn-controller
ovn-node The ovnkube-master pod has the following containers: run-ovn-northd
nb-ovsdb sb-ovsdb ovnkube-master

Logs from the containers can be viewed using the "kubectl logs" command. Each
container writes output to stdout and the results are displayed
using "kubectl logs". The log may be truncated but the full log is available
by rsh into the container and runnning ./ovnkube.sh display in the container
(see below).

The log is on a container basis so the logs can be shown using:
```
On each node pod:
# kubectl logs -c ovs-daemons ovnkube-node-c5vp6
# kubectl logs -c ovn-controller ovnkube-node-c5vp6
# kubectl logs -c ovn-node ovnkube-node-c5vp6

On each master pod:
# kubectl logs -c run-ovn-northd ovnkube-master-85bfb958f9-62dmq
# kubectl logs -c nb-ovsdb ovnkube-master-85bfb958f9-62dmq
# kubectl logs -c sb-ovsdb ovnkube-master-85bfb958f9-62dmq
# kubectl logs -c ovnkube-master ovnkube-master-85bfb958f9-62dmq
```
There is a convenience scripton the master, $HOME/ovn/ovn-logs that extracts logs
for all ovn pods in the cluster. The optional parameter will just display the
desired pod. The script is installed on the master by ./run-playbook.
```
# $HOME/ovn/ovn-logs
# $HOME/ovn/ovn-logs ovnkube-node-c5vp6
```


The full logs are available using:
```
# oc rsh -c ovs-daemons ovnkube-node-c5vp6 ./ovnkube.sh display
```
Where the container names and pods are as described above. In the following
the commit is the commit number that was built into the image. In the contianer
the /root/.git/* directories are copied from the github repo. This can be used
to match the image to a specific commit.

There is a convenience script, $HOME/ovn/ovn-display that extracts the complete logs
for all ovn pods in the cluster. The optional parameter will just display the
desired pod. The script is installed on the master by ./run-playbook.
```
# $HOME/ovn/ovn-display
# $HOME/ovn/ovn-display ovnkube-node-c5vp6
```

The display includes information on the image, daemonset and cluster in addition to the log. For example,
```
================== ovnkube.sh version 3 ================
 ==================== command: display
 =================== hostname: wsfd-netdev22.ntdv.lab.eng.bos.redhat.com
 =================== daemonset version 3
 =================== Image built from ovn-kubernetes ref: refs/heads/ovn-v3  commit: eacdf15c917e1bb49d06047711dab920d72af178
OVS_USER_ID root:root
OVS_OPTIONS
OVN_NORTH tcp://10.19.188.9:6641
OVN_NORTHD_OPTS --db-nb-sock=/var/run/openvswitch/ovnnb_db.sock --db-sb-sock=/var/run/openvswitch/ovnsb_db.sock
OVN_SOUTH tcp://10.19.188.9:6642
OVN_CONTROLLER_OPTS --ovn-controller-log=-vconsole:emer
OVN_NET_CIDR 10.128.0.0/14/24
OVN_SVC_CIDR 172.30.0.0/16
K8S_APISERVER https://wsfd-netdev22.ntdv.lab.eng.bos.redhat.com:8443
OVNKUBE_LOGLEVEL 4
OVN_DAEMONSET_VERSION 3
ovnkube.sh version 3
==================== display for wsfd-netdev22.ntdv.lab.eng.bos.redhat.com  ===================
Wed Nov 14 20:14:18 UTC 2018
====================== run-ovn-northd pid
10072
====================== run-ovn-northd log
2018-11-14T17:59:07.917Z|00001|vlog|INFO|opened log file /var/log/openvswitch/ovn-northd.log
2018-11-14T17:59:07.918Z|00002|reconnect|INFO|unix:/var/run/openvswitch/ovnnb_db.sock: connecting...
2018-11-14T17:59:07.918Z|00003|reconnect|INFO|unix:/var/run/openvswitch/ovnnb_db.sock: connection attempt failed (Connection refused)
...
```

The ovn configuration for each of the ovn pods can be extracted using:
```
# oc rsh -c ovs-daemons ovnkube-node-c5vp6 ./ovnkube.sh ovn_debug
```
Where the container names and pods are as described above.

There is a convenience script, $HOME/ovn/ovn-debug that extracts the complete logs
for all ovn pods in the cluster. The optional parameter will just display the
desired pod. The script is installed on the master by ./run-playbook.
```
# $HOME/ovn/ovn-debug
# $HOME/ovn/ovn-debug ovnkube-node-c5vp6
```

The reported ovn configuration includes:
- ovn-nbctl show
- ovn-nbctl list ACL
- ovn-nbctl list address_set
- ovs-vsctl show
- ovs-ofctl -O OpenFlow13 dump-ports br-int
- ovs-ofctl -O OpenFlow13 dump-ports-desc br-int
- ovs-ofctl dump-flows br-int

On the Master:
- ovn-sbctl show
- ovn-sbctl lflow-list
- ovn-sbctl list datapath
- ovn-sbctl list port

