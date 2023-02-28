# Bridge bypass configuration

## Description

To utilize hardware offloading NIC may require both IN and OUT ports be a devices backed by this NIC.
In case of external traffic one port is OVS bridge, hence such traffic wont be hw offloaded.
Using switchdev VirtualFunction (VF) or SubFunction (SF) as a gateway interface allows to cope with it.


## How it works?

When gateway interface is switchdev VF or SF ovnkube will create bypass bridge configuration.
Simplified can be depicted as following:

```
                         +----------+
                         |  br-ext  |
                   +--------+       |
                   | UPLINK |       |
                   +--------+       |  patch  +----------+
                         |          x---------x  br-int  |
    +--------+     +--------+       |  port   +----------+
    | NETDEV +-----+   REP  |       |
    +--------+     +--------+       |
                         +----------+
```

Where `UPLINK` is a port of offloading capable NIC, `NETDEV` is a switchdev function of this port
and `REP` is a representor netdevice of the switchdev function. Node IP assigned to `NETDEV` which
make OVS to chose `REP` port for external flows instead of the bridge.


## How to use?

Bypass bridge configuration can be enabled by one of:

- Specify `NETDEV` as a gateway interface explicitly via `OVN_GATEWAY_OPTS` environment variable for
ovnkube-node container. Example:

```yaml
            - name: OVN_GATEWAY_OPTS
              value: "--gateway-interface=eth0f0v0"
```

- Manually creating bypass bridge configuration: create external bridge, add `UPLINK` and `REP` to
it, and assign node IP with default route to `NETDEV`. Ovnkube behaviour is to lookup for an
interface with a default route, and if it is VF/SF - it will assume it is bypass bridge configuration
and will act accordingly.

- Requesting device as resource from device plugin. Device plugin, for ex. `sriov-network-device-plugin`,
must be configured to create ovnkube dedicated resource pool. Add name of the resource passed via
`OVNKUBE_NODE_BYPASS_PORT_DP_RESOURCE_NAME` environment variable to ovnkube-node container and
request using `resources` field. Example:

```yaml
        ...

        env:
        - name: OVNKUBE_NODE_BYPASS_PORT_DP_RESOURCE_NAME
          value: "nvidia.com/ovnkube"
        - ...

        resources:
          requests:
            nvidia.com/ovnkube: 1
          limits:
            nvidia.com/ovnkube: 1

        ...
```

Bypass resource name is stackable with management port resource name:

```yaml
        ...

        env:
        - name: OVNKUBE_NODE_MGMT_PORT_DP_RESOURCE_NAME
          value: "nvidia.com/ovnkube"
        - name: OVNKUBE_NODE_BYPASS_PORT_DP_RESOURCE_NAME
          value: "nvidia.com/ovnkube"
        - ...

        resources:
          requests:
            nvidia.com/ovnkube: 2
          limits:
            nvidia.com/ovnkube: 2

        ...
```

## Switching between regular and bypass configuration

Ovnkube can transit between configurations. If node has regular bridge configuration and gateway
interface *explicitly* set to switchdev VF or SF, then ovnkube will do necessary changes to apply
bypass configuration. And, opposite, if node has bypass bridge configuration and gateway interface
*explicitly* set to the bridge or uplink, then ovnkube will switch to regular bridge configuration
efectivelly removing VF/SF representor from the bridge and adding IP configuration to the bridge
netdevice.
