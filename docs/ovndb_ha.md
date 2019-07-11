# OVN Northbound/Southbound DB HA using Corosync/Pacemaker

This document describes how to start OVN DBs in Active/Standby
HA mode using [Corosync] and [Pacemaker]. This HA mode is supported by OVS
community and is widely used. For more information on how the HA itself
is done please read the information here:

http://docs.openvswitch.org/en/latest/topics/integration/#ha-for-ovn-db-servers-using-pacemaker

The following diagram captures the setup for this type of HA. The ovnkube-db
deployment has replica set to 3 and the ovnkube-db pods will end up on those
nodes that have label set to openvswitch.org/ovnkube-db=true.

![alt-txt](ovndb_ha_active_stdby.svg)

The OVN NB/SB DB is available behind a Virtual IP which should be from the
K8s Node subnet and must be provided by the user. Both the Northbound/Southbound
DBs run in a single container in the ovnkube-db pod. This is done to simplify
setting up corosync/pacemaker cluster since we just need one to run both the DBs.
 
Generate the ovnkube-db deployment
yaml file from the dist/templates/ovnkube-db-ha.yam.j2 template file by running

```
export VIRTUAL_IP=<free_ip_address_from_k8s_node_subnet>
cd $HOME/work/src/github.com/ovn-org/ovn-kubernetes/dist/images
./daemonset.sh --image=docker.io/ovnkube/ovn-daemonset-u:latest \
    --k8s-apiserver=https://$MASTER_IP:6443 \
    --net-cidr=192.168.0.0/16 --svc-cidr=172.16.1.0/24 \
    --db-replicas=3 --db-ha-vip=$VIRTUAL_IP
    
# Create OVN namespace, service accounts, ovnkube-db headless service, configmap, and policies
kubectl create -f $HOME/work/src/github.com/ovn-org/ovn-kubernetes/dist/yaml/ovn-setup.yaml

# Run ovnkube-db deployment.
kubectl create -f $HOME/work/src/github.com/ovn-org/ovn-kubernetes/dist/yaml/ovnkube-db-ha.yaml

# Run ovnkube-master deployment.
kubectl create -f $HOME/work/src/github.com/ovn-org/ovn-kubernetes/dist/yaml/ovnkube-master.yaml

# Run ovnkube daemonsets for nodes
kubectl create -f $HOME/work/src/github.com/ovn-org/ovn-kubernetes/dist/yaml/ovnkube-node.yaml
```

All of the OVN DB clients will now interact with the OVN DB using the VIRTUAL_IP. They get to
know about this VIRTUAL_IP using a headless service without the name selector. For more
information on how this is orchestrated, please read:

https://docs.google.com/document/d/1khkgTNvjSEKgOSrzJOTtApL_mai-f3-pW1NHXOv5Dak/edit?usp=sharing

[Corosync]: https://clusterlabs.org/corosync.html
[Pacemaker]: https://clusterlabs.org/pacemaker/



