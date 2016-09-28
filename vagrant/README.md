Kubernetes and OVN
==================

This contains a Vagrant setup for Kubernetes and OVN integration.

Howto
-----

Pull down Kubernetes (it's big)

* mkdir k8s
* cd k8s
* wget https://github.com/kubernetes/kubernetes/releases/download/v1.3.7/kubernetes.tar.gz
* tar xvzf kubernetes.tar.gz
* mkdir server
* cd server
* tar xvzf ../kubernetes/server/kubernetes-server-linux-amd64.tar.gz

Bringup the Vagrant setup

* vagrant up

Create a pod
------------

Create apache.yaml as follows:

```
apiVersion: v1
kind: Pod
metadata:
  name: apache
  labels:
    name: apache
spec:
  containers:
  - name: apache
    image: fedora/apache
```

Run that as follows:

* kubectl create -f ~/apache.yaml

Look at this deployment:

* kubectl get pods,svc
* kubectl describe pod apache

Launch a busybox pod:

* kubectl run -i --tty busybox --image=busybox -- sh

Verify this pod:

* kubectl get pods
* kubectl describe pod <busybox pod name>

You can now login to the busybox pod on the minion host and ping across pods.

[1]: https://hub.docker.com/r/google/nodejs-hello/
[2]: http://kubernetes.io/docs/hellonode/

References
----------

https://github.com/openvswitch/ovn-kubernetes
http://kubernetes.io/docs/hellonode/
http://kubernetes.io/docs/user-guide/kubectl-cheatsheet/
https://blog.jetstack.io/blog/k8s-getting-started-part2/
