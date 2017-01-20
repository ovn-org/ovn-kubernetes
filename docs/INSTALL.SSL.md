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
certificate authority to sign individual certificates of all the minions.  We
will then use the same certificate authority's certificate to verify minion's
connections to the master.

Copy this certificate to the master node. $CENTRAL_IP is the IP address of the
master node.

```
scp /var/lib/openvswitch/pki/switchca/cacert.pem \
    root@$CENTRAL_IP:/etc/openvswitch/.
```

### Generate self-signed certificates for the master node.

On the master node, run the following commands.

```
cd /etc/openvswitch
ovs-pki req ovnnb && ovs-pki self-sign ovnnb
```

The above commands will generate a private key "ovnnb-privkey.pem"
and create a self signed certificate "ovnnb-cert.pem".  These will
be used by the ovsdb-server that fronts the OVN NB database.

Now run the following command to ask ovsdb-server to use these
certificates.

```
ovn-nbctl set-ssl /etc/openvswitch/ovnnb-privkey.pem \
    /etc/openvswitch/ovnnb-cert.pem  /etc/openvswitch/cacert.pem
```

```
ovs-pki req ovnsb && sudo ovs-pki self-sign ovnsb
```

The above commands will generate a private key "ovnsb-privkey.pem"
and create a self signed certificate "ovnsb-cert.pem".  These will
be used by the ovsdb-server that fronts the OVN SB database.

Now run the following command to ask ovsdb-server to use these
certificates.

```
ovn-sbctl set-ssl /etc/openvswitch/ovnsb-privkey.pem \
    /etc/openvswitch/ovnsb-cert.pem  /etc/openvswitch/cacert.pem
```

### Generate certificates for the minion.

On each minion, create a certificate request.

```
cd /etc/openvswitch
ovs-pki req ovncontroller
```

The above command will create a new private key for the minion called
ovncontroller-privkey.pem and a certificate request file called
ovncontroller-req.pem.  Copy this certificate request file to the secure
machine where you created the certificate authority and from the directory
where the copied file exists, run:

```
ovs-pki -b sign ovncontroller switch
```

The above will create the certificate for the minion called
"ovncontroller-cert.pem". You should copy this certificate back to the
minion's /etc/openvswitch directory.

## One time setup.

As explained in [README.md], OVN architecture has a central component which
stores your networking intent in a database.  You start this central component
on the node where you intend to start your k8s central daemons by running:

```
/usr/share/openvswitch/scripts/ovn-ctl start_northd
```

Now, you need to additionally run the following commands to open up SSL ports
via which the database can be accessed.

```
ovn-nbctl set-connection ssl:6641
ovn-sbctl set-connection ssl:6642
```

On each host in your cluster (including the master node), you will need to run
the following command.

```
ovs-vsctl set Open_vSwitch . external_ids:ovn-remote="ssl:$CENTRAL_IP:6642" \
  external_ids:ovn-nb="ssl:$CENTRAL_IP:6641"
```

You should now restart the ovn-controller on each host with the following
additional options.

```
/usr/share/openvswitch/scripts/ovn-ctl \
    --ovn-controller-ssl-key="/etc/openvswitch/ovncontroller-privkey.pem"  \
    --ovn-controller-ssl-cert="/etc/openvswitch/ovncontroller-cert.pem"    \
    --ovn-controller-ssl-bootstrap-ca-cert="/etc/openvswitch/ovnsb-ca.cert" \
    restart_controller
```

To make sure that ovn-controller restarts with the above options during system
reboots, you should add the above options to your startup script's defaults
file.  For e.g. on Ubuntu, if you installed ovn-controller via the package
'ovn-host*.deb', write the following to your /etc/default/ovn-host file

```
OVN_CTL_OPTS="--ovn-controller-ssl-key=/etc/openvswitch/ovncontroller-privkey.pem  --ovn-controller-ssl-cert=/etc/openvswitch/ovncontroller-cert.pem --ovn-controller-ssl-bootstrap-ca-cert=/etc/openvswitch/ovnsb-ca.cert"
```


[README.md]: README.md
[OVS.SSL]: http://docs.openvswitch.org/en/latest/howto/ssl
