# Ansible playbooks to deploy ovn-kubernetes

The ansible playbooks are able to deploy a kubernetes cluster with
Linux and Windows minion nodes.

## Ansible requirements

Minimum required ansible version is `2.4.2.0`. The recommended version is `2.7.2`.

For Linux: Make sure that you are able to SSH into the target nodes without being
asked for the password. You can read more [here](http://docs.ansible.com/ansible/latest/user_guide/intro_getting_started.html).

For Windows: Follow [this guide](https://docs.ansible.com/ansible/devel/user_guide/windows_setup.html)
to setup the node to be used with ansible.

#### Verifying the setup

To verify the setup and that ansible has been successfully configured you can run the following:

```
ansible -m setup all
```

This will connect to the target hosts and will gather host facts.
If the command succeeds and everything is green, you're good to go with running the playbook.

## How to use

Make sure to update first the [inventory](/contrib/inventory) with
details about the nodes.

To start the playbook, please run the following:
```
ansible-playbook ovn-kubernetes-cluster.yml
```

Currently supported Linux nodes:
- Ubuntu 16.04 and 18.04

Currently supported Windows nodes:
- Windows Server 2016 build version 1709 (OS Version 10.0.16299.0)
- Windows Server 2016 build version 1803 (OS Version 10.0.17134.0)
- Windows Server 2019 LTSC and build version 1809 (OS Version 10.0.17763.0)

## Ports that have to be opened on public clouds when using the playbooks

The following ports need to be opened if we access the cluster machines via the public address.

#### Kubernetes ports

- Kubernetes service ports (deployment specific): UDP and TCP `30000 - 32767`
- Kubelet (default port): TCP `10250`
- Kubernetes API: TCP `8080` for HTTP and TCP `443` for HTTPS

#### Ansible related ports

- WinRM via HTTPS: TCP `5986` (for HTTP also TCP `5985`)
- SSH: TCP `22`

### OVN related ports

- OVN Northbound (NB): TCP `6641`
- OVN Southbound (SB): TCP `6642`

### OVS related encapsulation ports

- GENEVE encapsulation (used by default): UDP `6081`
- STT encapsulation (optional encapsulation type, [no special NIC required](https://networkheresy.com/2012/03/04/network-virtualization-encapsulation-and-stateless-tcp-transport-stt/)): TCP `7471`

### Further useful ports/types

- Windows RDP Port: 3389 (TCP)
- ICMP: useful for debugging

## Work in progress

- Support for hybrid cluster with master/minion nodes on different cloud providers.

- Different Linux versions support (currently only Ubuntu 16.04 and 18.04 supported)

### Known issues

- Windows containers do not support IPv6 at the moment. You can read more [here](https://docs.microsoft.com/en-us/virtualization/windowscontainers/container-networking/architecture#unsupported-features-and-network-options)
