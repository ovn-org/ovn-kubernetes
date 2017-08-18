# Installing OVS and OVN on Ubuntu

## Installing OVS and OVN from packages

Some students in a university in New Zealand maintain OVS and OVN packages at
http://packages.wand.net.nz/

To install packages from there, you can run:

```
sudo apt-get install apt-transport-https
echo "deb https://packages.wand.net.nz $(lsb_release -sc) main" | sudo tee /etc/apt/sources.list.d/wand.list
sudo curl https://packages.wand.net.nz/keyring.gpg -o /etc/apt/trusted.gpg.d/wand.gpg
sudo apt-get update
```

To install OVS bits on all nodes, run:

```
sudo apt-get build-dep dkms
sudo apt-get install python-six openssl python-pip -y
sudo -H pip install --upgrade pip

sudo apt-get install openvswitch-datapath-dkms -y
sudo apt-get install openvswitch-switch openvswitch-common -y
sudo -H pip install ovs
```

On the master node, where you intend to start OVN's central components,
run:

```
sudo apt-get install ovn-central ovn-common -y
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
easy_install -U pip
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

Install OVS python libraries

```
pip install ovs
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
