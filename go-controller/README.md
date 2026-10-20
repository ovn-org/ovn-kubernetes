# ovn-kubernetes go-controller

The golang based ovn controller is a reliable way to deploy the OVN SDN using kubernetes clients and watchers based on golang. 

### Build

Ensure go version >= 1.16

```
cd go-controller
make clean
make
```

Then find the executables here : go-controller/_output/go/bin/

'ovnkube' is the single all-in-one executable useful for deploying the ovn cni plugin for a kubernetes deployment

'ovn-k8s-cni-overlay' is the cni executable to be placed in /opt/cni/bin (or another directory in which kubernetes will look for the plugin) so that it can be invoked for each pod event by kubernetes

To get the Windows version of 'ovnkube' and 'ovn-k8s-cni-overlay' run the following:
```
cd go-controller
make clean
make windows
```

Then find the executables here : go-controller/_output/go/windows/

### Usage

Run the 'ovnkube' executable to initialize the control plane and nodes that build the network as pods, services, and ingress objects are created in kubernetes.
Options specified on the command-line override configuration file options which override environment variables. Environment variables override service account files.

```
Usage:
  -config-file string
     configuration file path (default: /etc/openvswitch/ovn_k8s.conf)
  -cluster-subnets string
     cluster-wide IP subnet to use (default: 11.11.0.0/16)
  -init-cluster-manager string
     initialize cluster manager that watches nodes (allocates subnet for each node from the cluster-subnets), requires the hostname as argument and doesn't connect to the OVN dbs.
  -init-ovnkube-controller string
     initialize ovnkube-controller (which watches pods/nodes/services/policies and creates OVN db resources), requires the Kubernetes node name as argument.
  -init-node string
     initialize node, requires the name under which the node is registered in the Kubernetes cluster
  -cleanup-node string
     clean up OVS resources on the k8s node (after ovnkube-node daemonset deletion), requires the name under which the node is registered in the Kubernetes cluster
  -remove-node string
     remove a node from the OVN cluster. Requires the name that node is
     registered with in kubernetes cluster
  -mtu int
     MTU value used for the overlay networks (default: 1400)
  -conntrack-zone int
     for gateway nodes, the conntrack zone used for conntrack flow rules (default: 64000)
  -loglevel int
     log verbosity and level: 5=debug, 4=info, 3=warn, 2=error, 1=fatal (default: 4)
  -logfile string
     path of a file to direct log output to
  -cni-conf-dir string
     the CNI config directory in which to write the overlay CNI config file (default: /etc/cni/net.d)
  -cni-plugin string
     the name of the CNI plugin (default: ovn-k8s-cni-overlay)
  -k8s-kubeconfig string
     absolute path to the Kubernetes kubeconfig file (not required if the --k8s-apiserver, --k8s-cacert, and --k8s-token are given)
  -k8s-apiserver string
     URL of the Kubernetes API server (not required if --k8s-kubeconfig is given) (default: http://localhost:8443)
  -k8s-cacert string
     the absolute path to the Kubernetes API CA certificate (not required if --k8s-kubeconfig is given)
  -k8s-token string
     the Kubernetes API authentication token (not required if --k8s-kubeconfig is given)
  -nb-client-privkey string
     Private key used by the node-to-node Egress IP gRPC health-check channel (historical name; no longer used for OVN DB connection).
  -nb-client-cert string
     Client certificate used by the node-to-node Egress IP gRPC health-check channel (historical name; no longer used for OVN DB connection).
  -nb-client-cacert string
     CA certificate used by the node-to-node Egress IP gRPC health-check channel (historical name; no longer used for OVN DB connection).
```

### Environment Variables
Values for some configuration options can be passed in environment variables. 

```
  -KUBECONFIG
     absolute path to the Kubernetes kubeconfig file overridden by --k8s-kubeconfig
  -K8S_APISERVER
     URL of the Kubernetes API server overridden by --k8s-apiserver
  -K8S_CACERT
     the absolute path to the Kubernetes API CA certificate overridden by --k8s-cacert
  -K8S_TOKEN
     the Kubernetes API authentication token overridden by --k8s-token
```

### Service Account Files
The cluster makes the serviceaccount ca.crt and token available in files that are mounted in each container.

```
  -/var/run/secrets/kubernetes.io/serviceaccount/ca.crt
     contains the Kubernetes API CA certificate overridden by --k8s-cacert and K8S_CACERT.
  -/var/run/secrets/kubernetes.io/serviceaccount/token
     contains the Kubernetes API token overridden by --k8s-token and K8S_TOKEN
```

### Configuration File

Generic configuration options (those common between control plane, node, and gateway modes) can also be specified in the configuration file.
The default configuration file path is /etc/openvswitch/ovn_k8s.conf but can be changed with the -config-file option.
Options specified in the config file override default values, but are themselves overridden by command-line options.

#### Example configuration file

```
[default]
mtu=1500
conntrack-zone=64321

[kubernetes]
apiserver=https://1.2.3.4:6443
token=TG9yZW0gaXBzdW0gZG9sb3Igc2l0IGFtZXQsIGNvbnNlY3RldHVyIGFkaXBpc2NpbmcgZWxpdC4gQ3JhcyBhdCB1bHRyaWNpZXMgZWxpdC4gVXQgc2l0IGFtZXQgdm9sdXRwYXQgbnVuYy4K
cacert=/etc/kubernetes/ca.crt

[logging]
loglevel=5
logfile=/var/log/ovnkube.log

[cni]
conf-dir=/etc/cni/net.d
plugin=ovn-k8s-cni-overlay

[ovnnorth]
run-dir=/var/run/ovn/
db-location=/etc/ovn/ovnnb_db.db

[ovnsouth]
run-dir=/var/run/ovn/
db-location=/etc/ovn/ovnsb_db.db

[egressip-healthcheck-tls]
client-privkey=/path/to/client.key
client-cert=/path/to/client.crt
client-cacert=/path/to/client-ca.crt
cert-common-name=egress-health-check.example.com
```

For compatibility, the Egress IP health-check TLS options are also accepted in
the `[ovnnorth]` section. The dedicated section takes precedence when both are
set.

## Example

#### Initialize the cluster manager and ovnkube controller

```
ovnkube --init-cluster-manager <host-name> \
	--k8s-cacert <path to the cacert file> \
	--k8s-token <token string for authentication with kube apiserver> \
	--k8s-apiserver <url to the kube apiserver e.g. https://10.11.12.13.8443> \
	--cluster-subnets <cidr representing the global pod network e.g. 192.168.0.0/16>
```

The aforementioned ovnkube cluster manager will establish the watcher loops for the following:
 - nodes: as new nodes are born and init-node is called, the subnet IPAM is allocated for the respective nodes

```
ovnkube --init-ovnkube-controller <kubernetes-node-name> \
	--k8s-cacert <path to the cacert file> \
	--k8s-token <token string for authentication with kube apiserver> \
	--k8s-apiserver <url to the kube apiserver e.g. https://10.11.12.13.8443> \
	--cluster-subnets <cidr representing the global pod network e.g. 192.168.0.0/16>
```

The aforementioned ovnkube controller will initialize OVN logical topology and establish the watcher loops for the following:
 - nodes: as new nodes are born and init-node is called, the logical switches will be created automatically by giving out IPAM for the respective nodes
 - pods: as new pods are born, allocate the logical port with dynamic addressing from the switch it belongs to
 - services/endpoints: as new endpoints of services are born, create/update the logical load balancer on all logical switches
 - network policies and a few other k8s resources

#### Initialize a newly added node for the OVN network

Remember to install the cni binary first. Use the Makefile, or, manually copy 'ovn-k8s-cni-overlay' to /opt/cni/bin (or the appropriate directory that kubernetes will use to look for the plugin).

```
make install
```

Then, run the ovnkube executable to initialize the node:

```
ovnkube --init-node <name of the node as identified in kubernetes> \
	--k8s-cacert <path to the cacert file> \
	--k8s-token <token string for authentication with kube apiserver> \
	--k8s-apiserver <url to the kube apiserver e.g. https://10.11.12.13.8443>
```

With the above command, the node will get initialized for all OVN communication and a logical switch will be created for it.
