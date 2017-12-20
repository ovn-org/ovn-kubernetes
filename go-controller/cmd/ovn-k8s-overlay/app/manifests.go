package app

const (
	bridgeTemplateDebian = `
allow-ovs br-int
iface br-int inet manual
    ovs_type OVSBridge
    ovs_ports {{.InterfaceName}}
    ovs_extra set bridge br-int fail_mode=secure
`
	interfaceTemplateDebian = `
allow-br-int {{.InterfaceName}}
iface {{.InterfaceName}} inet static
    address {{.Address}}
    netmask {{.Netmask}}
    ovs_type OVSIntPort
    ovs_bridge br-int
    ovs_extra set interface $IFACE mac=\"{{.Mac}}\" external-ids:iface-id={{.IfaceID}}
    up route add -net {{.ClusterIP}} netmask {{.ClusterMask}} gw {{.GwIP}}
    down route del -net {{.ClusterIP}} netmask {{.ClusterMask}} gw {{.GwIP}}
`
	bridgeTemplateRedhat = `
DEVICE=br-int
ONBOOT=yes
DEVICETYPE=ovs
TYPE=OVSBridge
OVS_EXTRA="set bridge br-int fail_mode=secure"
`
	interfaceTemplateRedhat = `
DEVICE={{.InterfaceName}}
DEVICETYPE=ovs
TYPE=OVSIntPort
OVS_BRIDGE=br-int
IP_ADDR={{.Address}}
NETMASK={{.Netmask}}
OVS_EXTRA="set interface $DEVICE mac=\"{{.Mac}}\" external-ids:iface-id={{.IfaceID}}"
`
	routeTemplate = `
ADDRESS0={{.ClusterIP}}
NETMASK0={{.ClusterMask}}
GATEWAY0={{.GwIP}}
`
)
