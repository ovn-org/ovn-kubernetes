package networkmanager

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"github.com/onsi/gomega"
	"github.com/onsi/gomega/format"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	nettypes "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/cache"

	ovncnitypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

type testNetworkController struct {
	util.ReconcilableNetInfo
	tcm *testControllerManager
}

func (tnc *testNetworkController) Start(context.Context) error {
	tnc.tcm.Lock()
	defer tnc.tcm.Unlock()
	fmt.Printf("starting network: %s\n", testNetworkKey(tnc))
	tnc.tcm.started = append(tnc.tcm.started, testNetworkKey(tnc))
	return nil
}

func (tnc *testNetworkController) Stop() {
	tnc.tcm.Lock()
	defer tnc.tcm.Unlock()
	tnc.tcm.stopped = append(tnc.tcm.stopped, testNetworkKey(tnc))
}

func (tnc *testNetworkController) Cleanup() error {
	tnc.tcm.Lock()
	defer tnc.tcm.Unlock()
	tnc.tcm.cleaned = append(tnc.tcm.cleaned, testNetworkKey(tnc))
	return nil
}

func (tnc *testNetworkController) Reconcile(netInfo util.NetInfo) error {
	return util.ReconcileNetInfo(tnc.ReconcilableNetInfo, netInfo)
}

// GomegaString is used to avoid printing embedded mutexes which can cause a
// race
func (tnc *testNetworkController) GomegaString() string {
	return format.Object(tnc.GetNetworkName(), 1)
}

func testNetworkKey(nInfo util.NetInfo) string {
	return nInfo.GetNetworkName() + " " + nInfo.TopologyType()
}

type testControllerManager struct {
	sync.Mutex

	defaultNetwork *testNetworkController
	controllers    map[string]NetworkController

	started []string
	stopped []string
	cleaned []string

	raiseErrorWhenCreatingController error

	valid []util.NetInfo
}

func (tcm *testControllerManager) NewNetworkController(netInfo util.NetInfo) (NetworkController, error) {
	tcm.Lock()
	defer tcm.Unlock()
	if tcm.raiseErrorWhenCreatingController != nil {
		return nil, tcm.raiseErrorWhenCreatingController
	}
	t := &testNetworkController{
		ReconcilableNetInfo: util.NewReconcilableNetInfo(netInfo),
		tcm:                 tcm,
	}
	tcm.controllers[testNetworkKey(netInfo)] = t
	return t, nil
}

func (tcm *testControllerManager) CleanupStaleNetworks(validNetworks ...util.NetInfo) error {
	tcm.valid = validNetworks
	return nil
}

func (tcm *testControllerManager) GetDefaultNetworkController() ReconcilableNetworkController {
	return tcm.defaultNetwork
}

func TestNADController(t *testing.T) {
	networkAPrimary := &ovncnitypes.NetConf{
		Topology: types.Layer2Topology,
		NetConf: cnitypes.NetConf{
			Name: "networkAPrimary",
			Type: "ovn-k8s-cni-overlay",
		},
		Subnets: "10.1.130.0/24",
		Role:    types.NetworkRolePrimary,
		MTU:     1400,
	}
	networkAIncompatible := &ovncnitypes.NetConf{
		Topology: types.LocalnetTopology,
		NetConf: cnitypes.NetConf{
			Name: "networkAPrimary",
			Type: "ovn-k8s-cni-overlay",
		},
		MTU: 1400,
	}
	networkASecondary := &ovncnitypes.NetConf{
		Topology: types.Layer2Topology,
		NetConf: cnitypes.NetConf{
			Name: "networkAPrimary",
			Type: "ovn-k8s-cni-overlay",
		},
		Subnets: "10.1.130.0/24",
		Role:    types.NetworkRoleSecondary,
		MTU:     1400,
	}

	networkBSecondary := &ovncnitypes.NetConf{
		Topology: types.LocalnetTopology,
		NetConf: cnitypes.NetConf{
			Name: "networkBSecondary",
			Type: "ovn-k8s-cni-overlay",
		},
		MTU: 1400,
	}

	networkDefault := &ovncnitypes.NetConf{
		NetConf: cnitypes.NetConf{
			Name: types.DefaultNetworkName,
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
			name: "NAD on default network is tracked with default controller",
			args: []args{
				{
					nad:     "test/nad_1",
					network: networkDefault,
				},
			},
			expected: []expected{
				{
					network: networkDefault,
					nads:    []string{"test/nad_1"},
				},
			},
		},
		{
			name: "NAD added",
			args: []args{
				{
					nad:     "test/nad_1",
					network: networkAPrimary,
				},
			},
			expected: []expected{
				{
					network: networkAPrimary,
					nads:    []string{"test/nad_1"},
				},
			},
		},
		{
			name: "NAD added then deleted",
			args: []args{
				{
					nad:     "test/nad_1",
					network: networkAPrimary,
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
					network: networkASecondary,
				},
				{
					nad:     "test/nad_2",
					network: networkASecondary,
				},
			},
			expected: []expected{
				{
					network: networkASecondary,
					nads:    []string{"test/nad_1", "test/nad_2"},
				},
			},
		},
		{
			name: "Two Primary NADs added for same namespace",
			args: []args{
				{
					nad:     "test/nad_1",
					network: networkAPrimary,
				},
				{
					nad:     "test/nad_2",
					network: networkAPrimary,
					wantErr: true,
				},
			},
			expected: []expected{
				{
					network: networkAPrimary,
					nads:    []string{"test/nad_1"},
				},
			},
		},
		{
			name: "two Primary NADs added then one deleted",
			args: []args{
				{
					nad:     "test/nad_1",
					network: networkAPrimary,
				},
				{
					nad:     "test2/nad_2",
					network: networkAPrimary,
				},
				{
					nad: "test/nad_1",
				},
			},
			expected: []expected{
				{
					network: networkAPrimary,
					nads:    []string{"test2/nad_2"},
				},
			},
		},
		{
			name: "two NADs added then one deleted",
			args: []args{
				{
					nad:     "test/nad_1",
					network: networkASecondary,
				},
				{
					nad:     "test/nad_2",
					network: networkASecondary,
				},
				{
					nad: "test/nad_1",
				},
			},
			expected: []expected{
				{
					network: networkASecondary,
					nads:    []string{"test/nad_2"},
				},
			},
		},
		{
			name: "two NADs added then deleted",
			args: []args{
				{
					nad:     "test/nad_1",
					network: networkASecondary,
				},
				{
					nad:     "test/nad_2",
					network: networkASecondary,
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
					network: networkAPrimary,
				},
				{
					nad:     "test/nad_1",
					network: networkBSecondary,
				},
			},
			expected: []expected{
				{
					network: networkBSecondary,
					nads:    []string{"test/nad_1"},
				},
			},
		},
		{
			name: "two NADs added then one updated to different network",
			args: []args{
				{
					nad:     "test/nad_1",
					network: networkASecondary,
				},
				{
					nad:     "test/nad_2",
					network: networkASecondary,
				},
				{
					nad:     "test/nad_1",
					network: networkBSecondary,
				},
			},
			expected: []expected{
				{
					network: networkASecondary,
					nads:    []string{"test/nad_2"},
				},
				{
					network: networkBSecondary,
					nads:    []string{"test/nad_1"},
				},
			},
		},
		{
			name: "two NADs added then one updated to same network",
			args: []args{
				{
					nad:     "test/nad_1",
					network: networkAPrimary,
				},
				{
					nad:     "test/nad_2",
					network: networkBSecondary,
				},
				{
					nad:     "test/nad_1",
					network: networkBSecondary,
				},
			},
			expected: []expected{
				{
					network: networkBSecondary,
					nads:    []string{"test/nad_1", "test/nad_2"},
				},
			},
		},
		{
			name: "NAD added then incompatible NAD added",
			args: []args{
				{
					nad:     "test/nad_1",
					network: networkAPrimary,
				},
				{
					nad:     "test/nad_2",
					network: networkAIncompatible,
					wantErr: true,
				},
			},
			expected: []expected{
				{
					network: networkAPrimary,
					nads:    []string{"test/nad_1"},
				},
			},
		},
		{
			name: "NAD added then updated to incompatible network",
			args: []args{
				{
					nad:     "test/nad_1",
					network: networkAPrimary,
				},
				{
					nad:     "test/nad_1",
					network: networkAIncompatible,
				},
			},
			expected: []expected{
				{
					network: networkAIncompatible,
					nads:    []string{"test/nad_1"},
				},
			},
		},
		{
			name: "two NADs added then one updated to incompatible network",
			args: []args{
				{
					nad:     "test/nad_1",
					network: networkASecondary,
				},
				{
					nad:     "test/nad_2",
					network: networkASecondary,
				},
				{
					nad:     "test/nad_1",
					network: networkAIncompatible,
					wantErr: true,
				},
			},
			expected: []expected{
				{
					network: networkASecondary,
					nads:    []string{"test/nad_2"},
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			err := config.PrepareTestConfig()
			g.Expect(err).ToNot(gomega.HaveOccurred())
			config.OVNKubernetesFeature.EnableNetworkSegmentation = true
			config.OVNKubernetesFeature.EnableMultiNetwork = true
			tcm := &testControllerManager{
				controllers: map[string]NetworkController{},
				defaultNetwork: &testNetworkController{
					ReconcilableNetInfo: &util.DefaultNetInfo{},
				},
			}
			nadController := &nadController{
				nads:              map[string]string{},
				primaryNADs:       map[string]string{},
				networkController: newNetworkController("", "", "", tcm, nil),
			}
			netController := nadController.networkController

			g.Expect(nadController.networkController.Start()).To(gomega.Succeed())
			defer nadController.networkController.Stop()

			for _, args := range tt.args {
				namespace, name, err := cache.SplitMetaNamespaceKey(args.nad)
				g.Expect(err).ToNot(gomega.HaveOccurred())

				var nad *nettypes.NetworkAttachmentDefinition
				if args.network != nil {
					args.network.NADName = args.nad
					nad, err = buildNAD(name, namespace, args.network)
					g.Expect(err).ToNot(gomega.HaveOccurred())
				}

				err = nadController.syncNAD(args.nad, nad)
				if args.wantErr {
					g.Expect(err).To(gomega.HaveOccurred())
				} else {
					g.Expect(err).NotTo(gomega.HaveOccurred())
				}
			}

			meetsExpectations := func(g gomega.Gomega) {
				// test that the manager has all the desired networks
				g.Expect(netController.getAllNetworks()).To(gomega.HaveLen(len(tt.expected)))

				var expectRunning []string
				for _, expected := range tt.expected {
					netInfo, err := util.NewNetInfo(expected.network)
					g.Expect(err).ToNot(gomega.HaveOccurred())

					name := netInfo.GetNetworkName()
					testNetworkKey := testNetworkKey(netInfo)
					func() {
						netController.Lock()
						defer netController.Unlock()
						tcm.Lock()
						defer tcm.Unlock()

						// test that the desired networks have the expected
						// config and NADs, including the default network which
						// could have had NAD/Advertisement changes as well
						g.Expect(netController.networks).To(gomega.HaveKey(name))
						g.Expect(util.AreNetworksCompatible(netController.networks[name], netInfo)).To(gomega.BeTrue(),
							fmt.Sprintf("matching network config for network %s", name))
						g.Expect(netController.networks[name].GetNADs()).To(gomega.ConsistOf(expected.nads),
							fmt.Sprintf("matching NADs for network %s", name))

						// test that the actual controllers have the expected config and NADs
						if !netInfo.IsDefault() {
							g.Expect(tcm.controllers).To(gomega.HaveKey(testNetworkKey))
							g.Expect(util.AreNetworksCompatible(tcm.controllers[testNetworkKey], netInfo)).To(gomega.BeTrue(),
								fmt.Sprintf("matching network config for network %s", name))
							g.Expect(tcm.controllers[testNetworkKey].GetNADs()).To(gomega.ConsistOf(expected.nads),
								fmt.Sprintf("matching NADs for network %s", name))
							expectRunning = append(expectRunning, testNetworkKey)
						}
					}()
					if netInfo.IsPrimaryNetwork() && !netInfo.IsDefault() {
						key := expected.nads[0]
						namespace, _, err := cache.SplitMetaNamespaceKey(key)
						g.Expect(err).ToNot(gomega.HaveOccurred())
						netInfoFound, err := nadController.GetActiveNetworkForNamespace(namespace)
						g.Expect(err).ToNot(gomega.HaveOccurred())
						g.Expect(util.AreNetworksCompatible(netInfoFound, netInfo)).To(gomega.BeTrue())
						g.Expect(netInfoFound.GetNADs()).To(gomega.ConsistOf(expected.nads))
					}
				}
				tcm.Lock()
				defer tcm.Unlock()
				expectStopped := sets.New(tcm.started...).Difference(sets.New(expectRunning...)).UnsortedList()
				// test that the controllers are started, stopped and cleaned up as expected
				g.Expect(tcm.started).To(gomega.ContainElements(expectRunning), "started network controllers")
				g.Expect(tcm.stopped).To(gomega.ConsistOf(expectStopped), "stopped network controllers")
				g.Expect(tcm.cleaned).To(gomega.ConsistOf(expectStopped), "cleaned up network controllers")
			}

			g.Eventually(meetsExpectations).Should(gomega.Succeed())
			g.Consistently(meetsExpectations).Should(gomega.Succeed())
		})
	}
}

func TestSyncAll(t *testing.T) {
	network_A := &ovncnitypes.NetConf{
		Topology: types.Layer3Topology,
		NetConf: cnitypes.NetConf{
			Name: "network_A",
			Type: "ovn-k8s-cni-overlay",
		},
		Role: types.NetworkRolePrimary,
		MTU:  1400,
	}
	network_A_Copy := *network_A
	network_B := &ovncnitypes.NetConf{
		Topology: types.LocalnetTopology,
		NetConf: cnitypes.NetConf{
			Name: "network_B",
			Type: "ovn-k8s-cni-overlay",
		},
		MTU: 1400,
	}
	type TestNAD struct {
		name    string
		netconf *ovncnitypes.NetConf
	}
	tests := []struct {
		name         string
		testNADs     []TestNAD
		syncAllError error
	}{
		{
			name: "multiple networks referenced by multiple nads",
			testNADs: []TestNAD{
				{
					name:    "test/nad1",
					netconf: network_A,
				},
				{
					name:    "test/nad2",
					netconf: network_B,
				},
				{
					name:    "test2/nad3",
					netconf: &network_A_Copy,
				},
			},
			syncAllError: ErrNetworkControllerTopologyNotManaged,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := gomega.NewWithT(t)

			err := config.PrepareTestConfig()
			g.Expect(err).ToNot(gomega.HaveOccurred())
			config.OVNKubernetesFeature.EnableNetworkSegmentation = true
			config.OVNKubernetesFeature.EnableMultiNetwork = true
			fakeClient := util.GetOVNClientset().GetOVNKubeControllerClientset()
			wf, err := factory.NewOVNKubeControllerWatchFactory(fakeClient)
			g.Expect(err).ToNot(gomega.HaveOccurred())

			tcm := &testControllerManager{
				controllers: map[string]NetworkController{},
			}
			if tt.syncAllError != nil {
				tcm.raiseErrorWhenCreatingController = tt.syncAllError
			}

			controller, err := NewForCluster(
				tcm,
				wf,
				nil,
			)
			g.Expect(err).ToNot(gomega.HaveOccurred())

			expectedNetworks := map[string]util.NetInfo{}
			expectedPrimaryNetworks := map[string]util.NetInfo{}
			for _, testNAD := range tt.testNADs {
				namespace, name, err := cache.SplitMetaNamespaceKey(testNAD.name)
				g.Expect(err).ToNot(gomega.HaveOccurred())
				testNAD.netconf.NADName = testNAD.name
				nad, err := buildNAD(name, namespace, testNAD.netconf)
				g.Expect(err).ToNot(gomega.HaveOccurred())
				_, err = fakeClient.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(namespace).Create(
					context.Background(),
					nad,
					v1.CreateOptions{},
				)
				g.Expect(err).ToNot(gomega.HaveOccurred())
				netInfo := expectedNetworks[testNAD.netconf.Name]
				if netInfo == nil {
					netInfo, err = util.NewNetInfo(testNAD.netconf)
					g.Expect(err).ToNot(gomega.HaveOccurred())
					expectedNetworks[testNAD.netconf.Name] = netInfo
					if netInfo.IsPrimaryNetwork() && !netInfo.IsDefault() {
						expectedPrimaryNetworks[netInfo.GetNetworkName()] = netInfo
					}
				}
			}

			err = wf.Start()
			g.Expect(err).ToNot(gomega.HaveOccurred())
			defer wf.Shutdown()

			err = controller.Start()
			g.Expect(err).ToNot(gomega.HaveOccurred())
			// sync has already happened, stop
			controller.Stop()

			actualNetworks := map[string]util.NetInfo{}
			for _, network := range tcm.valid {
				actualNetworks[network.GetNetworkName()] = network
			}

			g.Expect(actualNetworks).To(gomega.HaveLen(len(expectedNetworks)))
			for name, network := range expectedNetworks {
				g.Expect(actualNetworks).To(gomega.HaveKey(name))
				g.Expect(util.AreNetworksCompatible(actualNetworks[name], network)).To(gomega.BeTrue())
			}

			actualPrimaryNetwork, err := controller.Interface().GetActiveNetworkForNamespace("test")
			g.Expect(err).ToNot(gomega.HaveOccurred())
			g.Expect(expectedPrimaryNetworks).To(gomega.HaveKey(actualPrimaryNetwork.GetNetworkName()))
			expectedPrimaryNetwork := expectedPrimaryNetworks[actualPrimaryNetwork.GetNetworkName()]
			g.Expect(util.AreNetworksCompatible(expectedPrimaryNetwork, actualPrimaryNetwork)).To(gomega.BeTrue())
		})
	}
}

func buildNAD(name, namespace string, network *ovncnitypes.NetConf) (*nettypes.NetworkAttachmentDefinition, error) {
	config, err := json.Marshal(network)
	if err != nil {
		return nil, err
	}
	nad := &nettypes.NetworkAttachmentDefinition{
		ObjectMeta: v1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: nettypes.NetworkAttachmentDefinitionSpec{
			Config: string(config),
		},
	}
	return nad, nil
}
