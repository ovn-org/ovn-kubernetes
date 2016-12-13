#!/bin/bash

# Save trace setting
XTRACE=$(set +o | grep xtrace)
set -o xtrace

# args:
# $1: The gateway IP to use
# $2: The gateway subnet mask
# $3: The default GW for the GW device

PUBLIC_IP=$1
PUBLIC_SUBNET_MASK=$2
GW_IP=$3

# First, install docker
sudo apt-get update
sudo apt-get install -y apt-transport-https ca-certificates
sudo apt-key adv --keyserver hkp://p80.pool.sks-keyservers.net:80 --recv-keys 58118E89F3A912897C070ADBF76221572C52609D
sudo su -c "echo \"deb https://apt.dockerproject.org/repo ubuntu-xenial main\" >> /etc/apt/sources.list.d/docker.list"
sudo apt-get update
sudo apt-get purge lxc-docker
sudo apt-get install -y linux-image-extra-$(uname -r) linux-image-extra-virtual
sudo apt-get install -y docker-engine
sudo service docker start

# Install k8s

# Install an etcd cluster
sudo docker run --net=host -d gcr.io/google_containers/etcd:2.0.12 /usr/local/bin/etcd \
                --addr=127.0.0.1:4001 --bind-addr=0.0.0.0:4001 --data-dir=/var/etcd/data

# Download Kubernetes ... yes, it's huge
#mkdir k8s
#pushd k8s
#wget https://github.com/kubernetes/kubernetes/releases/download/v1.3.7/kubernetes.tar.gz
#tar xvzf kubernetes.tar.gz

# Now untar kubernetes-server-linux-amd64.tar.gz
#mkdir server
#cd server
#tar xvzf ../kubernetes/server/linux/kubernetes-server-linux-amd64.tar.gz
#popd

# Start k8s daemons
pushd k8s/server/kubernetes/server/bin
echo "Starting kube-apiserver ..."
nohup sudo ./kube-apiserver --service-cluster-ip-range=192.168.200.0/24 \
                            --address=0.0.0.0 --etcd-servers=http://127.0.0.1:4001 \
                            --v=2 2>&1 0<&- &>/dev/null &
sleep 5

echo "Starting kube-controller-manager ..."
nohup sudo ./kube-controller-manager --master=127.0.0.1:8080 --v=2 2>&1 0<&- &>/dev/null &
sleep 5

echo "Starting kube-scheduler ..."
nohup sudo ./kube-scheduler --master=127.0.0.1:8080 --v=2 2>&1 0<&- &>/dev/null &
sleep 5

echo "Starting ovn-k8s-watcher ..."
sudo ovn-k8s-watcher --overlay --pidfile --log-file -vfile:info -vconsole:emer --detach

# Create a OVS physical bridge and move IP address of enp0s9 to br-enp0s9
echo "Creating physical bridge ..."
sudo ovs-vsctl add-br br-enp0s9
sudo ovs-vsctl add-port br-enp0s9 enp0s9
sudo ip addr flush dev enp0s9
sudo ifconfig br-enp0s9 $PUBLIC_IP netmask $PUBLIC_SUBNET_MASK up

# Setup the GW node on the master
sudo ovn-k8s-overlay gateway-init --cluster-ip-subnet="192.168.0.0/16" --bridge-interface br-enp0s9 \
                                  --physical-ip $PUBLIC_IP/$PUBLIC_SUBNET_MASK \
                                  --node-name="kube-gateway-node1" --default-gw $GW_IP

# Start the gateway helper.
sudo ovn-k8s-gateway-helper --physical-bridge=br-enp0s9 --physical-interface=enp0s9 --pidfile --detach

sleep 5
popd

# Setup some example yaml files
cat << APACHEPOD >> ~/apache-pod.yaml
apiVersion: v1
kind: Pod
metadata:
  name: apachetwin
  labels:
    name: webserver
spec:
  containers:
  - name: apachetwin
    image: fedora/apache
APACHEPOD

cat << NGINXPOD >> ~/nginx-pod.yaml
apiVersion: v1
kind: Pod
metadata:
  name: nginxtwin
  labels:
    name: webserver
spec:
  containers:
  - name: nginxtwin
    image: nginx
NGINXPOD

cat << APACHEEW >> ~/apache-e-w.yaml
apiVersion: v1
kind: Service
metadata:
  labels:
    name: apacheservice
    role: service
  name: apacheservice
spec:
  ports:
    - port: 8800
      targetPort: 80
      protocol: TCP
      name: tcp
  selector:
    name: webserver
APACHEEW

cat << APACHENS >> ~/apache-n-s.yaml
apiVersion: v1
kind: Service
metadata:
  labels:
    name: apacheexternal
    role: service
  name: apacheexternal
spec:
  ports:
    - port: 8800
      targetPort: 80
      protocol: TCP
      name: tcp
  selector:
    name: webserver
  type: NodePort
APACHENS

# Restore xtrace
$XTRACE
