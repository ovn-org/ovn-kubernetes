# User Defined Networks

To debug UDN the ovnkube-trace can dump multiple elements of the topology to 
make easier to match to what network they belong.

## Local gateway VRF table ID numbers for networks

Following command will dump the VRF table IDs from a local gateway system for
UDNs.

```bash
ovnkube-trace -dump-udn-vrf-table-ids
```

The output will be tableIDs indexed by node and network name 
```json
{
    "ovn-control-plane": {
       "net-blue": 1,
       "net-red": 2
    },
    "ovn-worker": {
       "net-blue": 3,
       "net-red": 4
    }
}
```
