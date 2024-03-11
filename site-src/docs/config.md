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

The config file contains common configuration options shared between the various
ovn-kubernetes programs (ovnkube, ovn-k8s-cni-overlay, etc).  All configuration
file options can also be specified as command-line arguments which override
config file options; see the -help output of each program for more details.

### [default] section

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
conntrack-zone=64000
```

The following option only affects ovn-controller. This is the maximum number
of milliseconds of idle time on connection to the server before sending an
inactivity probe message.  As a client connects to the server over TCP, it
may take a while for the kernel to figure out if the connection is
broken.  But the client can overcome this by periodically sending probes over
the TCP connection to make sure that the connection is up.  On the flip side,
when there are hundreds of nodes, the server can get bogged down by client
probe messages.

The default value is set as 100000ms. But can be changed with this config
option

inactivity-probe=600000

### [logging] section

The following config values control what verbosity level logging is written at
and to what file (if any).
```
loglevel=5
logfile=/var/log/ovnkube.log
```

### [cni] section

The following config values are used for the CNI plugin.
```
conf-dir=/etc/cni/net.d
plugin=ovn-k8s-cni-overlay
```

### [kubernetes] section

Kubernetes API options are stored in the following section.
```
apiserver=https://1.2.3.4:6443
token=TG9yZW0gaXBzdW0gZG9sb3Igc2l0IGFtZXQsIGNvbnNlY3RldHVyIGFkaXBpc2NpbmcgZWxpdC4gQ3JhcyBhdCB1bHRyaWNpZXMgZWxpdC4gVXQgc2l0IGFtZXQgdm9sdXRwYXQgbnVuYy4K
cacert=/etc/kubernetes/ca.crt
```

### [ovnnorth] section

This section contains the address and (if the 'ssl' method is used) certificates
needed to use the OVN northbound database API. Only the the ovn-kubernetes
master needs to specify the 'server' options.
```
address=ssl:1.2.3.4:6641
client-privkey=/path/to/private.key
client-cert=/path/to/client.crt
client-cacert=/path/to/client-ca.crt
server-privkey=/path/to/private.key
server-cert=/path/to/server.crt
server-cacert=/path/to/server-ca.crt
```

### [ovnsouth] section

This section contains the address and (if the 'ssl' method is used) certificates
needed to use the OVN southbound database API. Only the the ovn-kubernetes
master needs to specify the 'server' options.
```
address=ssl:1.2.3.4:6642
client-privkey=/path/to/private.key
client-cert=/path/to/client.crt
client-cacert=/path/to/client-ca.crt
server-privkey=/path/to/private.key
server-cert=/path/to/server.crt
server-cacert=/path/to/server-ca.crt
```
