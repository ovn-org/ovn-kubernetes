
# ovn-kubernetes Image

ovn-kubernetes has a daemonset that runs on each node and a deployment
with replicas=1 that runs on a master. The daemonset and deployment
run a collection of containers. Each container runs the same image.

Each container's entry point is a shell function in the ovnkube.sh script.

```
containers:
- name: run-ovn-northd
  image: docker.io/ovnkube/ovn-daemonset:latest
  command: ["/root/ovnkube.sh", "run-ovn-northd"]
```

The interface includes the command names (e.g., "run-ovn-northd"),
a set of environment variables, configMaps, and a set of files that mounted
into the container. Over time the interface evolves and the daemonsets need
to be updated as well. To assist in this the OVN_DAEMONSET_VERSION environment
variable is used by the daemonset to specify the version of the interface it
is using. The ovnkube.sh script notes the version and operates accordingly.


## Image Component Parts

Images created from this repo are for development, debug and local testing. In a product
context the product's build environment will build and push the image to a
suitable image repository.

The image includes:
* The ovnkube.sh script
* The ovnkube binary
* openvswitch (ovs) and open virtual network (ovn)
* a base for the image and dependency packages.

Some notes on the components:
* The ovnkube.sh script provides the interface between the daemonsets and the image.
  Because the interface evolves over time it is versioned. The script supports two
  consecutive versions of the interface and older versions are deprecated. This
  permits evolving the interface while not breaking existing users.

* The ovnkube binary is built in ../../go_controller and copied to this directory.

* ovs/ovn evolves over time and a suitable version needs to be included in the image.
  As of this writing ovs/ovn 2.11, or higher, is required. It is up to the image
  builder to include a suitable version.

* The base image is from any of many distributions. It is up to the image builder to
  select a suitable base. Depending on the base image, additional packages may be
  needed to satisfy dependencies. There are sample Dockerfiles included here
  that can be used as examples and to build development images. And a Makefile
  to assist in the image build.

# Interface Versions

Every container needs to set an environment variable that specifies the
interface version that it is using.

```
env:
- name: OVN_DAEMONSET_VERSION
  value: "4"
```
At present version "4" and "3" are supported.

# Version 4 interface

## ovn-config configMap

The ovn-config configMap is mounted into the container in directory
```
/var/run/ovn-config
```
The following files contain the config information:
```
k8s_apiserver
net_cidr
svc_cidr
```
In the yaml files each container has the volumeMounts: and the pod volumes: has configMap:
```
containers:
- name:
  volumeMounts:
  - mountPath: /var/run/ovn-config
    name: ovn-config-mount
volumes:
- configMap:
    name: ovn-config
  name: ovn-config-mount
```
The ovn-config configMap is as follows:
```
apiVersion: v1
data:
  k8s_apiserver: https://api.pcameron-0829e.devcluster.openshift.com:6443
  net_cidr: "10.128.0.0/14/23"
  svc_cidr: ""172.30.0.0/16"
kind: ConfigMap
metadata:
  name: ovn-config
  namespace: ovn-kubernetes
```

## ovnkube-config configMap

The ovnkube-config configMap is mounted into the container in directory
```
/var/run/ovnkube-config
```
In the yaml files each container has the volumeMounts: and the pod volumes: has configMap:
```
containers:
- name:
  volumeMounts:
  - mountPath: /var/run/ovnkube-config
    name: ovnkube-config-mount
volumes:
- configMap:
    name: ovnkube-config
  name: ovnkube-config-mount
```
The following configmap contains the config file for ovnkube:

```
kind: ConfigMap
apiVersion: v1
metadata:
  name: ovnkube-config
  namespace: ovn-kubernetes
data:
  ovn_k8s.conf: |-
    [default]
    mtu="1400"
    cluster-subnets="10.128.0.0/14/23"

    [kubernetes]
    service-cidr="172.30.0.0/16"
    ovn-config-namespace="ovn-kubernetes"
    apiserver=https://api.pcameron-0829e.devcluster.openshift.com:6443

    [logging]
    loglevel="2"

    [gateway]
    mode=local
    nodeport=true
```

## Environment variables

OVN_LOG_NORTHD - control northd logging
￼
￼Default -vconsole:emer -vsyslog:err -vfile:info
￼
VN_LOG_NB - control north bound data base logging
￼
￼Default -vconsole:info
￼
VN_LOG_SB - control south bound data base logging
￼
￼Default -vconsole:info
￼
OVN_LOG_CONTROLLER - ovn controller log level
￼
￼Default -vconsole:info
￼


# Version 3 interface

## Environment variables

* OVN_NET_CIDR - network cidr
* OVN_SVC_CIDR - service cidr
* K8S_APISERVER - api server url

The above 3 environment variables are set from the ovn-config configMap.

```
        - name: OVN_NET_CIDR
          valueFrom:
            configMapKeyRef:
              name: ovn-config
              key: net_cidr
        - name: OVN_SVC_CIDR
          valueFrom:
            configMapKeyRef:
              name: ovn-config
              key: svc_cidr
        - name: K8S_APISERVER
          valueFrom:
            configMapKeyRef:
              name: ovn-config
              key: k8s_apiserver
```
OVN_LOG_NORTHD - control northd logging
￼
￼Default -vconsole:emer -vsyslog:err -vfile:info
￼
VN_LOG_NB - control north bound data base logging
￼
￼Default -vconsole:info
￼
VN_LOG_SB - control south bound data base logging
￼
￼Default -vconsole:info
￼
OVN_LOG_CONTROLLER - ovn controller log level
￼
￼Default -vconsole:info
￼
OVNKUBE_LOGLEVEL - loglevel for ovnkube binary

Default 4

K8S_TOKEN - kubernetes token - optional

Default is to use: /var/run/secrets/kubernetes.io/serviceaccount/token

K8S_CACERT - ca crt - optional

Default is to use: /var/run/secrets/kubernetes.io/serviceaccount/ca.crt

# Version 2, 1 interfaces
These versions are deprecated.

# Common Interface Components
OVNKUBE_LOGLEVEL - loglevel for ovnkube binary

Default 4

K8S_TOKEN - kubernetes token - optional

Default is to use: /var/run/secrets/kubernetes.io/serviceaccount/token

K8S_CACERT - ca crt - optional

Default is to use: /var/run/secrets/kubernetes.io/serviceaccount/ca.crt

# Version 2, 1 interfaces
These versions are deprecated.

# Common Interface Components

The following environment variables are used by the supported versions.

K8S_NODE - the name of the node.
```
env:
- name: K8S_NODE
  valueFrom:
    fieldRef:
      fieldPath: spec.nodeName
```

OVN_KUBERNETES_NAMESPACE - contains the namespace used by the ovs/ovn pods/containers
```
env:
- name: OVN_KUBERNETES_NAMESPACE
  valueFrom:
    fieldRef:
      fieldPath: metadata.namespace
```


