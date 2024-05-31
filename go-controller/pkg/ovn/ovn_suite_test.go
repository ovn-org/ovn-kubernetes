package ovn

import (
	"fmt"
	"testing"

	"github.com/onsi/ginkgo"
	"github.com/onsi/gomega"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
)

func TestClusterNode(t *testing.T) {
	gomega.RegisterFailHandler(ginkgo.Fail)
	ginkgo.RunSpecs(t, "OVN Operations Suite")
}

type option func(machine *secondaryNetworkExpectationMachine)

type secondaryNetworkExpectationMachine struct {
	fakeOvn               *FakeOVN
	pods                  []testPod
	disablePortSecurity   bool
	allowSendingToUnknown bool
}

func newSecondaryNetworkExpectationMachine(fakeOvn *FakeOVN, pods []testPod, opts ...option) *secondaryNetworkExpectationMachine {
	machine := &secondaryNetworkExpectationMachine{
		fakeOvn: fakeOvn,
		pods:    pods,
	}

	for _, opt := range opts {
		opt(machine)
	}
	return machine
}

func withoutPortSecurity() option {
	return func(machine *secondaryNetworkExpectationMachine) {
		machine.disablePortSecurity = true
	}
}

func allowSendingToUnknownL2Addrs() option {
	return func(machine *secondaryNetworkExpectationMachine) {
		machine.allowSendingToUnknown = true
	}
}

func (em *secondaryNetworkExpectationMachine) expectedLogicalSwitchesAndPorts() []libovsdb.TestData {
	data := []libovsdb.TestData{}
	for _, ocInfo := range em.fakeOvn.secondaryControllers {
		nodeslsps := make(map[string][]string)
		var switchName string
		for _, pod := range em.pods {
			podInfo, ok := pod.secondaryPodInfos[ocInfo.bnc.GetNetworkName()]
			if !ok {
				continue
			}
			for nad, portInfo := range podInfo.allportInfo {
				portName := portInfo.portName
				var lspUUID string
				if len(portInfo.portUUID) == 0 {
					lspUUID = portName + "-UUID"
				} else {
					lspUUID = portInfo.portUUID
				}
				podAddr := fmt.Sprintf("%s %s", portInfo.podMAC, portInfo.podIP)
				lsp := &nbdb.LogicalSwitchPort{
					UUID:      lspUUID,
					Name:      portName,
					Addresses: []string{podAddr},
					ExternalIDs: map[string]string{
						"pod":                       "true",
						"namespace":                 pod.namespace,
						ovntypes.NetworkExternalID:  ocInfo.bnc.GetNetworkName(),
						ovntypes.NADExternalID:      nad,
						ovntypes.TopologyExternalID: ocInfo.bnc.TopologyType(),
					},
					Options: map[string]string{
						"requested-chassis": pod.nodeName,
						"iface-id-ver":      pod.podName,
					},
					PortSecurity: []string{podAddr},
				}

				if em.disablePortSecurity {
					lsp.PortSecurity = nil
				}
				if em.allowSendingToUnknown {
					lsp.Addresses = append(lsp.Addresses, "unknown")
				}

				if pod.noIfaceIdVer {
					delete(lsp.Options, "iface-id-ver")
				}
				data = append(data, lsp)
				switch ocInfo.bnc.TopologyType() {
				case ovntypes.Layer3Topology:
					switchName = ocInfo.bnc.GetNetworkScopedName(pod.nodeName)
				case ovntypes.Layer2Topology:
					switchName = ocInfo.bnc.GetNetworkScopedName(ovntypes.OVNLayer2Switch)
				case ovntypes.LocalnetTopology:
					switchName = ocInfo.bnc.GetNetworkScopedName(ovntypes.OVNLocalnetSwitch)
				}
				nodeslsps[switchName] = append(nodeslsps[switchName], lspUUID)
			}
			data = append(data, &nbdb.LogicalSwitch{
				UUID:        switchName + "-UUID",
				Name:        switchName,
				Ports:       nodeslsps[switchName],
				ExternalIDs: map[string]string{ovntypes.NetworkExternalID: ocInfo.bnc.GetNetworkName()},
			})
		}
	}
	return data
}
