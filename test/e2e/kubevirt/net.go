package kubevirt

import (
	"fmt"
)

func GenerateAddressDiscoveryConfigurationCommand(iface string) string {
	return fmt.Sprintf(`
nmcli c mod %[1]s ipv4.addresses "" ipv6.addresses "" ipv4.gateway "" ipv6.gateway "" ipv6.method auto ipv4.method auto ipv6.addr-gen-mode eui64
nmcli d reapply %[1]s`, iface)
}
