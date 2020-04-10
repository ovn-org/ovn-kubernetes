# Installing OVS and OVN on Ubuntu

## Installing OVS and OVN from packages

For testing and POCs, we maintain OVS and OVN packages in a AWS VM.
(For production, it is recommended that you build your own OVS packages.)
To install packages from there, you can run:

```
sudo apt-get install apt-transport-https
echo "deb http://3.19.28.122/openvswitch/stable /" |  sudo tee /etc/apt/sources.list.d/openvswitch.list
wget -O - http://3.19.28.122/openvswitch/keyFile |  sudo apt-key add -
sudo apt-get update
```

To install OVS bits on all nodes, run:

```
sudo apt-get build-dep dkms
sudo apt-get install python-six openssl -y

sudo apt-get install openvswitch-datapath-dkms -y
sudo apt-get install openvswitch-switch openvswitch-common -y
```

On the master, where you intend to start OVN's central components,
run:

```
sudo apt-get install ovn-central ovn-common ovn-host -y
```

On the agent nodes, run:

```
sudo apt-get install ovn-host ovn-common -y
```

## Installing OVS and OVN from sources

Install a few pre-requisite packages.

```
apt-get update
apt-get install -y build-essential fakeroot debhelper \
                    autoconf automake libssl-dev \
                    openssl python-all \
                    python-setuptools \
                    python-six \
                    libtool git dh-autoreconf \
                    linux-headers-$(uname -r)
```

Clone the OVS repo.

```
git clone https://github.com/openvswitch/ovs.git
cd ovs
```

Configure and compile the sources

```
./boot.sh
./configure --prefix=/usr --localstatedir=/var  --sysconfdir=/etc --enable-ssl --with-linux=/lib/modules/`uname -r`/build
make -j3
```

Install the executables

```
make install
make modules_install
```

Create a depmod.d file to use OVS kernel modules from this repo instead of
upstream linux.

```
cat > /etc/depmod.d/openvswitch.conf << EOF
override openvswitch * extra
override vport-geneve * extra
override vport-stt * extra
override vport-* * extra
EOF
```

Copy a startup script and start OVS

```
depmod -a
cp debian/openvswitch-switch.init /etc/init.d/openvswitch-switch
/etc/init.d/openvswitch-switch force-reload-kmod
```
