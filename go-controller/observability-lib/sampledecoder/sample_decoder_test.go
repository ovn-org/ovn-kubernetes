package sampledecoder

import (
	"testing"

	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	libovsdbutil "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/stretchr/testify/assert"
)

func TestCreateOrUpdateACL(t *testing.T) {
	event := newACLEvent(&nbdb.ACL{
		Action: nbdb.ACLActionAllow,
		ExternalIDs: map[string]string{
			libovsdbops.OwnerTypeKey.String():       libovsdbops.NetworkPolicyOwnerType,
			libovsdbops.ObjectNameKey.String():      "foo",
			libovsdbops.PolicyDirectionKey.String(): string(libovsdbutil.ACLIngress),
		},
	})
	assert.Equal(t, "Allowed by network policy foo, direction Ingress", event.String())

	event = newACLEvent(&nbdb.ACL{
		Action: nbdb.ACLActionAllow,
		ExternalIDs: map[string]string{
			libovsdbops.OwnerTypeKey.String():       libovsdbops.AdminNetworkPolicyOwnerType,
			libovsdbops.ObjectNameKey.String():      "foo",
			libovsdbops.PolicyDirectionKey.String(): string(libovsdbutil.ACLIngress),
		},
	})
	assert.Equal(t, "Allowed by admin network policy foo, direction Ingress", event.String())

	event = newACLEvent(&nbdb.ACL{
		Action: nbdb.ACLActionAllow,
		ExternalIDs: map[string]string{
			libovsdbops.OwnerTypeKey.String():  libovsdbops.EgressFirewallOwnerType,
			libovsdbops.ObjectNameKey.String(): "foo",
		},
	})
	assert.Equal(t, "Allowed by egress firewall in namespace foo", event.String())
	assert.Equal(t, "Egress", event.Direction)
}
