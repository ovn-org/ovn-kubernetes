# Live Migration

## Introduction

The Live Migration feature allows [kubevirt](kubevirt.io) virtual machines to be [live migrated](https://kubevirt.io/user-guide/operations/live_migration/)
while keeping the established TCP connections alive, and preserving the VM IP configuration.
These two requirements provide seamless live-migration of a KubeVirt VM using OVN-Kubernetes
cluster default network.

## Requirements

- KubeVirt >= v1.0.0
- DHCP aware guest image (fedora family is well tested, https://quay.io/organization/containerdisks)

# Limitations

- Only KubeVirt VMs with bridge binding pod network are supported
- Single stack IPv6 is not supported
- DualSack does not configure routes for IPv6 over DHCP/autoconf
- SRIOV is not supported

## Example: live migrating a fedora guest image

Install KubeVirt following the guide [here](https://kubevirt.io/user-guide/operations/installation/)

Create a fedora virtual machine with the annotations `kubevirt.io/allow-pod-bridge-network-live-migration`:
```bash
cat <<'EOF' | kubectl create -f -
apiVersion: kubevirt.io/v1
kind: VirtualMachine
metadata:
  name: fedora
spec:
  runStrategy: Always
  template:
    metadata:
      annotations:
        # Allow KubeVirt VMs with bridge binding to be migratable
        # also ovn-k will not configure network at pod, delegate it to DHCP
        kubevirt.io/allow-pod-bridge-network-live-migration: ""
    spec:
      domain:
        devices:
          disks:
          - disk:
              bus: virtio
            name: containerdisk
          - disk:
              bus: virtio
            name: cloudinit
          rng: {}
        features:
          acpi: {}
          smm:
            enabled: true
        firmware:
          bootloader:
            efi:
              secureBoot: true
        resources:
          requests:
            memory: 1Gi
      terminationGracePeriodSeconds: 180
      volumes:
      - containerDisk:
          image: quay.io/containerdisks/fedora:38
        name: containerdisk
      - cloudInitNoCloud:
          networkData: |
            version: 2
            ethernets:
              eth0:
                dhcp4: true
          userData: |-
            #cloud-config
            # The default username is: fedora
            password: fedora
            chpasswd: { expire: False }
        name: cloudinit
EOF
```

After waiting for the VM to be ready - `kubectl wait vmi fedora --for=condition=Ready --timeout=5m` -
the VM status should be as shown below - i.e. `kubectl get vmi fedora`:

```bash
kubectl get vmi fedora
NAME     AGE     PHASE     IP            NODENAME      READY
fedora   9m42s   Running   10.244.2.26   ovn-worker3   True
```

Login and check that the VM has receive a proper address `virtctl console fedora`
```bash
[fedora@fedora ~]ip a
1: lo: <LOOPBACK,UP,LOWER_UP> mtu 65536 qdisc noqueue state UNKNOWN group default qlen 1000
    link/loopback 00:00:00:00:00:00 brd 00:00:00:00:00:00
    inet 127.0.0.1/8 scope host lo
       valid_lft forever preferred_lft forever
    inet6 ::1/128 scope host
       valid_lft forever preferred_lft forever
2: eth0: <BROADCAST,MULTICAST,UP,LOWER_UP> mtu 1400 qdisc fq_codel state UP group default qlen 1000
    link/ether 0a:58:0a:f4:02:1a brd ff:ff:ff:ff:ff:ff
    altname enp1s0
    inet 10.244.2.26/24 brd 10.244.2.255 scope global dynamic noprefixroute eth0
       valid_lft 3412sec preferred_lft 3412sec
    inet6 fe80::32d2:10d4:f5ed:3064/64 scope link noprefixroute
       valid_lft forever preferred_lft forever
```

Also we can check the neighbours cache to verify it later on
```bash
[fedora@fedora ~]arp -a
_gateway (169.254.1.1) at 0a:58:a9:fe:01:01 [ether] on eth0
```


Keep in mind the default gw is a link local address; that is because 
the live migration feature is implemented using ARP proxy.

The last route is needed since the link local address subnet is not bound to any interface, that
route is automatically created by dhcp client.

```bash
[fedora@fedora ~]ip route
default via 169.254.1.1 dev eth0 proto dhcp src 10.244.2.26 metric 100
10.244.2.0/24 dev eth0 proto kernel scope link src 10.244.2.26 metric 100
169.254.1.1 dev eth0 proto dhcp scope link src 10.244.2.26 metric 100
```

Then a live migration can be initialized with `virtctl migrate fedora` and wait
at the vmim resource
```bash
virtctl migrate fedora
VM fedora was scheduled to migrate
kubectl get vmim -A -o yaml
  status:
    migrationState:
      completed: true
```

After migration, the network configuration is the same - including the GW neighbor cache.
```bash
oc get vmi -A
NAMESPACE   NAME     AGE   PHASE     IP            NODENAME     READY
default     fedora   16m   Running   10.244.2.26   ovn-worker   True
[fedora@fedora ~]ip a
1: lo: <LOOPBACK,UP,LOWER_UP> mtu 65536 qdisc noqueue state UNKNOWN group default qlen 1000
    link/loopback 00:00:00:00:00:00 brd 00:00:00:00:00:00
    inet 127.0.0.1/8 scope host lo
       valid_lft forever preferred_lft forever
    inet6 ::1/128 scope host
       valid_lft forever preferred_lft forever
2: eth0: <BROADCAST,MULTICAST,UP,LOWER_UP> mtu 1400 qdisc fq_codel state UP group default qlen 1000
    link/ether 0a:58:0a:f4:02:1a brd ff:ff:ff:ff:ff:ff
    altname enp1s0
    inet 10.244.2.26/24 brd 10.244.2.255 scope global dynamic noprefixroute eth0
       valid_lft 2397sec preferred_lft 2397sec
    inet6 fe80::32d2:10d4:f5ed:3064/64 scope link noprefixroute
       valid_lft forever preferred_lft forever
[fedora@fedora ~]arp -a
_gateway (169.254.1.1) at 0a:58:a9:fe:01:01 [ether] on eth0
```

# Configuring dns server
By default the DHCP server at ovn-kuberntes will configure the kubernetes
default dns service `kube-system/kube-dns` as the name server. This can be
overriden with the following command line options:
- dns-service-namespace
- dns-service-name


# Configuring dual stack guest images
For dual stack, ovn-kubernetes is configuring the IPv6 address to guest VMs using
DHCPv6, but the IPv6 default gateway has to be configured manually.
Since this address is the same - `fe80::1` - the virtual machine configuration
is stable. 

Both the ipv4 and ipv6 configurations have to be activated.

NOTE: The IPv6 autoconf/SLAAC is not supported at ovn-k live-migration


For fedora cloud-init can be used to activate dual stack:
```yaml
 - cloudInitNoCloud:
     networkData: |
       version: 2
       ethernets:
         eth0:
           dhcp4: true
           dhcp6: true
     userData: |-
       #cloud-config
       # The default username is: fedora
       password: fedora
       chpasswd: { expire: False }
       runcmd:
         - nmcli c m "cloud-init eth0" ipv6.method dhcp
         - nmcli c m "cloud-init eth0" +ipv6.routes "fe80::1"
         - nmcli c m "cloud-init eth0" +ipv6.routes "::/0 fe80::1"
         - nmcli c reload "cloud-init eth0"
```

For fedora coreos this can be configured with using the following
ignition yaml:

```yaml
variant: fcos
version: 1.4.0
storage:
  files:
    - path: /etc/nmstate/001-dual-stack-dhcp.yml
      contents:
        inline: |
          interfaces:
          - name: enp1s0
            type: ethernet
            state: up
            ipv4:
              enabled: true
              dhcp: true
            ipv6:
              enabled: true
              dhcp: true
              autoconf: false
    - path: /etc/nmstate/002-dual-sack-ipv6-gw.yml
      contents:
        inline: |
          routes:
            config:
            - destination: ::/0
              next-hop-interface: enp1s0
              next-hop-address: fe80::1
```
