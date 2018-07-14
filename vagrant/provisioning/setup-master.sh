#!/bin/bash

# Save trace setting
XTRACE=$(set +o | grep xtrace)
set -o xtrace

MASTER1=$1
MASTER2=$2
MASTER3=$3
NODE_NAME=$4
PUBLIC_SUBNET_MASK=$5
GW_IP=$6
OVN_EXTERNAL=$7

if [ -n "$OVN_EXTERNAL" ]; then
    MASTER1=`ifconfig enp0s8 | grep 'inet addr' | cut -d: -f2 | awk '{print $1}'`
    PUBLIC_SUBNET_MASK=`ifconfig enp0s8 | grep 'inet addr' | cut -d: -f4`
    GW_IP=`grep 'option routers' /var/lib/dhcp/dhclient.enp0s8.leases | head -1 | sed -e 's/;//' | awk '{print $3}'`
fi

OVERLAY_IP=$MASTER1

cat > setup_master_args.sh <<EOL
OVERLAY_IP=$OVERLAY_IP
MASTER1=$MASTER1
MASTER2=$MASTER2
MASTER3=$MASTER3
PUBLIC_SUBNET_MASK=$PUBLIC_SUBNET_MASK
GW_IP=$GW_IP
NODE_NAME=$NODE_NAME
OVN_EXTERNAL=$OVN_EXTERNAL
EOL

# Comment out the next line, if you prefer TCP instead of SSL.
SSL="true"

# Set HA to "true" if you want OVN HA
HA="false"

# FIXME(mestery): Remove once Vagrant boxes allow apt-get to work again
sudo rm -rf /var/lib/apt/lists/*

# Add external repos to install docker and OVS from packages.
sudo apt-get update
sudo apt-get install -y apt-transport-https ca-certificates
echo "deb http://18.191.116.101/openvswitch/stable /" |  sudo tee /etc/apt/sources.list.d/openvswitch.list
wget -O - http://18.191.116.101/openvswitch/keyFile |  sudo apt-key add -
sudo apt-key adv --keyserver hkp://keyserver.ubuntu.com:80 --recv-keys 58118E89F3A912897C070ADBF76221572C52609D
sudo su -c "echo \"deb https://apt.dockerproject.org/repo ubuntu-xenial main\" >> /etc/apt/sources.list.d/docker.list"
sudo apt-get update

# First, install docker
sudo apt-get purge lxc-docker
sudo apt-get install -y linux-image-extra-$(uname -r) linux-image-extra-virtual
sudo apt-get install -y docker-engine
sudo service docker start

# Install OVS and dependencies
sudo apt-get build-dep dkms
sudo apt-get install python-six openssl python-pip -y
sudo -H pip install --upgrade pip

sudo apt-get install openvswitch-datapath-dkms=2.9.2-1 -y
sudo apt-get install openvswitch-switch=2.9.2-1 openvswitch-common=2.9.2-1 libopenvswitch=2.9.2-1 -y
sudo -H pip install ovs

sudo apt-get install ovn-central=2.9.2-1 ovn-common=2.9.2-1 ovn-host=2.9.2-1 -y

if [ -n "$SSL" ]; then
    PROTOCOL=ssl
    echo "PROTOCOL=ssl" >> setup_master_args.sh
    # Install SSL certificates
    pushd /etc/openvswitch
    sudo ovs-pki -d /vagrant/pki init --force
    sudo ovs-pki req ovnsb && sudo ovs-pki self-sign ovnsb

    sudo ovs-pki req ovnnb && sudo ovs-pki self-sign ovnnb

    sudo ovs-pki req ovncontroller
    sudo ovs-pki -b -d /vagrant/pki sign ovncontroller switch
    popd
else
    PROTOCOL=tcp
    echo "PROTOCOL=tcp" >> setup_master_args.sh
fi

if [ "$HA" = "true" ]; then
    sudo /usr/share/openvswitch/scripts/ovn-ctl stop_nb_ovsdb
    sudo /usr/share/openvswitch/scripts/ovn-ctl stop_sb_ovsdb
    sudo rm /etc/openvswitch/ovn*.db
    sudo /usr/share/openvswitch/scripts/ovn-ctl stop_northd

    LOCAL_IP=$OVERLAY_IP

    sudo /usr/share/openvswitch/scripts/ovn-ctl \
        --db-nb-cluster-local-addr=$LOCAL_IP start_nb_ovsdb

    sudo /usr/share/openvswitch/scripts/ovn-ctl \
        --db-sb-cluster-local-addr=$LOCAL_IP start_sb_ovsdb
    
    ovn_nb="$PROTOCOL:$MASTER1:6641,$PROTOCOL:$MASTER2:6641,$PROTOCOL:$MASTER3:6641"
    ovn_sb="$PROTOCOL:$MASTER1:6642,$PROTOCOL:$MASTER2:6642,$PROTOCOL:$MASTER3:6642"

    sudo ovn-northd -vconsole:emer -vsyslog:err -vfile:info \
    --ovnnb-db="$ovn_nb" --ovnsb-db="$ovn_sb" --no-chdir \
    --log-file=/var/log/openvswitch/ovn-northd.log \
    --pidfile=/var/run/openvswitch/ovn-northd.pid --detach --monitor
fi

# Install golang
wget -nv https://dl.google.com/go/go1.9.2.linux-amd64.tar.gz
sudo tar -C /usr/local -xzf go1.9.2.linux-amd64.tar.gz
export PATH="/usr/local/go/bin:echo $PATH"
export GOPATH=$HOME/work

# Setup CNI directory
sudo mkdir -p /opt/cni/bin/

# Install OVN+K8S Integration
mkdir -p $HOME/work/src/github.com/openvswitch
pushd $HOME/work/src/github.com/openvswitch
git clone https://github.com/openvswitch/ovn-kubernetes
popd
pushd $HOME/work/src/github.com/openvswitch/ovn-kubernetes/go-controller
make 1>&2 2>/dev/null
sudo make install
popd

# Install CNI
pushd ~/
wget -nv https://github.com/containernetworking/cni/releases/download/v0.5.2/cni-amd64-v0.5.2.tgz
popd
sudo mkdir -p /opt/cni/bin
pushd /opt/cni/bin
sudo tar xvzf ~/cni-amd64-v0.5.2.tgz
popd

# Install k8s

# Install an etcd cluster
sudo docker run --net=host -v /var/etcd/data:/var/etcd/data -d \
        gcr.io/google_containers/etcd:3.0.17 /usr/local/bin/etcd \
        --listen-peer-urls http://127.0.0.1:2380 \
        --advertise-client-urls=http://127.0.0.1:4001 \
        --listen-client-urls=http://0.0.0.0:4001 \
        --data-dir=/var/etcd/data

# Start k8s daemons
sudo sh -c 'echo "PATH=$PATH:$HOME/k8s/server/kubernetes/server/bin" >> /etc/profile'
pushd k8s/server/kubernetes/server/bin
echo "Starting kube-apiserver ..."
nohup sudo ./kube-apiserver --service-cluster-ip-range=172.16.1.0/24 \
                            --address=0.0.0.0 \
                            --etcd-servers=http://127.0.0.1:4001 \
                            --advertise-address=$MASTER1 \
                            --v=2 2>&1 0<&- &>/dev/null &

# Wait till kube-apiserver starts up
while true; do
    ./kubectl get nodes
    if [ $? -eq 0 ]; then
        break
    fi
    echo "waiting for kube-apiserver to start...."
    sleep 1
done

echo "Starting kube-controller-manager ..."
nohup sudo ./kube-controller-manager --master=127.0.0.1:8080 --v=2 2>&1 0<&- &>/dev/null &

echo "Starting kube-scheduler ..."
nohup sudo ./kube-scheduler --master=127.0.0.1:8080 --v=2 2>&1 0<&- &>/dev/null &

popd

# Create a kubeconfig file.
cat << KUBECONFIG >> ~/kubeconfig.yaml
apiVersion: v1
clusters:
- cluster:
    server: http://localhost:8080
  name: default-cluster
- cluster:
    server: http://localhost:8080
  name: local-server
- cluster:
    server: http://localhost:8080
  name: ubuntu
contexts:
- context:
    cluster: ubuntu
    user: ubuntu
  name: ubuntu
current-context: ubuntu
kind: Config
preferences: {}
users:
- name: ubuntu
  user:
    password: p1NVMZqhOOOqkWQq
    username: admin
KUBECONFIG

pushd k8s/server/kubernetes/server/bin
nohup sudo ./kubelet --kubeconfig $HOME/kubeconfig.yaml \
                     --v=2 --address=0.0.0.0 \
                     --fail-swap-on=false \
                     --runtime-cgroups=/systemd/system.slice \
                     --kubelet-cgroups=/systemd/system.slice \
                     --enable-server=true --network-plugin=cni \
                     --cni-conf-dir=/etc/cni/net.d \
                     --cni-bin-dir="/opt/cni/bin/" >/tmp/kubelet.log 2>&1 0<&- &
popd

if [ $PROTOCOL = "ssl" ]; then
 SSL_ARGS="-nb-server-privkey /etc/openvswitch/ovnnb-privkey.pem \
 -nb-server-cert /etc/openvswitch/ovnnb-cert.pem \
 -nb-server-cacert /vagrant/pki/switchca/cacert.pem \
 -sb-server-privkey /etc/openvswitch/ovnsb-privkey.pem \
 -sb-server-cert /etc/openvswitch/ovnsb-cert.pem \
 -sb-server-cacert /vagrant/pki/switchca/cacert.pem  \
 -nb-client-privkey /etc/openvswitch/ovncontroller-privkey.pem \
 -nb-client-cert /etc/openvswitch/ovncontroller-cert.pem \
 -nb-client-cacert /etc/openvswitch/ovnnb-ca.cert \
 -sb-client-privkey /etc/openvswitch/ovncontroller-privkey.pem \
 -sb-client-cert /etc/openvswitch/ovncontroller-cert.pem \
 -sb-client-cacert /etc/openvswitch/ovnsb-ca.cert"
fi

if [ "$HA" = "true" ]; then
    ovn_nb="$PROTOCOL://$MASTER1:6641,$PROTOCOL://$MASTER2:6641,$PROTOCOL://$MASTER3:6641"
    ovn_sb="$PROTOCOL://$MASTER1:6642,$PROTOCOL://$MASTER2:6642,$PROTOCOL://$MASTER3:6642"
else
    ovn_nb="$PROTOCOL://$OVERLAY_IP:6641" 
    ovn_sb="$PROTOCOL://$OVERLAY_IP:6642"
fi

nohup sudo ovnkube -k8s-kubeconfig $HOME/kubeconfig.yaml -net-controller -loglevel=4 \
 -k8s-apiserver="http://$OVERLAY_IP:8080" \
 -logfile="/var/log/openvswitch/ovnkube.log" \
 -init-master="k8smaster" -cluster-subnet="192.168.0.0/16" \
 -init-node="k8smaster" \
 -service-cluster-ip-range=172.16.1.0/24 \
 -nodeport \
 -k8s-token="test" \
 -nb-address="$ovn_nb" \
 -sb-address="$ovn_sb" \
 -init-gateways -gateway-localnet \
 ${SSL_ARGS} 2>&1 &

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

sleep 10

# Restore xtrace
$XTRACE
