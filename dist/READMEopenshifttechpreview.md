# OVN tech preview on OpenShift/origin/kubernetes

NOTE:
- This is a temporary approach to working with the tech preview. Ultimately
4.0 will install using the network operator.

You can preview ovn overlay networks on an OpenShift 4.0 cluster by replacing
the installed CNI plugin with OVN.
A v3.11 cluster installs with no plugin and 4.0 (currently) installs flannel.

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
# git clone https://github.com/openvswitch/ovn-kubernetes
# cd ovn-kubernetes/dist/ansible
```

Edit the hosts file adding the name of the cluster master and select the ovn_image
and ovn_image_pull_policy. The default is the community image.

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
# oc get ds
NAME             DESIRED   CURRENT   READY     UP-TO-DATE   AVAILABLE   NODE SELECTOR                         AGE
ovnkube          3         3         3         3            3           beta.kubernetes.io/os=linux                                       1h
ovnkube-master   1         1         1         1            1           beta.kubernetes.io/os=linux,node-role.kubernetes.io/master=true   1h

# oc get po
NAME                   READY     STATUS    RESTARTS   AGE
ovnkube-cggxz          3/3       Running   0          1h
ovnkube-ltpzr          3/3       Running   0          1h
ovnkube-master-mj55x   2/2       Running   0          1h
ovnkube-pdw7h          3/3       Running   0          1h
```

At this point ovn is providing networking for the cluster.

## Images:

There is a single docker image that is used in all of the ovn daemonsets.
The desired image is entered in the hosts file.

Images can be found in docker.io, one of the official OKD repositories or
a user provided image repository.

A development image can be built in the openvswitch/ovn-kubernetes git repo
and taged and pushed to a private image repo.

The OCP image is built in the openshift/ose-ovn-kubernetes repo from rhel:7
with openvswitch from the fastdatapath repo.
The default community image is built from centos:7 with openvswitch from
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
openshift3/ose-ovn-kubernetes:v4.0
```

The community image based on current development is:
```
docker.io/ovnkube/ovn-daemonset:latest
```
The the community image is the default.


Alternatively, the image can be built from the ovn-kubernetes repo.
When doing this edit the Makefile to tag and push the image to your existing
docker registry.  Edit the hosts file to reference the image.

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
NAME                   READY     STATUS    RESTARTS   AGE
ovnkube-6sk8k          3/3       Running   0          1m
ovnkube-9gc8j          3/3       Running   0          1m
ovnkube-ks8kt          3/3       Running   0          1m
ovnkube-master-4nnkh   4/4       Running   0          1m
# oc delete po ovnkube-6sk8k ovnkube-9gc8j ovnkube-ks8kt ovnkube-master-4nnkh
```

##Debugging Aids

The ovnkube pod has the following containers: ovs-daemons ovn-controller
ovn-node The ovnkube-master pod has the following containers: run-ovn-northd
nb-ovsdb sb-ovsdb ovnkube-master

Logs from the containers can be viewed using the "kubectl logs" command. Each
container does a "tail -f" of its log file and the results are displayed
using "oc logs". The log may be truncated but the full log is available
by rsh into the container and runnning ./ovnkube.sh display in the container
(see below).

The log is on a container basis so the logs can be shown using:
```
On each node pod:
# kubectl logs -c ovs-daemons ovnkube-6sk8k
# kubectl logs -c ovn-controller ovnkube-6sk8k
# kubectl logs -c ovn-node ovnkube-6sk8k

On each master pod:
# kubectl logs -c run-ovn-northd ovnkube-master-4nnkh
# kubectl logs -c nb-ovsdb ovnkube-master-4nnkh
# kubectl logs -c sb-ovsdb ovnkube-master-4nnkh
# kubectl logs -c ovnkube-master ovnkube-master-4nnkh
```
There is a convenience script, $HOME/ovn/ovn-logs that extracts logs
for all ovn pods in the cluster. The optional parameter will just display the
desired pod. The script is installed on the master by ./run-playbook.
```
# $HOME/ovn/ovn-logs
# $HOME/ovn/ovn-logs ovnkube-6sk8k
```


The full logs are available using:
```
# oc rsh -c ovs-daemons ovnkube-6sk8k ./ovnkube.sh display
```
Where the container names and pods are as described above.

There is a convenience script, $HOME/ovn/ovn-display that extracts the complete logs
for all ovn pods in the cluster. The optional parameter will just display the
desired pod. The script is installed on the master by ./run-playbook.
```
# $HOME/ovn/ovn-display
# $HOME/ovn/ovn-display ovnkube-6sk8k
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
# oc rsh -c ovs-daemons ovnkube-6sk8k ./ovnkube.sh ovn_debug
```
Where the container names and pods are as described above.

There is a convenience script, $HOME/ovn/ovn-debug that extracts the complete logs
for all ovn pods in the cluster. The optional parameter will just display the
desired pod. The script is installed on the master by ./run-playbook.
```
# $HOME/ovn/ovn-debug
# $HOME/ovn/ovn-debug ovnkube-6sk8k
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

