This document explains the way one could use SSL for connectivity between OVN
components.  The details in this documentation needs a minimum version of
Open vSwitch 2.7.

## Generating certificates

Detailed explanation of how OVS utilites and daemons use SSL certificates is
explained at [OVS.SSL].  The following section summarizes it for
ovn-kubernetes.

### Create a certificate authority.

On a secure machine (separate from your k8s cluster), where you have installed
the ovs-pki utility (comes with OVS installations), create a certificate
authority by running

```
ovs-pki init --force
```

The above command creates 2 certificate authorities.  But we are concerned only
with one of them, i.e the "switch" certificate authority.  We will use this
certificate authority to sign individual certificates of all the nodes.  We
will then use the same certificate authority's certificate to verify a node's
connections to the master.

Copy this certificate to the master and each of the nodes. $CENTRAL_IP
is the IP address of the master.

```
scp /var/lib/openvswitch/pki/switchca/cacert.pem \
    root@$CENTRAL_IP:/etc/openvswitch/.
```

### Generate signed certificates for OVN components running on the master.

#### Generate signed certificates for OVN NB Database
On the master, run the following commands.

```
cd /etc/openvswitch
ovs-pki req ovnnb
```

The above command will generate a private key "ovnnb-privkey.pem"
and a corresponding certificate request named "ovnnb-req.pem". Copy over
the ovnnb-req.pem to the aforementioned secure machine's /var/lib/openvswitch/pki
folder to sign it using the command below.

```
ovs-pki -b sign ovnnb
```

The above command will generate ovnnb-cert.pem. Copy over this file back
to the master's /etc/openvswitch. The ovnnb-privkey.pem and ovnnb-cert.pem
will be used by the ovsdb-server that fronts the OVN NB database.

Now run the following commands to ask ovsdb-server to use these
certificates and also to open up SSL ports via which the database
can be accessed.

```
ovn-nbctl set-ssl /etc/openvswitch/ovnnb-privkey.pem \
    /etc/openvswitch/ovnnb-cert.pem  /etc/openvswitch/cacert.pem

ovn-nbctl set-connection pssl:6641
```

#### Generate signed certificates for OVN SB Database
```
cd /etc/openvswitch
ovs-pki req ovnsb
```

The above command will generate a private key "ovnsb-privkey.pem"
and a corresponding certificate request named "ovnsb-req.pem". Copy over
the ovnsb-req.pem to the aforementioned secure machine's /var/lib/openvswitch/pki
to sign it using the command below.

```
ovs-pki -b sign ovnsb
```

The above command will generate ovnsb-cert.pem. Copy over this file back
to the master's /etc/openvswitch. The ovnsb-privkey.pem and ovnsb-cert.pem
will be used by the ovsdb-server that fronts the OVN SB database.

Now run the following commands to ask ovsdb-server to use these
certificates  and also to open up SSL ports via which the database
can be accessed.

```
ovn-sbctl set-ssl /etc/openvswitch/ovnsb-privkey.pem \
    /etc/openvswitch/ovnsb-cert.pem  /etc/openvswitch/cacert.pem

ovn-sbctl set-connection pssl:6642
```

#### Generate signed certificates for OVN Northd

If you are running ovn-northd on the same host as the OVN NB and SB database servers, then
there is no need to secure the communication between ovn-northd and OVN NB/SB daemons.
ovn-northd will communicate using UNIX path.

In case you still want to secure the communication, or the daemons are running on
separate hosts, then follow the instructions on this page [OVN-NORTHD.SSL.md]


### Generate certificates for the nodes

On each node, create a certificate request.

```
cd /etc/openvswitch
ovs-pki req ovncontroller
```

The above command will create a new private key for the node called
ovncontroller-privkey.pem and a certificate request file called
ovncontroller-req.pem.  Copy this certificate request file to the secure
machine where you created the certificate authority and from the directory
where the copied file exists, run:

```
ovs-pki -b sign ovncontroller switch
```

The above will create the certificate for the node called
"ovncontroller-cert.pem". You should copy this certificate back to the
node's /etc/openvswitch directory.

Additionally, the common name used for signing the certificates (`ovncontroller`)
needs to be passed in for TLS server certificate verification using the 
`-nb-cert-common-name` and the `-sb-cert-common-name` CLI options.

## One time setup.

As explained in [README.md], OVN architecture has a central component which
stores your networking intent in a database.  You start this central component
on the node where you intend to start your k8s central daemons by running:

```
/usr/share/openvswitch/scripts/ovn-ctl start_northd
```

You should now restart the ovn-controller on each host with the following
additional options.

```
/usr/share/openvswitch/scripts/ovn-ctl \
    --ovn-controller-ssl-key="/etc/openvswitch/ovncontroller-privkey.pem"  \
    --ovn-controller-ssl-cert="/etc/openvswitch/ovncontroller-cert.pem"    \
    --ovn-controller-ssl-ca-cert="/etc/openvswitch/cacert.pem" \
    restart_controller
```

To make sure that ovn-controller restarts with the above options during system
reboots, you should add the above options to your startup script's defaults
file.  For e.g. on Ubuntu, if you installed ovn-controller via the package
'ovn-host*.deb', write the following to your /etc/default/ovn-host file

```
OVN_CTL_OPTS="--ovn-controller-ssl-key=/etc/openvswitch/ovncontroller-privkey.pem  --ovn-controller-ssl-cert=/etc/openvswitch/ovncontroller-cert.pem --ovn-controller-ssl-ca-cert=/etc/openvswitch/cacert.pem"
```

Now, when you start the ovnkube utility on master, you should pass the SSL
certificates to it. For e.g:

```
sudo ovnkube -k8s-kubeconfig kubeconfig.yaml -loglevel=4 \
 -k8s-apiserver="http://$CENTRAL_IP:8080" \
 -logfile="/var/log/ovn-kubernetes/ovnkube.log" \
 -init-master="$NODE_NAME" -cluster-subnets=$CLUSTER_IP_SUBNET \
 -k8s-service-cidr=$SERVICE_IP_SUBNET \
 -nodeport \
 -nb-address="ssl:$CENTRAL_IP:6641" \
 -sb-address="ssl:$CENTRAL_IP:6642" \
 -nb-client-privkey /etc/openvswitch/ovncontroller-privkey.pem \
 -nb-client-cert /etc/openvswitch/ovncontroller-cert.pem \
 -nb-client-cacert /etc/openvswitch/cacert.pem \
 -nb-cert-common-name ovncontroller \
 -sb-client-privkey /etc/openvswitch/ovncontroller-privkey.pem \
 -sb-client-cert /etc/openvswitch/ovncontroller-cert.pem \
 -sb-client-cacert /etc/openvswitch/cacert.pem \
 -sb-cert-common-name ovncontroller 
```

And when you start your ovnkube utility on nodes, you should pass the SSL
certificates to it. For e.g:

```
sudo ovnkube -k8s-kubeconfig $HOME/kubeconfig.yaml -loglevel=4 \
    -k8s-apiserver="http://$CENTRAL_IP:8080" \
    -init-node="$NODE_NAME"  \
    -nodeport \
    -nb-address="ssl:$CENTRAL_IP:6641" \
    -sb-address="ssl:$CENTRAL_IP:6642" -k8s-token=$TOKEN \
    -nb-client-privkey /etc/openvswitch/ovncontroller-privkey.pem \
    -nb-client-cert /etc/openvswitch/ovncontroller-cert.pem \
    -nb-client-cacert /etc/openvswitch/cacert.pem \
    -nb-cert-common-name ovncontroller \
    -sb-client-privkey /etc/openvswitch/ovncontroller-privkey.pem \
    -sb-client-cert /etc/openvswitch/ovncontroller-cert.pem \
    -sb-client-cacert /etc/openvswitch/cacert.pem \
    -sb-cert-common-name ovncontroller \
    -init-gateways \
    -k8s-service-cidr=$SERVICE_IP_SUBNET \
    -cluster-subnets=$CLUSTER_IP_SUBNET
```

[README.md]: README.md
[OVS.SSL]: http://docs.openvswitch.org/en/latest/howto/ssl
