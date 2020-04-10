# OVN central database High-availability

OVN architecture has two central databases that can be clustered.
The databases are OVN_Northbound and OVN_Southbound.  This document
explains how to cluster them and start various daemons for the
ovn-kubernetes integration.  You will ideally need at least 3 masters
for a HA cluster. (You will need a miniumum of OVS/OVN 2.9.2
for clustering.)

## Master1 initialization

To bootstrap your cluster, you need to start on one master.
For a lack of better name, let's call it MASTER1 with an IP
address of $MASTER1

On MASTER1, delete any stale OVN databases and stop any
ovn-northd running. e.g:

```
sudo /usr/share/openvswitch/scripts/ovn-ctl stop_nb_ovsdb
sudo /usr/share/openvswitch/scripts/ovn-ctl stop_sb_ovsdb
sudo rm /etc/openvswitch/ovn*.db
sudo /usr/share/openvswitch/scripts/ovn-ctl stop_northd
```

Start the two databases on that host with:

```
LOCAL_IP=$MASTER1
sudo /usr/share/openvswitch/scripts/ovn-ctl \
    --db-nb-cluster-local-addr=$LOCAL_IP start_nb_ovsdb

sudo /usr/share/openvswitch/scripts/ovn-ctl \
    --db-sb-cluster-local-addr=$LOCAL_IP start_sb_ovsdb
```


## Master2, Master3... initialization

Delete any stale databases and stop any running ovn-northd
daemons. e.g:

```
sudo /usr/share/openvswitch/scripts/ovn-ctl stop_nb_ovsdb
sudo /usr/share/openvswitch/scripts/ovn-ctl stop_sb_ovsdb
sudo rm /etc/openvswitch/ovn*.db
sudo /usr/share/openvswitch/scripts/ovn-ctl stop_northd
```

On master with a IP of $LOCAL_IP, start the databases and ask it to
join $MASTER1

```
LOCAL_IP=$LOCAL_IP
MASTER_IP=$MASTER1

sudo /usr/share/openvswitch/scripts/ovn-ctl  \
    --db-nb-cluster-local-addr=$LOCAL_IP \
    --db-nb-cluster-remote-addr=$MASTER_IP start_nb_ovsdb

sudo /usr/share/openvswitch/scripts/ovn-ctl  \
    --db-sb-cluster-local-addr=$LOCAL_IP \
    --db-sb-cluster-remote-addr=$MASTER_IP start_sb_ovsdb
```

This should get your cluster up and running. You can verify the
status of your cluster with:

```
sudo ovs-appctl -t /var/run/openvswitch/ovnnb_db.ctl \
    cluster/status OVN_Northbound

sudo ovs-appctl -t /var/run/openvswitch/ovnsb_db.ctl \
    cluster/status OVN_Southbound
```

## Start 'ovn-kube -init-master'

On any one of the masters, we need to start 'ovnkube -init-master'.
(This should ideally be a daemonset with replica count of 1.)

IP1="$MASTER1"
IP2="$MASTER2"
IP3="$MASTER3"

ovn_nb="tcp:$IP1:6641,tcp:$IP2:6641,tcp:$IP3:6641"
ovn_sb="tcp:$IP1:6642,tcp:$IP2:6642,tcp:$IP3:6642"

nohup sudo ovnkube -k8s-kubeconfig kubeconfig.yaml \
 -loglevel=4 \
 -k8s-apiserver="http://$K8S_APISERVER_IP:8080" \
 -logfile="/var/log/openvswitch/ovnkube.log" \
 -init-master="$NODENAME" -cluster-subnets="$CLUSTER_IP_SUBNET" \
 -init-node="$NODENAME" \
 -k8s-service-cidr="$SERVICE_IP_SUBNET" \
 -k8s-token="$TOKEN" \
 -nodeport \
 -nb-address="${ovn_nb}" \
 -sb-address="${ovn_sb}"  2>&1 &

## start ovn-northd

On any one of the masters (ideally via a daemonset with replica count as 1),
start ovn-northd. Let the 3 master IPs be $IP1, $IP2 and $IP3.

```
IP1="$MASTER1"
IP2="$MASTER2"
IP3="$MASTER3"

export ovn_nb="tcp:$IP1:6641,tcp:$IP2:6641,tcp:$IP3:6641"
export ovn_sb="tcp:$IP1:6642,tcp:$IP2:6642,tcp:$IP3:6642"

sudo ovn-northd -vconsole:emer -vsyslog:err -vfile:info \
    --ovnnb-db="$ovn_nb" --ovnsb-db="$ovn_sb" --no-chdir \
    --log-file=/var/log/openvswitch/ovn-northd.log \
    --pidfile=/var/run/openvswitch/ovn-northd.pid --detach --monitor
```

## Start 'ovn-kube -init-node'

On all nodes (and if needed on other masters), start ovnkube with
'-init-node'. For e.g:

```
IP1="$MASTER1"
IP2="$MASTER2"
IP3="$MASTER3"

ovn_nb="tcp:$IP1:6641,tcp:$IP2:6641,tcp:$IP3:6641"
ovn_sb="tcp:$IP1:6642,tcp:$IP2:6642,tcp:$IP3:6642"

nohup sudo ovnkube -k8s-kubeconfig $HOME/kubeconfig.yaml -loglevel=4 \
    -logfile="/var/log/openvswitch/ovnkube.log" \
    -k8s-apiserver="http://$K8S_APISERVER_IP:8080" \
    -init-node="$NODE_NAME"  \
    -nb-address="${ovn_nb}" \
    -sb-address="${ovn_sb}" \
    -k8s-token="$TOKEN" \
    -init-gateways \
    -k8s-service-cidr= \
    -cluster-subnets="$SERVICE_IP_SUBNET" 2>&1 &
```
