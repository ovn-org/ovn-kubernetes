Kubernetes and OVN
==================

This contains a Vagrant setup for Kubernetes and OVN integration.  This needs
a minimum vagrant version of 1.8.5 and is known to atleast work on Mac,
Ubuntu 16.04 and Windows 10.

This has been tested with Virtualbox only.  Some work has been done to get it
working with vagrant-libvirt, but this may not fully work yet.

Howto
-----

From the cloned ovn-kubernetes repo,
* cd vagrant

Pull down Kubernetes (it's big)

* mkdir k8s
* cd k8s
* wget https://github.com/kubernetes/kubernetes/releases/download/v1.7.5/kubernetes.tar.gz
* tar xvzf kubernetes.tar.gz
* ./kubernetes/cluster/get-kube-binaries.sh
* mkdir server
* cd server
* tar xvzf ../kubernetes/server/kubernetes-server-linux-amd64.tar.gz

Bringup the Vagrant setup

* vagrant up

Run some containers
-------------------

The Vagrant will create some sample yaml files for configuring a pod
running Apache, as well as yaml for creating an east-west service and
a north-south service. To try these out, follow these instructions.

* cd ~/k8s/server/kubernetes/server/bin
* ./kubectl create -f ~/apache-pod.yaml
* ./kubectl create -f ~/nginx-pod.yaml
* ./kubectl create -f ~/apache-e-w.yaml
* ./kubectl create -f ~/apache-n-s.yaml

You can verify the services are up and running now:

* ./kubectl get pods
* ./kubectl get svc

You can now get to the service from the host running Virtualbox by using
the Nodeport and the IP 10.10.0.11 (the public-ip for the master found in
the vagrant/provisioning/vm_config.conf.yml file).

* curl 10.10.0.11:[nodeport]

Since the vagrant initializes gateway node on the minions too, you should be
able to access the same service via the IP addresses of the minion nodes too.

* curl 10.10.0.12:[nodeport]
* curl 10.10.0.13:[nodeport]

You should see OVN doing load-balancing between the pods, which means you will
both the apache example page and the nginx example page.

Launch a busybox pod:

* kubectl run -i --tty busybox --image=busybox -- sh

Verify this pod:

* kubectl get pods
* kubectl describe pod <busybox pod name>

You can now login to the busybox pod on the minion host and ping across pods.
