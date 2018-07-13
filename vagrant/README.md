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
* wget https://github.com/kubernetes/kubernetes/releases/download/v1.8.8/kubernetes.tar.gz
* tar xvzf kubernetes.tar.gz
* ./kubernetes/cluster/get-kube-binaries.sh
* mkdir server
* cd server
* tar xvzf ../kubernetes/server/kubernetes-server-linux-amd64.tar.gz

Bringup the Vagrant setup

* vagrant up k8s-master
* vagrant up k8s-minion1
* vagrant up k8s-minion2

Please note that, by default, the pods created cannot reach the internet.
This is because, the network to which it is attached is a vagrant private
network.  You can reach the pods running inside the VMs from your host via
the nodeport though.

If you need external network connectivity for your pods, you should use
the vagrant's "public network" option. This will give dhcp provided IP
address for your gateways. You can invoke this by providing your host's
network interface while running the vagrant. For e.g., if your host network
interface on your MAC is "en4: Thunderbolt Ethernet", you can run

* OVN_EXTERNAL="en4: Thunderbolt Ethernet" vagrant up k8s-minion1

Run some containers
-------------------

The Vagrant will create some sample yaml files for configuring a pod
running Apache, as well as yaml for creating an east-west service and
a north-south service. To try these out, follow these instructions.

* kubectl create -f ~/apache-pod.yaml
* kubectl create -f ~/nginx-pod.yaml
* kubectl create -f ~/apache-e-w.yaml
* kubectl create -f ~/apache-n-s.yaml

You can verify the services are up and running now:

* kubectl get pods
* kubectl get svc

You can now get to the service from the host running Virtualbox by using
the Nodeport and the IP 10.10.0.12 (the public-ip for the k8s-minion1 found in
the vagrant/provisioning/vm_config.conf.yml file).

* curl 10.10.0.12:[nodeport]

Since the vagrant initializes gateway node on the other minion too, you should
be able to access the same service via 10.10.0.13 too.

* curl 10.10.0.13:[nodeport]

Note: The above IP addresss are NOT used when you use the vagrant's public
network option. In that case, the above IP addresses are provided by dhcp
by your underlying network. So it is dynamic. You can fetch these IP
addresses by running 'ifconfig brenp0s8' on each of your host.  You can then
run the curl commands on those IP addresses.

You should see OVN doing load-balancing between the pods, which means you will
both the apache example page and the nginx example page.

Launch a busybox pod:

* kubectl run -i --tty busybox --image=busybox -- sh

Verify this pod:

* kubectl get pods
* kubectl describe pod <busybox pod name>

You can now login to the busybox pod on the minion host and ping across pods.
