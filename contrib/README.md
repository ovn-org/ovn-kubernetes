# Ansible playbooks to deploy ovn-kubernetes

The ansible playbooks are able to deploy a kubernetes cluster with
Linux and Windows minion nodes.

## How to use

Make sure to update first the [inventory](/contrib/inventory) with
details about the nodes.

To start the playbook, please run the following:
```
ansible-playbook ovn-kubernetes-cluster.yml
```

Currently supported Linux nodes:
- Ubuntu 14.04 and 16.04

Currently supported Windows nodes:
- Windows Server 2016 build version 1709 (OS Version 10.0.16299.0)
- Windows Server 2016 build version 1803 (OS Version 10.0.17134.0)

Note: Minimum required ansible version is 2.4.2.0. The recommended version is 2.7.2.

## Ports to be opened in public clouds

The following ports need to be opened if we access the cluster machines via the public address.

#### OVN related ports

- GENEVE: UDP `6081` or alternatively with STT: TCP `7471`
- OVN Database Ports: TCP `6641 - 6642`

#### Kubernetes ports

- Kubernetes service ports: UDP and TCP `30000 - 32767`
- Kubelet: TCP `10250 - 10252`
- Kubernetes API: TCP `8080` for HTTP and TCP `443` for HTTPS

#### Ansible related ports

- WinRM via HTTPS: TCP `5986` (for HTTP also TCP `5985`)
- SSH: TCP `22`

## Work in progress

- Support for hybrid cluster with master/minion nodes on different cloud providers.

- Different Linux versions support (currently only Ubuntu 14.04 and 16.04 supported)

### Known issues

- Windows containers do not support IPv6 at the moment. You can read more [here](https://docs.microsoft.com/en-us/virtualization/windowscontainers/container-networking/architecture#unsupported-features-and-network-options)

## Ansible requirements

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
