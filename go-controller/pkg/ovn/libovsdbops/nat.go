package libovsdbops

import (
	"fmt"
	"net"

	libovsdbclient "github.com/ovn-org/libovsdb/client"
	libovsdb "github.com/ovn-org/libovsdb/ovsdb"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
)

func buildRouterNAT(
	natType nbdb.NATType,
	externalIP string,
	logicalIP string,
	logicalPort string,
	externalMac string,
) *nbdb.NAT {
	nat := &nbdb.NAT{
		Type:       natType,
		ExternalIP: externalIP,
		LogicalIP:  logicalIP,
		Options:    map[string]string{"stateless": "false"},
	}

	if logicalPort != "" {
		nat.LogicalPort = []string{logicalPort}
	}

	if externalMac != "" {
		nat.ExternalMAC = []string{externalMac}
	}

	return nat
}

func findNat(nbClient libovsdbclient.Client, natType nbdb.NATType, externalIP string, logicalIP string) ([]string, error) {
	nats := []nbdb.NAT{}
	// Use "" as the wildcard value for externalIP and logicalIP parameters.
	err := nbClient.WhereCache(func(item *nbdb.NAT) bool {
		return item.Type == natType &&
			(externalIP == "" || item.ExternalIP == externalIP) &&
			(logicalIP == "" || item.LogicalIP == logicalIP)
	}).List(&nats)

	if err != nil {
		return nil, fmt.Errorf("error finding NAT entries for type %s external IP %s logical IP %s: %v",
			natType, externalIP, logicalIP, err)
	}

	natUUIDs := make([]string, 0, len(nats))
	for _, nat := range nats {
		natUUIDs = append(natUUIDs, nat.UUID)
	}
	return natUUIDs, nil
}

func findNatFromLogicalPort(nbClient libovsdbclient.Client, natType nbdb.NATType, logicalPort string) ([]string, error) {
	nats := []nbdb.NAT{}
	err := nbClient.WhereCache(func(item *nbdb.NAT) bool {
		if item.Type == natType {
			for _, lp := range item.LogicalPort {
				if lp == logicalPort {
					return true
				}
			}
		}
		return false
	}).List(&nats)

	if err != nil {
		return nil, fmt.Errorf("error finding NAT entries for type %s logical port %s: %v",
			natType, logicalPort, err)
	}

	natUUIDs := make([]string, 0, len(nats))
	for _, nat := range nats {
		natUUIDs = append(natUUIDs, nat.UUID)
	}
	return natUUIDs, nil
}

// Based on the NAT type and the mask, return the string that should be used
// for NAT operations for finding and storing logical IP in NAT.
func logicalIPString(natType nbdb.NATType, logicalIP net.IPNet) string {
	logicalIPMask, _ := logicalIP.Mask.Size()
	if natType == nbdb.NATTypeSNAT && logicalIPMask != 32 && logicalIPMask != 128 {
		return logicalIP.String()
	}
	return logicalIP.IP.String()
}

// CreateOrUpdateRouterNAT checks if a NAT entry exists in OVN for a specific router and updates it if necessary.
func CreateOrUpdateRouterNAT(nbClient libovsdbclient.Client, routerName string,
	natType nbdb.NATType, externalIP net.IP, logicalIP *net.IPNet, logicalPort string,
	externalMac string) error {

	if logicalIP == nil {
		return fmt.Errorf("logicalIP cannot be nil when setting NAT for router %s", routerName)
	}

	// There can only be one NAT entry for non-snat types, since external IP must be unique.
	// Being so, allow updating of the NAT's logical IP when using "wildcard" logicalIP paramater
	// in findNat.
	logicalIPFind := ""
	if natType == nbdb.NATTypeSNAT {
		logicalIPFind = logicalIPString(natType, *logicalIP)
	}
	natUUIDs, err := findNat(nbClient, natType, externalIP.String(), logicalIPFind)
	if err != nil {
		return err
	}

	router, err := FindRouter(nbClient, routerName)
	if err != nil {
		return fmt.Errorf("error getting logical router %s: %v", routerName, err)
	}

	// Find out if NAT is already listed in the logical router. It is okay if
	// natUUIDs is empty at this point; it just means that a NAT row will be created.
	natIndex := -1
	for i := 0; natIndex == -1 && i < len(natUUIDs); i++ {
		for _, rtrNatUUID := range router.Nat {
			if natUUIDs[i] == rtrNatUUID {
				natIndex = i
				break
			}
		}
	}

	ops := []libovsdb.Operation{}
	nat := buildRouterNAT(natType, externalIP.String(), logicalIPString(natType, *logicalIP), logicalPort, externalMac)
	if natIndex == -1 {
		nat.UUID = BuildNamedUUID(fmt.Sprintf("nat_%s_%s_%s_%s_%s",
			natType, externalIP.String(), logicalIP.String(), logicalPort, externalMac))

		op, err := nbClient.Create(nat)
		if err != nil {
			return fmt.Errorf("error creating NAT %s for logical router %s: %v", nat.UUID, routerName, err)
		}
		ops = append(ops, op...)

		ops, err = AddNatsToRouterOps(nbClient, ops, router, nat.UUID)
		if err != nil {
			return err
		}
	} else {
		op, err := nbClient.Where(
			&nbdb.NAT{
				UUID: natUUIDs[natIndex],
			}).Update(nat)
		if err != nil {
			return fmt.Errorf("error updating NAT %s for logical router %s: %v", nat.UUID, routerName, err)
		}
		ops = append(ops, op...)
	}

	_, err = TransactAndCheck(nbClient, ops)
	return err
}

// CreateOrUpdateRouterSNAT checks if a SNAT entry exists in OVN for a specific router and updates it if necessary.
func CreateOrUpdateRouterSNAT(nbClient libovsdbclient.Client, routerName string, externalIP net.IP, logicalIP *net.IPNet) error {
	return CreateOrUpdateRouterNAT(nbClient, routerName, nbdb.NATTypeSNAT, externalIP, logicalIP, "", "")
}

func DeleteRouterNAT(nbClient libovsdbclient.Client, routerName string, natUUIDs ...string) error {
	router, err := FindRouter(nbClient, routerName)
	if err != nil {
		return fmt.Errorf("error getting logical router %s: %v", routerName, err)
	}

	// Filter out NATs that are not related to logical router provided
	delNatUUIDs := make([]string, 0, len(natUUIDs))
	for _, natUUID := range natUUIDs {
		for _, rtrNatUUID := range router.Nat {
			if natUUID == rtrNatUUID {
				delNatUUIDs = append(delNatUUIDs, natUUID)
				break
			}
		}
	}

	ops := []libovsdb.Operation{}
	for _, natUUID := range delNatUUIDs {
		op, err := nbClient.Where(
			&nbdb.NAT{
				UUID: natUUID,
			}).Delete()
		if err != nil {
			return fmt.Errorf("error deleting NAT for logical router %s: %v", routerName, err)
		}
		ops = append(ops, op...)
	}

	ops, err = DelNatsFromRouterOps(nbClient, ops, router, delNatUUIDs...)
	if err != nil {
		return err
	}

	_, err = TransactAndCheck(nbClient, ops)
	return err
}

func DeleteRouterDNATAndSNAT(nbClient libovsdbclient.Client, routerName string, externalIP net.IP) error {
	// There can only be one NAT entry for dnat types, since external IP must be unique.
	// Being so, we will use "wildcard" logicalIP paramater in findNat.
	natType := nbdb.NATTypeDNATAndSNAT
	natUUIDs, err := findNat(nbClient, natType, externalIP.String(), "")
	if err != nil {
		return err
	}
	return DeleteRouterNAT(nbClient, routerName, natUUIDs...)
}

func DeleteRouterDNATAndSNATFromLogicalPort(nbClient libovsdbclient.Client, routerName string, logicalPort string) error {
	natType := nbdb.NATTypeDNATAndSNAT
	natUUIDs, err := findNatFromLogicalPort(nbClient, natType, logicalPort)
	if err != nil {
		return err
	}
	return DeleteRouterNAT(nbClient, routerName, natUUIDs...)
}

func DeleteRouterSNAT(nbClient libovsdbclient.Client, routerName string, logicalSubnet net.IPNet) error {
	natType := nbdb.NATTypeSNAT
	natUUIDs, err := findNat(nbClient, natType, "", logicalIPString(natType, logicalSubnet))
	if err != nil {
		return err
	}
	return DeleteRouterNAT(nbClient, routerName, natUUIDs...)
}
