package util

import (
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

func GetPortGroupName(dbIDs *ops.DbObjectIDs) string {
	return util.HashForOVN(dbIDs.GetExternalIDs()[ops.PrimaryIDKey.String()])
}

func BuildPortGroup(pgIDs *ops.DbObjectIDs, ports []*nbdb.LogicalSwitchPort, acls []*nbdb.ACL) *nbdb.PortGroup {
	externalIDs := pgIDs.GetExternalIDs()
	pg := nbdb.PortGroup{
		Name:        util.HashForOVN(externalIDs[ops.PrimaryIDKey.String()]),
		ExternalIDs: externalIDs,
	}

	if len(acls) > 0 {
		pg.ACLs = make([]string, 0, len(acls))
		for _, acl := range acls {
			pg.ACLs = append(pg.ACLs, acl.UUID)
		}
	}

	if len(ports) > 0 {
		pg.Ports = make([]string, 0, len(ports))
		for _, port := range ports {
			pg.Ports = append(pg.Ports, port.UUID)
		}
	}

	return &pg
}
