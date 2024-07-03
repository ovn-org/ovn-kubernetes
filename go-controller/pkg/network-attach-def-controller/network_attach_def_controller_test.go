package networkAttachDefController

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	nettypes "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	"github.com/onsi/gomega"
	"github.com/onsi/gomega/format"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/cache"

	ovncnitypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

type testNetworkController struct {
	util.NetInfo
	tncm *testNetworkControllerManager
}

func (tnc *testNetworkController) Start(context.Context) error {
	tnc.tncm.Lock()
	defer tnc.tncm.Unlock()
	tnc.tncm.started = append(tnc.tncm.started, testNetworkKey(tnc))
	return nil
}

func (tnc *testNetworkController) Stop() {
	tnc.tncm.Lock()
	defer tnc.tncm.Unlock()
	tnc.tncm.stopped = append(tnc.tncm.stopped, testNetworkKey(tnc))
}

func (tnc *testNetworkController) Cleanup() error {
	tnc.tncm.Lock()
	defer tnc.tncm.Unlock()
	tnc.tncm.cleaned = append(tnc.tncm.cleaned, testNetworkKey(tnc))
	return nil
}

// GomegaString is used to avoid printing embedded mutexes which can cause a
// race
func (tnc *testNetworkController) GomegaString() string {
	return format.Object(tnc.NetInfo.GetNetworkName(), 1)
}

func testNetworkKey(nInfo util.NetInfo) string {
	return nInfo.GetNetworkName() + " " + nInfo.TopologyType()
}

type testNetworkControllerManager struct {
	sync.Mutex
	controllers map[string]NetworkController
	started     []string
	stopped     []string
	cleaned     []string
}

func (tncm *testNetworkControllerManager) NewNetworkController(netInfo util.NetInfo) (NetworkController, error) {
	tncm.Lock()
	defer tncm.Unlock()
	t := &testNetworkController{
		NetInfo: netInfo,
		tncm:    tncm,
	}
	tncm.controllers[testNetworkKey(netInfo)] = t
	return t, nil
}

func (tncm *testNetworkControllerManager) CleanupDeletedNetworks(allControllers []NetworkController) error {
	return nil
}

func TestNetAttachDefinitionController(t *testing.T) {
	network_A := &ovncnitypes.NetConf{
		Topology: types.Layer2Topology,
		NetConf: cnitypes.NetConf{
			Name: "network_A",
			Type: "ovn-k8s-cni-overlay",
		},
		MTU: 1400,
	}
	network_A_incompatible := &ovncnitypes.NetConf{
		Topology: types.LocalnetTopology,
		NetConf: cnitypes.NetConf{
			Name: "network_A",
			Type: "ovn-k8s-cni-overlay",
		},
		MTU: 1400,
	}

	network_B := &ovncnitypes.NetConf{
		Topology: types.LocalnetTopology,
		NetConf: cnitypes.NetConf{
			Name: "network_B",
			Type: "ovn-k8s-cni-overlay",
		},
		MTU: 1400,
	}

	network_Default := &ovncnitypes.NetConf{
		Topology: types.Layer3Topology,
		NetConf: cnitypes.NetConf{
			Name: "default",
			Type: "ovn-k8s-cni-overlay",
		},
		MTU: 1400,
	}

	type args struct {
		nad     string
		network *ovncnitypes.NetConf
		wantErr bool
	}
	type expected struct {
		network *ovncnitypes.NetConf
		nads    []string
	}
	tests := []struct {
		name     string
		args     []args
		expected []expected
	}{
		{
			name: "NAD on default network should be skipped",
			args: []args{
				{
					nad:     "test/nad_1",
					network: network_Default,
				},
			},
			expected: []expected{},
		},
		{
			name: "NAD added",
			args: []args{
				{
					nad:     "test/nad_1",
					network: network_A,
				},
			},
			expected: []expected{
				{
					network: network_A,
					nads:    []string{"test/nad_1"},
				},
			},
		},
		{
			name: "NAD added then deleted",
			args: []args{
				{
					nad:     "test/nad_1",
					network: network_A,
				},
				{
					nad: "test/nad_1",
				},
			},
		},
		{
			name: "two NADs added",
			args: []args{
				{
					nad:     "test/nad_1",
					network: network_A,
				},
				{
					nad:     "test/nad_2",
					network: network_A,
				},
			},
			expected: []expected{
				{
					network: network_A,
					nads:    []string{"test/nad_1", "test/nad_2"},
				},
			},
		},
		{
			name: "two NADs added then one deleted",
			args: []args{
				{
					nad:     "test/nad_1",
					network: network_A,
				},
				{
					nad:     "test/nad_2",
					network: network_A,
				},
				{
					nad: "test/nad_1",
				},
			},
			expected: []expected{
				{
					network: network_A,
					nads:    []string{"test/nad_2"},
				},
			},
		},
		{
			name: "two NADs added then deleted",
			args: []args{
				{
					nad:     "test/nad_1",
					network: network_A,
				},
				{
					nad:     "test/nad_2",
					network: network_A,
				},
				{
					nad: "test/nad_2",
				},
				{
					nad: "test/nad_1",
				},
			},
		},
		{
			name: "NAD added then updated to different network",
			args: []args{
				{
					nad:     "test/nad_1",
					network: network_A,
				},
				{
					nad:     "test/nad_1",
					network: network_B,
				},
			},
			expected: []expected{
				{
					network: network_B,
					nads:    []string{"test/nad_1"},
				},
			},
		},
		{
			name: "two NADs added then one updated to different network",
			args: []args{
				{
					nad:     "test/nad_1",
					network: network_A,
				},
				{
					nad:     "test/nad_2",
					network: network_A,
				},
				{
					nad:     "test/nad_1",
					network: network_B,
				},
			},
			expected: []expected{
				{
					network: network_A,
					nads:    []string{"test/nad_2"},
				},
				{
					network: network_B,
					nads:    []string{"test/nad_1"},
				},
			},
		},
		{
			name: "two NADs added then one updated to same network",
			args: []args{
				{
					nad:     "test/nad_1",
					network: network_A,
				},
				{
					nad:     "test/nad_2",
					network: network_B,
				},
				{
					nad:     "test/nad_1",
					network: network_B,
				},
			},
			expected: []expected{
				{
					network: network_B,
					nads:    []string{"test/nad_1", "test/nad_2"},
				},
			},
		},
		{
			name: "NAD added then incompatible NAD added",
			args: []args{
				{
					nad:     "test/nad_1",
					network: network_A,
				},
				{
					nad:     "test/nad_2",
					network: network_A_incompatible,
					wantErr: true,
				},
			},
			expected: []expected{
				{
					network: network_A,
					nads:    []string{"test/nad_1"},
				},
			},
		},
		{
			name: "NAD added then updated to incompatible network",
			args: []args{
				{
					nad:     "test/nad_1",
					network: network_A,
				},
				{
					nad:     "test/nad_1",
					network: network_A_incompatible,
				},
			},
			expected: []expected{
				{
					network: network_A_incompatible,
					nads:    []string{"test/nad_1"},
				},
			},
		},
		{
			name: "two NADs added then one updated to incompatible network",
			args: []args{
				{
					nad:     "test/nad_1",
					network: network_A,
				},
				{
					nad:     "test/nad_2",
					network: network_A,
				},
				{
					nad:     "test/nad_1",
					network: network_A_incompatible,
					wantErr: true,
				},
			},
			expected: []expected{
				{
					network: network_A,
					nads:    []string{"test/nad_2"},
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			tncm := &testNetworkControllerManager{
				controllers: map[string]NetworkController{},
			}
			nadController := &NetAttachDefinitionController{
				networks:       map[string]util.NetInfo{},
				nads:           map[string]string{},
				networkManager: newNetworkManager("", tncm),
			}

			g.Expect(nadController.networkManager.Start()).To(gomega.Succeed())

			for _, args := range tt.args {
				namespace, name, err := cache.SplitMetaNamespaceKey(args.nad)
				g.Expect(err).ToNot(gomega.HaveOccurred())

				var nad *nettypes.NetworkAttachmentDefinition
				if args.network != nil {
					args.network.NADName = args.nad
					config, err := json.Marshal(args.network)
					g.Expect(err).ToNot(gomega.HaveOccurred())

					nad = &nettypes.NetworkAttachmentDefinition{
						ObjectMeta: v1.ObjectMeta{
							Name:      name,
							Namespace: namespace,
						},
						Spec: nettypes.NetworkAttachmentDefinitionSpec{
							Config: string(config),
						},
					}
				}

				err = nadController.syncNAD(args.nad, nad)
				if args.wantErr {
					g.Expect(err).To(gomega.HaveOccurred())
				} else {
					g.Expect(err).NotTo(gomega.HaveOccurred())
				}
			}

			meetsExpectations := func(g gomega.Gomega) {
				tncm.Lock()
				defer tncm.Unlock()

				var expectRunning []string
				for _, expected := range tt.expected {
					netInfo, err := util.NewNetInfo(expected.network)
					g.Expect(err).ToNot(gomega.HaveOccurred())

					name := netInfo.GetNetworkName()
					testNetworkKey := testNetworkKey(netInfo)

					// test that the controller have the expected config and NADs
					g.Expect(tncm.controllers).To(gomega.HaveKey(testNetworkKey))
					g.Expect(tncm.controllers[testNetworkKey].Equals(netInfo)).To(gomega.BeTrue(),
						fmt.Sprintf("matching network config for network %s", name))
					g.Expect(tncm.controllers[testNetworkKey].GetNADs()).To(gomega.ConsistOf(expected.nads),
						fmt.Sprintf("matching NADs for network %s", name))
					expectRunning = append(expectRunning, testNetworkKey)
				}
				expectStopped := sets.New(tncm.started...).Difference(sets.New(expectRunning...)).UnsortedList()

				// test that the controllers are started, stopped and cleaned up as expected
				g.Expect(tncm.started).To(gomega.ContainElements(expectRunning), "started network controllers")
				g.Expect(tncm.stopped).To(gomega.ConsistOf(expectStopped), "stopped network controllers")
				g.Expect(tncm.cleaned).To(gomega.ConsistOf(expectStopped), "cleaned up network controllers")
			}

			g.Eventually(meetsExpectations).Should(gomega.Succeed())
			g.Consistently(meetsExpectations).Should(gomega.Succeed())
		})
	}
}
