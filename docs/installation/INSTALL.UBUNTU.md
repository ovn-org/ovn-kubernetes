# Installing OVS and OVN on Ubuntu

## Installing OVS and OVN from packages

To install OVS bits on all nodes, run:

```
sudo apt-get install python3-six openssl -y

sudo apt-get install openvswitch-switch openvswitch-common -y
```

On the node where you intend to start OVN database and northd components, run:

```
sudo apt-get install ovn-central ovn-common ovn-host -y
```

On the agent nodes, run:

```
sudo apt-get install ovn-host ovn-common -y
```
