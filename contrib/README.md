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
- Windows Server 2016 build version 1709

Note: minimum required ansible version is 2.4.2.0

## Work in progress

- Support for hybrid cluster with master/minion nodes on different cloud providers.

- Windows Server 2016 build number 14393

- Windows Server 2016 build version 1803

- Different Linux versions support (currently only Ubuntu 14.04 and 16.04 supported)

### Known issues

- Windows Server 2016 build version 1709 requires the Hyper-V feature for the moment. This will be fixed with the next release of Open vSwitch on Windows

- The interface on Windows should be able to receive the IP via DHCP

- Warning: the firewall on Windows gets disabled for this test

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
