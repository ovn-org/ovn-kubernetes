If the ovn-northd instance is not running on the same node as OVN NB
and OVN SB database, then you will need to follow the steps below to
secure the communication between ovn-northd and NB/SB databases.

## Generate signed certificates for OVN Northd
On the node running ovn-northd daemon, run the following commands.
```
cd /etc/openvswitch
ovs-pki req ovnnorthd
```

The above command will generate a private key "ovnnorthd-privkey.pem"
and a corresponding certificate request named "ovnnorthd-req.pem". Copy over
the ovnnorthd-req.pem to the secure machine's (where you have installed the
ovs-pki utility) /var/lib/openvswitch/pki folder to sign it using the
command below.

```
ovs-pki -b sign ovnnorthd
```

The above command will generate ovnnorthd-cert.pem. Copy over this file back
to the ovn-northd node's /etc/openvswitch foler. The ovnnorthd-privkey.pem and
ovnnorthd-cert.pem will be used by the ovn-northd to communicate with OVN NB 
and SB database.

Now run the following command to ask ovn-northd to use these
certificates.

```
cat > /etc/openvswitch/ovn-northd-db-params.conf << EOF
--ovnnb-db=ssl:$CENTRAL_IP:6641 --ovnsb-db=ssl:$CENTRAL_IP:6642 \
-p /etc/openvswitch/ovnnorthd-privkey.pem -c /etc/openvswitch/ovnnorthd-cert.pem \
-C /etc/openvswitch/cacert.pem
EOF

/usr/share/openvswitch/scripts/ovn-ctl restart_northd
```