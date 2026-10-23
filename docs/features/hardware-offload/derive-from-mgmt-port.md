# From PCI Address Gateway Interface Feature

## Overview

The "derive-from-mgmt-port" gateway interface feature is a new capability in OVN-Kubernetes that enables automatic gateway interface resolution in DPU (Data Processing Unit) host mode deployments. This feature automatically discovers and configures the appropriate Physical Function (PF) interface as the gateway interface based on the Virtual Function (VF) used for the management port.

## Problem Statement

In DPU deployments, the host typically has access to Virtual Functions (VFs) for management purposes, while the Physical Functions (PFs) are used for external connectivity. Previously, administrators had to manually specify the gateway interface, which required:

1. Knowledge of the hardware topology
2. Manual mapping of VF to PF relationships
3. Configuration updates when hardware changes
4. Potential for misconfiguration

## Solution

The "derive-from-mgmt-port" feature automates the gateway interface discovery process by:

1. **Automatic Discovery**: Automatically finds the PF interface associated with the management port VF
2. **Hardware Abstraction**: Eliminates the need for manual hardware topology knowledge
3. **Startup Resolution**: Resolves the gateway interface during node initialization
4. **Reduced Configuration**: Simplifies deployment configuration

## Benefits

### For Administrators

- **Simplified Configuration**: No need to manually specify gateway interfaces
- **Reduced Errors**: Eliminates manual mapping errors
- **Hardware Agnostic**: Works with any SR-IOV capable hardware
- **Adaptation at startup**: Derives the gateway interface from the current management-port VF when the node starts

### For Operations

- **Faster Deployment**: Reduced configuration time
- **Consistent Setup**: Standardized gateway interface selection
- **Reduced Maintenance**: Less manual intervention required
- **Better Reliability**: Fewer configuration-related issues

### For Development

- **Cleaner Code**: Centralized gateway interface logic
- **Better Testing**: Comprehensive unit test coverage
- **Extensible Design**: Foundation for future enhancements

## Technical Implementation

Always check the dependencies on the [Requirements page](../requirements.md)

### Code Changes

1. **New Constant**: Added `DeriveFromMgmtPort = "derive-from-mgmt-port"` constant in `go-controller/pkg/types/const.go`
2. **Enhanced Logic**: Extended gateway initialization in `go-controller/pkg/node/default_node_network_controller.go`
3. **Comprehensive Testing**: Added unit tests covering success and failure scenarios

### Key Functions

- `getManagementPortNetDev()`: Resolves management port device name
- `GetPciFromNetDevice()`: Retrieves PCI address from network device
- `GetPfPciFromVfPci()`: Resolves PF PCI address from VF PCI address
- `GetNetDevicesFromPci()`: Discovers network devices associated with PCI address

### Error Handling

The implementation includes robust error handling for:
- Missing network devices
- PCI address resolution failures
- SR-IOV operation failures
- Hardware compatibility issues

## Configuration Examples

### Basic Configuration

```bash
--ovnkube-node-mode=dpu-host
--ovnkube-node-mgmt-port-dp-resource-name=example.com/sriov_pool
--gateway-interface=derive-from-mgmt-port
```

Alternatively, the management port VF can be specified directly by netdev name
using `--ovnkube-node-mgmt-port-netdev`. Note that this approach does **not**
support network segmentation (UDN); use `--ovnkube-node-mgmt-port-dp-resource-name`
instead when network segmentation is enabled.

```bash
--ovnkube-node-mode=dpu-host
--ovnkube-node-mgmt-port-netdev=pf0vf0
--gateway-interface=derive-from-mgmt-port
```

> **Note:** When both `--ovnkube-node-mgmt-port-dp-resource-name` and
> `--ovnkube-node-mgmt-port-netdev` are specified,
> `--ovnkube-node-mgmt-port-dp-resource-name` takes priority and the netdev
> provided by `--ovnkube-node-mgmt-port-netdev` will be overridden by the
> device chosen from the SR-IOV resource pool. Additionally, when network
> segmentation (UDN) is enabled in DPU-host mode,
> `--ovnkube-node-mgmt-port-dp-resource-name` is **mandatory** and
> `--ovnkube-node-mgmt-port-netdev` is not supported.

### Helm Configuration

```yaml
ovnkube-node-dpu-host:
  mgmtPortVFResourceName: example.com/sriov_pool
  gatewayOpts: "--gateway-interface=derive-from-mgmt-port"
```

Or using the netdev name directly (not supported with network segmentation / UDN):

```yaml
ovnkube-node-dpu-host:
  nodeMgmtPortNetdev: pf0vf0
  gatewayOpts: "--gateway-interface=derive-from-mgmt-port"
```

### Configuration File

```ini
[OvnKubeNode]
mode=dpu-host
mgmt-port-dp-resource-name=example.com/sriov_pool

[Gateway]
interface=derive-from-mgmt-port
```

Or using the netdev name directly (not supported with network segmentation / UDN):

```ini
[OvnKubeNode]
mode=dpu-host
mgmt-port-netdev=pf0vf0

[Gateway]
interface=derive-from-mgmt-port
```

## Migration Guide

### From Manual Configuration

**Before:**
```bash
--gateway-interface=eth0
```

**After:**
```bash
--gateway-interface=derive-from-mgmt-port
```

### Verification Steps

1. Verify SR-IOV configuration is correct
2. Ensure management port device is properly configured
3. Check that PF interfaces are available
4. Monitor logs for successful gateway interface resolution

## Testing

### Unit Tests

Comprehensive unit tests cover:
- Successful gateway interface resolution
- Error handling for missing devices
- PCI address resolution failures
- Network device discovery failures

### Integration Tests

The feature integrates with existing:
- Gateway initialization
- DPU host mode functionality
- SR-IOV operations
- Network configuration

## Future Enhancements

Potential improvements include:
- Support for multiple gateway interfaces
- Enhanced device selection criteria
- Integration with device plugins
- Support for non-SR-IOV hardware
- Advanced error reporting and diagnostics

## Related Documentation

- [DPU Gateway Interface Configuration](dpu-gateway-interface.md)
- [DPU Support](dpu-support.md)
- [Gateway Accelerated Interface Configuration](../../design/gateway-accelerated-interface-configuration.md)
- [Configuration Guide](../../getting-started/configuration.md)

## Support

For issues related to this feature:
1. Check the troubleshooting section in the DPU Gateway Interface Configuration guide
2. Verify SR-IOV hardware and driver support
3. Review error messages and logs
4. Consult the OVN-Kubernetes community for additional support 