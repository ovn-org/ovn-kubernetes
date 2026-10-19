# DPU Gateway Interface Configuration

## Overview

In DPU (Data Processing Unit) host mode deployments, OVN-Kubernetes supports automatic gateway interface resolution from PCI address. This feature is particularly useful when the management port is a Virtual Function (VF) and you want to automatically select the corresponding Physical Function (PF) interface as the gateway.

## Background

In DPU deployments, the host typically has access to Virtual Functions (VFs) for management purposes, while the Physical Functions (PFs) are used for external connectivity. The "derive-from-mgmt-port" feature allows OVN-Kubernetes to automatically discover and configure the appropriate PF interface as the gateway interface based on the VF used for the management port.

## How It Works

When configured with `--gateway-interface=derive-from-mgmt-port`, OVN-Kubernetes performs the following steps:

1. **Management Port Resolution**: Gets the management port network device name (specified by `--ovnkube-node-mgmt-port-netdev`)
2. **VF PCI Address Retrieval**: Retrieves the PCI address of the management port device (VF)
3. **PF PCI Address Resolution**: Gets the Physical Function (PF) PCI address from the Virtual Function (VF) PCI address
4. **Network Device Discovery**: Retrieves all network devices associated with the PF PCI address
5. **Interface Selection**: Selects the first available network device as the gateway interface

## Configuration

Always check the dependencies on the [Requirements page](../requirements.md)

### Command Line Options

```bash
--ovnkube-node-mode=dpu-host
--ovnkube-node-mgmt-port-netdev=pf0vf0
--gateway-interface=derive-from-mgmt-port
```

### Configuration File

```ini
[OvnKubeNode]
mode=dpu-host
mgmt-port-netdev=pf0vf0

[Gateway]
interface=derive-from-mgmt-port
```

### Helm Configuration

```yaml
ovnkube-node:
  mode: dpu-host
  mgmtPortNetdev: pf0vf0

gateway:
  interface: derive-from-mgmt-port
```

## DPU host gateway representor interface

This setting is the `DPUHostGatewayRepresentorInterface` field in code and in the Gateway config section (`dpu-host-gateway-representor-interface`).

On real switchdev DPUs, the DPU-side representor for the host uplink (the PF’s representor toward the embedded CPU) is often discovered automatically from switchdev metadata (for example via `phys_port_name`). Simulation environments do not expose that metadata: dpu-sim KinD clusters, dpu-sim virtio-based VMs, or other “simulated DPU” setups. There, OVN-Kubernetes cannot infer which netdev is the host-gateway representor, and you must set it explicitly.

The optional gateway value (`--dpu-host-gateway-representor-interface`) names that DPU-side representor explicitly. When the node builds an OVS gateway bridge from a plain NIC (not an existing OVS bridge), and this value is non-empty, ovnkube adds that interface as a port on the gateway bridge and marks it `other-config:transient=true` so the host’s uplink path is wired through OVS correctly.

Typical combinations:

- **Production switchdev DPU**: Usually leave this unset; discovery via switchdev metadata handles the representor.
- **Simulated DPU**: Set `--simulate-dpu` on the node (see [DPU support](dpu-support.md)) and set `--dpu-host-gateway-representor-interface` to the known representor netdev name (for example the veth or virtio peer your topology uses).

### Command line

```bash
--dpu-host-gateway-representor-interface=<netdev>
```

### Configuration file

```ini
[Gateway]
dpu-host-gateway-representor-interface=<netdev>
```

### Helm (`global` values)

```yaml
global:
  dpuHostGatewayRepresentorInterface: <netdev>
```

If the representor name is wrong or the interface is missing, gateway bridge setup fails with an error when adding the port to the bridge.

## Example Scenario

Consider a DPU setup with the following configuration:

- **Management port device**: `pf0vf0` (Virtual Function)
- **VF PCI address**: `0000:01:02.3`
- **PF PCI address**: `0000:01:00.0`
- **Available PF interfaces**: `eth0`, `eth1`

With `--gateway-interface=derive-from-mgmt-port`, OVN-Kubernetes will:

1. Start with the management port device `pf0vf0`
2. Get its PCI address `0000:01:02.3`
3. Resolve the PF PCI address to `0000:01:00.0`
4. Find all network devices associated with PF `0000:01:00.0`: `eth0`, `eth1`
5. Select `eth0` (first device) as the gateway interface

## Requirements

### Hardware Requirements

- SR-IOV capable network interface card
- Virtual Function (VF) and Physical Function (PF) setup
- Management port configured as a VF

### Software Requirements

- SR-IOV utilities available on the system
- OVN-Kubernetes running in DPU host mode
- Proper VF/PF driver support

### Configuration Requirements

- Must be used in DPU host mode (`--ovnkube-node-mode=dpu-host`)
- Management port netdev must be specified (`--ovnkube-node-mgmt-port-netdev`)
- Gateway interface must be set to `derive-from-mgmt-port`

## Error Handling

The system will return an error in the following scenarios:

### No Network Devices Found

```
no netdevs found for pci address 0000:01:00.0
```

**Cause**: The PF PCI address doesn't have any associated network devices.

**Resolution**: Verify that the PF has network interfaces configured and are visible to the system.

### PCI Address Resolution Failure

```
failed to get PCI address
```

**Cause**: Unable to retrieve the PCI address from the management port device.

**Resolution**: Ensure the management port device exists and is properly configured.

### PF PCI Address Resolution Failure

```
failed to get PF PCI address
```

**Cause**: Unable to resolve the PF PCI address from the VF PCI address.

**Resolution**: Verify SR-IOV configuration and driver support.

### Network Device Discovery Failure

```
failed to get network devices
```

**Cause**: Unable to retrieve network devices associated with the PF PCI address.

**Resolution**: Check SR-IOV utilities and system configuration.

## Troubleshooting

### Verify SR-IOV Configuration

```bash
# Check if SR-IOV is enabled
lspci | grep -i ethernet

# Check VF configuration
ip link show

# Check PF/VF relationship
ls /sys/bus/pci/devices/*/virtfn*
```

### Verify Management Port Device

```bash
# Check if management port device exists
ip link show pf0vf0

# Check PCI address
ethtool -i pf0vf0 | grep bus-info
```

### Debug PCI Address Resolution

```bash
# Get VF PCI address
cat /sys/class/net/pf0vf0/device/address

# Get PF PCI address (if available)
cat /sys/class/net/pf0vf0/device/physfn/address
```

## Integration with Existing Features

### Gateway Accelerated Interface

The "derive-from-mgmt-port" feature is used in conjunction with management interface to select the appropriate gateway accelerated interface.

The management port can be specified through one of the following options:
```
  --ovnkube-node-mgmt-port-netdev)
    OVNKUBE_NODE_MGMT_PORT_NETDEV=$VALUE
```

```
  --ovnkube-node-mgmt-port-dp-resource-name)
    OVNKUBE_NODE_MGMT_PORT_DP_RESOURCE_NAME=$VALUE
```

OVNKUBE_NODE_MGMT_PORT_DP_RESOURCE_NAME has priority over OVNKUBE_NODE_MGMT_PORT_NETDEV and it is easier to use since it points to a SRIOV Device Plugin pool name.

### Multiple Network Support

This feature works with multiple network support and can be used in environments where pods have multiple interfaces connected to different networks.

## Limitations

- Only available in DPU host mode
- Requires SR-IOV capable hardware
- Limited to the first available network device from the PF
- Depends on proper VF/PF driver support
- May not work with all SR-IOV implementations

## Future Enhancements

Potential improvements to this feature could include:

- Support for selecting specific network devices based on criteria
- Integration with device plugin resources
- Support for multiple gateway interfaces
- Enhanced error reporting and diagnostics
- Support for non-SR-IOV hardware configurations