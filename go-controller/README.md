# ovn kubernetes go-controller

The golang based ovn controller is a reliable way to deploy the OVN SDN using kubernetes clients and watchers based on golang. 

### Build

Ensure go version >= 1.8

```
cd go-controller
make clean
make
```

Then find the executable here : go-controller/_output/go/bin/ovnkube

'ovnkube' is the single all-in-one executable useful for deploying the ovn cni plugin for a kubernetes deployment

### Usage

Run the 'ovnkube' executable to initialize master, node(s) and as the central all-in-one controller that builds the network as pods/services/ingress objects are born in kubernetes.

```
Usage:
  -alsologtostderr
    	log to standard error as well as files
  -apiserver string
    	url to the kubernetes apiserver (default "https://localhost:8443")
  -ca-cert string
    	CA cert for the api server
  -cluster-subnet string
    	Cluster wide IP subnet to use (default "11.11.0.0/16")
  -init-master string
    	initialize master, requires the hostname as argument
  -init-node string
    	initialize node, requires the name that node is registered with in kubernetes cluster
  -log_backtrace_at value
    	when logging hits line file:N, emit a stack trace
  -log_dir string
    	If non-empty, write log files in this directory
  -logtostderr
    	log to standard error instead of files
  -net-controller
    	Flag to start the central controller that watches pods/services/policies
  -stderrthreshold value
    	logs at or above this threshold go to stderr
  -token string
    	Bearer token to use for establishing ovn infrastructure
  -v value
    	log level for V logs
  -vmodule value
    	comma-separated list of pattern=N settings for file-filtered logging
```

## Example

#### Initialize the master and run the main controller

```
ovnkube --init-master <master-host-name> \
	--ca-cert <path to the cacert file> \
	--token <token string for authentication with kube apiserver> \
	--apiserver <url to the kube apiserver e.g. https://10.11.12.13.8443> \
	--cluster-subnet <cidr representing the global pod network e.g. 192.168.0.0/16> \
	--net-controller
```

With the above the master ovnkube controller will initialize the central master logical router and establish the watcher loops for the following:
 - nodes: as new nodes are born and init-node is called, the logical switches will be created automatically by giving out IPAM for the respective nodes
 - pods: as new pods are born, allocate the logical port with dynamic addressing from the switch it belongs to
 - services/endpoints: as new endpoints of services are born, create/update the logical load balancer on all logical switches


#### Initialize a newly added node for the OVN network

```
ovnkube --init-node <name of the node as identified in kubernetes> \
	--ca-cert <path to the cacert file> \
	--token <token string for authentication with kube apiserver> \
	--apiserver <url to the kube apiserver e.g. https://10.11.12.13.8443>
```

With the above command, the node will get initialized for all OVN communication and a logical switch will be created for it.
