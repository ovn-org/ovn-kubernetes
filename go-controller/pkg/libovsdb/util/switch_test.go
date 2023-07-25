package util

import (
	"fmt"
	"net"
	"testing"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	libovsdbtest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
)

func TestUpdateNodeSwitchExcludeIPs(t *testing.T) {
	nodeName := "ovn-control-plane"

	fakeManagementPort := &nbdb.LogicalSwitchPort{
		Name: types.K8sPrefix + nodeName,
		UUID: types.K8sPrefix + nodeName + "-uuid",
	}

	fakeHoPort := &nbdb.LogicalSwitchPort{
		Name: types.HybridOverlayPrefix + nodeName,
		UUID: types.HybridOverlayPrefix + nodeName + "-uuid",
	}

	tests := []struct {
		desc                    string
		inpSubnetStr            string
		setCfgHybridOvlyEnabled bool
		initialNbdb             libovsdbtest.TestSetup
		expectedNbdb            libovsdbtest.TestSetup
	}{
		{
			desc: "IPv6 CIDR, excludes ips empty",
			initialNbdb: libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					&nbdb.LogicalSwitch{
						Name:        nodeName,
						UUID:        nodeName + "-uuid",
						Ports:       []string{fakeManagementPort.UUID, fakeHoPort.UUID},
						OtherConfig: map[string]string{"ipv6_prefix": "ipv6_prefix"},
					},
					fakeManagementPort,
					fakeHoPort,
				},
			},
			expectedNbdb: libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					&nbdb.LogicalSwitch{
						Name:        nodeName,
						UUID:        nodeName + "-uuid",
						Ports:       []string{fakeManagementPort.UUID, fakeHoPort.UUID},
						OtherConfig: map[string]string{"ipv6_prefix": "ipv6_prefix"},
					},
					fakeManagementPort,
					fakeHoPort,
				},
			},
			inpSubnetStr: "fd04:3e42:4a4e:3381::/64",
		},
		{
			desc:                    "IPv4 CIDR, config.HybridOverlayEnable=true, haveHybridOverlayPort=true and haveManagementPort=true, excludes ips empty",
			inpSubnetStr:            "192.168.1.0/24",
			setCfgHybridOvlyEnabled: true,
			initialNbdb: libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					&nbdb.LogicalSwitch{
						UUID:  nodeName + "-uuid",
						Name:  nodeName,
						Ports: []string{fakeManagementPort.UUID, fakeHoPort.UUID},
						OtherConfig: map[string]string{
							"subnet":      "subnet",
							"exclude_ips": "192.168.1.2..192.168.1.3",
						},
					},
					fakeManagementPort,
					fakeHoPort,
				},
			},
			expectedNbdb: libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					&nbdb.LogicalSwitch{
						UUID:  nodeName + "-uuid",
						Name:  nodeName,
						Ports: []string{fakeManagementPort.UUID, fakeHoPort.UUID},
						OtherConfig: map[string]string{
							"subnet": "subnet",
						},
					},
					fakeManagementPort,
					fakeHoPort,
				},
			},
		},
		{
			desc:                    "IPv4 CIDR, config.HybridOverlayEnable=true, haveHybridOverlayPort=false and haveManagementPort=false, excludes HO and MP ips",
			inpSubnetStr:            "192.168.1.0/24",
			setCfgHybridOvlyEnabled: true,
			initialNbdb: libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					&nbdb.LogicalSwitch{
						UUID:  nodeName + "-uuid",
						Name:  nodeName,
						Ports: []string{},
						OtherConfig: map[string]string{
							"subnet":      "subnet",
							"exclude_ips": "192.168.1.2..192.168.1.3",
						},
					},
				},
			},
			expectedNbdb: libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					&nbdb.LogicalSwitch{
						UUID:  nodeName + "-uuid",
						Name:  nodeName,
						Ports: []string{},
						OtherConfig: map[string]string{
							"subnet":      "subnet",
							"exclude_ips": "192.168.1.2..192.168.1.3",
						},
					},
				},
			},
		},
		{
			desc:                    "IPv4 CIDR, config.HybridOverlayEnable=true, sets haveHybridOverlayPort=false and haveManagementPort=true, excludes HO ip",
			inpSubnetStr:            "192.168.1.0/24",
			setCfgHybridOvlyEnabled: true,
			initialNbdb: libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					&nbdb.LogicalSwitch{
						UUID:  nodeName + "-uuid",
						Name:  nodeName,
						Ports: []string{fakeManagementPort.UUID},
						OtherConfig: map[string]string{
							"subnet":      "subnet",
							"exclude_ips": "192.168.1.2..192.168.1.3",
						},
					},
					fakeManagementPort,
				},
			},
			expectedNbdb: libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					&nbdb.LogicalSwitch{
						UUID:  nodeName + "-uuid",
						Name:  nodeName,
						Ports: []string{fakeManagementPort.UUID},
						OtherConfig: map[string]string{
							"subnet":      "subnet",
							"exclude_ips": "192.168.1.3",
						},
					},
					fakeManagementPort,
				},
			},
		},
		{
			desc:                    "IPv4 CIDR, config.HybridOverlayEnable=true, sets haveHybridOverlayPort=true and haveManagementPort=false, excludes MP ip",
			inpSubnetStr:            "192.168.1.0/24",
			setCfgHybridOvlyEnabled: true,
			initialNbdb: libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					&nbdb.LogicalSwitch{
						UUID:  nodeName + "-uuid",
						Name:  nodeName,
						Ports: []string{fakeManagementPort.UUID},
						OtherConfig: map[string]string{
							"subnet":      "subnet",
							"exclude_ips": "192.168.1.2..192.168.1.3",
						},
					},
					fakeHoPort,
				},
			},
			expectedNbdb: libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					&nbdb.LogicalSwitch{
						UUID:  nodeName + "-uuid",
						Name:  nodeName,
						Ports: []string{fakeManagementPort.UUID},
						OtherConfig: map[string]string{
							"subnet":      "subnet",
							"exclude_ips": "192.168.1.2",
						},
					},
					fakeHoPort,
				},
			},
		},
		{
			desc:                    "IPv4 CIDR, config.HybridOverlayEnable=false, haveManagementPort=true, excludes ips empty",
			inpSubnetStr:            "192.168.1.0/24",
			setCfgHybridOvlyEnabled: false,
			initialNbdb: libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					&nbdb.LogicalSwitch{
						UUID:  nodeName + "-uuid",
						Name:  nodeName,
						Ports: []string{fakeManagementPort.UUID},
						OtherConfig: map[string]string{
							"subnet":      "subnet",
							"exclude_ips": "192.168.1.2",
						},
					},
					fakeManagementPort,
				},
			},
			expectedNbdb: libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					&nbdb.LogicalSwitch{
						UUID:  nodeName + "-uuid",
						Name:  nodeName,
						Ports: []string{fakeManagementPort.UUID},
						OtherConfig: map[string]string{
							"subnet": "subnet",
						},
					},
					fakeManagementPort,
				},
			},
		},
		{
			desc:                    "IPv4 CIDR, config.HybridOverlayEnable=false, haveManagementPort=false, excludes MP ip",
			inpSubnetStr:            "192.168.1.0/24",
			setCfgHybridOvlyEnabled: false,
			initialNbdb: libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					&nbdb.LogicalSwitch{
						UUID:  nodeName + "-uuid",
						Name:  nodeName,
						Ports: []string{fakeManagementPort.UUID},
						OtherConfig: map[string]string{
							"subnet":      "subnet",
							"exclude_ips": "192.168.1.2",
						},
					},
				},
			},
			expectedNbdb: libovsdbtest.TestSetup{
				NBData: []libovsdbtest.TestData{
					&nbdb.LogicalSwitch{
						UUID:  nodeName + "-uuid",
						Name:  nodeName,
						Ports: []string{fakeManagementPort.UUID},
						OtherConfig: map[string]string{
							"subnet":      "subnet",
							"exclude_ips": "192.168.1.2",
						},
					},
				},
			},
		},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprintf("%d:%s", i, tc.desc), func(t *testing.T) {
			nbClient, cleanup, err := libovsdbtest.NewNBTestHarness(tc.initialNbdb, nil)
			if err != nil {
				t.Fatal(fmt.Errorf("test: \"%s\" failed to create test harness: %v", tc.desc, err))
			}
			t.Cleanup(cleanup.Cleanup)

			_, ipnet, err := net.ParseCIDR(tc.inpSubnetStr)
			if err != nil {
				t.Fail()
			}
			var e error
			if tc.setCfgHybridOvlyEnabled {
				config.HybridOverlay.Enabled = true
				if e = UpdateNodeSwitchExcludeIPs(nbClient, nodeName, ipnet); e != nil {
					t.Fatal(fmt.Errorf("failed to update NodeSwitchExcludeIPs with Hybrid Overlay enabled err: %v", e))
				}
				config.HybridOverlay.Enabled = false
			} else {
				if e = UpdateNodeSwitchExcludeIPs(nbClient, nodeName, ipnet); e != nil {
					t.Fatal(fmt.Errorf("failed to update NodeSwitchExcludeIPs with Hybrid Overlay disabled err: %v", e))
				}

			}

			matcher := libovsdbtest.HaveDataIgnoringUUIDs(tc.expectedNbdb.NBData)
			success, err := matcher.Match(nbClient)
			if !success {
				t.Fatal(fmt.Errorf("test: \"%s\" didn't match expected with actual, err: %v", tc.desc, matcher.FailureMessage(nbClient)))
			}
			if err != nil {
				t.Fatal(fmt.Errorf("test: \"%s\" encountered error: %v", tc.desc, err))
			}
		})
	}
}
