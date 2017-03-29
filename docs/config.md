# Config variables

## Initial setup

If you want to override the default values for some config options, then the
file available in this repo (etc/ovn_k8s.conf) must be copied to the following
locations:

- on Linux:
```
/etc/openvswitch/ovn_k8s.conf
```
The following command copies the config file if it is run from inside the repo:
```
cp etc/ovn_k8s.conf /etc/openvswitch/ovn_k8s.conf
```

- on Windows:
```
C:\etc\ovn_k8s.conf
```
The following PowerShell command copies the config file if is run from inside
the repo:
```
Copy-Item ".\etc\ovn_k8s.conf" -Destination (New-Item "C:\etc" -Type container -Force)
```

## Config values

The following config option represents the MTU value which should be used
for the overlay networks.
```
mtu=1400
```

The following option affects only the gateway nodes. This value is used to
track connections that are initiated from the pods so that the reverse
connections go back to the pods. This represents the conntrack zone used
for the conntrack flow rules.
```
conntrack_zone=64000
```

The mode in which ovn-kubernetes should operate. This value can be overlay or
underlay. At the moment only overlay networks are supported.
```
ovn_mode=overlay
```

The following config values are used for the CNI plugin.
```
log_path=/var/log/openvswitch/ovn-k8s-cni-overlay.log
unix_socket=/var/run/openvswitch/ovnnb_db.sock
cni_conf_path=/etc/cni/net.d
cni_link_path=/opt/cni/bin/
cni_plugin=ovn-k8s-cni-overlay
```

OVN and K8S certificates are stored in the following options.
```
private_key=/etc/openvswitch/ovncontroller-privkey.pem
certificate=/etc/openvswitch/ovncontroller-cert.pem
ca_cert=/etc/openvswitch/ovnnb-ca.cert
k8s_ca_certificate=/etc/openvswitch/k8s-ca.crt
```

We do not know how OVS core utilities have been installed. If those values
are not set, we try to do a best guess for rundir / logdir and choose between
"/var/run/openvswitch" / "/var/log/openvswitch" and
"/usr/local/var/run/openvswitch" / "/usr/local/var/log/openvswitch"
```
rundir=
logdir=
```
