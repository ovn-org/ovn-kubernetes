package egressfirewall

import (
	"fmt"
	"testing"

	"github.com/miekg/dns"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	util "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	//mock "github.com/stretchr/testify/mock"
	efapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1"
	util_mocks "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/mocks"
)

const (
	aclWrongDirectionUUID          string = "1a3dfc82-2749-4931-9190-c30e7c0ecea3"
	obsoleteACLOnClusterRouterUUID string = "6d3142fc-53e8-4ac1-88e6-46094a5a9957"
	spuriousExternalID             string = "egressFirewall=namespace"
	spuriousNodeSwitchACLUUID      string = "58a1ef18-3649-11eb-bd94-a8a1590cda29"
	logicalSwitchUUID              string = "2e290f10-3652-11eb-839b-a8a1590cda29"
)

func TestRepairEgressFirewall(t *testing.T) {

	config.IPv4Mode = true
	defer func() {
		config.IPv4Mode = false
	}()

	tests := []struct {
		name                string
		isSharedGatewayMode bool
		ovnCmd              []ovntest.ExpectedCmd
	}{
		{
			name:                "the repair function runs the correctly  when no repair is needed in localGateway mode",
			isSharedGatewayMode: false,
			ovnCmd: []ovntest.ExpectedCmd{
				{
					Cmd: fmt.Sprintf("ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid --format=table find acl priority<=%s priority>=%s direction=%s",
						types.EgressFirewallStartPriority,
						types.MinimumReservedEgressFirewallPriority,
						types.DirectionFromLPort),
					Output: "",
				},
				{
					Cmd: fmt.Sprintf("ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid --format=table find logical_router_policy priority<=%s priority>=%s",
						types.EgressFirewallStartPriority,
						types.MinimumReservedEgressFirewallPriority),
					Output: "",
				},
				{
					Cmd: fmt.Sprintf("ovn-nbctl --timeout=15 --data=bare --no-heading --columns=external_id --format=table find acl priority<=%s priority>=%s",
						types.EgressFirewallStartPriority,
						types.MinimumReservedEgressFirewallPriority),
					Output: "",
				},
			},
		},
		{
			name:                "the repair function runs the correctly  when repairs are needed in localGateway mode",
			isSharedGatewayMode: false,
			ovnCmd: []ovntest.ExpectedCmd{
				{
					Cmd: fmt.Sprintf("ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid --format=table find acl priority<=%s priority>=%s direction=%s",
						types.EgressFirewallStartPriority,
						types.MinimumReservedEgressFirewallPriority,
						types.DirectionFromLPort),
					Output: aclWrongDirectionUUID,
				},
				{
					Cmd: fmt.Sprintf("ovn-nbctl --timeout=15 set acl %s direction=%s",
						aclWrongDirectionUUID,
						types.DirectionToLPort),
					Output: "",
				},
				{
					Cmd: fmt.Sprintf("ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid --format=table find logical_router_policy priority<=%s priority>=%s",
						types.EgressFirewallStartPriority,
						types.MinimumReservedEgressFirewallPriority),
					Output: obsoleteACLOnClusterRouterUUID,
				},
				{
					Cmd: fmt.Sprintf("ovn-nbctl --timeout=15 remove logical_router %s policies %s",
						types.OVNClusterRouter,
						obsoleteACLOnClusterRouterUUID),
					Output: "",
				},
				{
					Cmd: fmt.Sprintf("ovn-nbctl --timeout=15 --data=bare --no-heading --columns=external_id --format=table find acl priority<=%s priority>=%s",
						types.EgressFirewallStartPriority,
						types.MinimumReservedEgressFirewallPriority),
					Output: spuriousExternalID,
				},
				{
					Cmd: fmt.Sprintf("ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid find ACL external-ids:%s",
						spuriousExternalID),
					Output: "",
				},
			},
		},
		{
			name:                "the controller starts and the correct repair queries are run no repairs required in sharedGateway mode",
			isSharedGatewayMode: true,
			ovnCmd: []ovntest.ExpectedCmd{
				{
					Cmd: fmt.Sprintf("ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid --format=table find acl priority<=%s priority>=%s",
						types.EgressFirewallStartPriority,
						types.MinimumReservedEgressFirewallPriority),
					Output: "",
				},
				{
					Cmd: fmt.Sprintf("ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid --format=table find acl priority<=%s priority>=%s direction=from-lport",
						types.EgressFirewallStartPriority,
						types.MinimumReservedEgressFirewallPriority),
					Output: "",
				},
				{
					Cmd: fmt.Sprintf("ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid --format=table find logical_router_policy priority<=%s priority>=%s",
						types.EgressFirewallStartPriority,
						types.MinimumReservedEgressFirewallPriority),
					Output: "",
				},
				{
					Cmd: fmt.Sprintf("ovn-nbctl --timeout=15 --data=bare --no-heading --columns=external_id --format=table find acl priority<=%s priority>=%s",
						types.EgressFirewallStartPriority,
						types.MinimumReservedEgressFirewallPriority),
					Output: "",
				},
			},
		},
		{
			//only testing the repair queries that are exclusive to shared gateway mode
			name:                "the controller starts and the correct repair queries are run for shared gateway mode",
			isSharedGatewayMode: true,
			ovnCmd: []ovntest.ExpectedCmd{
				{
					Cmd: fmt.Sprintf("ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid --format=table find acl priority<=%s priority>=%s",
						types.EgressFirewallStartPriority,
						types.MinimumReservedEgressFirewallPriority),
					Output: spuriousNodeSwitchACLUUID,
				},
				{
					Cmd:    fmt.Sprintf("ovn-nbctl --timeout=15 --data=bare --no-heading --columns=acls list logical_switch %s", nodeName),
					Output: logicalSwitchUUID,
				},
				{
					Cmd: fmt.Sprintf("ovn-nbctl --timeout=15 remove logical_switch testing acls %s",
						spuriousNodeSwitchACLUUID),
					Output: "",
				},
				{
					Cmd: fmt.Sprintf("ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid --format=table find acl priority<=%s priority>=%s direction=from-lport",
						types.EgressFirewallStartPriority,
						types.MinimumReservedEgressFirewallPriority),
					Output: "",
				},
				{
					Cmd: fmt.Sprintf("ovn-nbctl --timeout=15 --data=bare --no-heading --columns=_uuid --format=table find logical_router_policy priority<=%s priority>=%s",
						types.EgressFirewallStartPriority,
						types.MinimumReservedEgressFirewallPriority),
					Output: "",
				},
				{
					Cmd: fmt.Sprintf("ovn-nbctl --timeout=15 --data=bare --no-heading --columns=external_id --format=table find acl priority<=%s priority>=%s",
						types.EgressFirewallStartPriority,
						types.MinimumReservedEgressFirewallPriority),
					Output: "",
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config.Gateway.Mode = config.GatewayModeLocal
			if tt.isSharedGatewayMode {
				config.Gateway.Mode = config.GatewayModeShared
			}
			mockDNSOps := new(util_mocks.DNSOps)
			util.SetDNSLibOpsMockInst(mockDNSOps)
			// always return success from NewEgressDNS because we are not testing that here
			mockDNSOps.On("ClientConfigFromFile", "/etc/resolv.conf").Return(&dns.ClientConfig{}, nil)
			controller := newController()
			fexec := ovntest.NewLooseCompareFakeExec()
			for _, cmd := range tt.ovnCmd {
				cmd := cmd
				fexec.AddFakeCmd(&cmd)

			}
			err := util.SetExec(fexec)
			if err != nil {
				t.Errorf("fexec error: %v", err)
			}
			//always return an empty set of egressFirewalls
			controller.kubeMocks.On("GetEgressFirewalls").Return(&efapi.EgressFirewallList{}, nil)
			if err := controller.repair.runOnce(); err != nil {
				t.Errorf("Unexpected error: %v", err)
			}
		})
	}

}
