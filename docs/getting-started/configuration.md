## Disable Forwarding Config

OVN-Kubernetes allows to enable or disable IP forwarding for all traffic on OVN-Kubernetes managed interfaces (such as br-ex). By default forwarding is enabled and this allows host to forward traffic across OVN-Kubernetes managed interfaces. If forwarding is disabled then Kubernetes related traffic is still forwarded appropriately, but other IP traffic will not be routed by cluster nodes.

IP forwarding is implemented at cluster node level by modifying both iptables `FORWARD` chain and IP forwarding `sysctl` parameters. 

- If forwarding is enabled(default) then system administrators need to set following sysctl parameters. An operator can be built to manage forwarding sysctl parameters based on forwarding mode. No extra iptables rules are added by OVN-Kubernetes to FORWARD chain while using this IP forwarding mode.

```
net.ipv4.ip_forward=1
net.ipv6.conf.all.forwarding=1
```

- IP forwarding can be disabled either by setting `disable-forwarding` command line option to `true` while starting ovnkube or by setting `disable-forwarding` to `true` in config file. If forwarding is disabled then system administrators need to set following sysctl parameters to stop routing other IP traffic. An operator can be built to manage forwarding sysctl parameters based on forwarding mode.

```
net.ipv4.ip_forward=0
net.ipv6.conf.all.forwarding=0
```

When IP forwarding is disabled, following sysctl parameters are modified by OVN-Kubernetes to allow forwarding Kubernetes related traffic on OVN-Kubernetes managed bridge interfaces and management port interface.

```
net.ipv4.conf.br-ex.forwarding=1
net.ipv4.conf.ovn-k8s-mp0.forwarding = 1
```

Additionally following iptables rules are added at FORWARD chain to forward clusterNetwork and serviceNetwork traffic to their intended destinations. 

```
-A FORWARD -s 10.128.0.0/14 -j ACCEPT
-A FORWARD -d 10.128.0.0/14 -j ACCEPT
-A FORWARD -s 169.254.169.1 -j ACCEPT
-A FORWARD -d 169.254.169.1 -j ACCEPT
-A FORWARD -d 172.16.1.0/24 -j ACCEPT
-A FORWARD -s 172.16.1.0/24 -j ACCEPT
-A FORWARD -i breth1 -j DROP
-A FORWARD -o breth1 -j DROP
```