Installing OVS and OVN from sources on Ubuntu
=============================================

Install a few pre-requisite packages.

```
apt-get update
apt-get install -y build-essential fakeroot debhelper \
                    autoconf automake bzip2 libssl-dev \
                    openssl graphviz python-all procps \
                    python-dev python-setuptools \
                    python-twisted-conch libtool git dh-autoreconf \
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
override vport-* * extra
EOF
```

Copy a startup script and start OVS

```
depmod -a
cp debian/openvswitch-switch.init /etc/init.d/openvswitch-switch
/etc/init.d/openvswitch-switch force-reload-kmod
```
