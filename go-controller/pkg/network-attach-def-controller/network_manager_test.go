package networkAttachDefController

import (
	"context"
	"testing"

	"github.com/onsi/gomega"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"

	ovncnitypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	ratypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

func TestSetVRFs(t *testing.T) {
	testZoneName := "testZone"
	testNodeName := "testNode"
	testNodeOnZoneName := "testNodeOnZone"
	testNADName := "test/NAD"
	testRAName := "testRA"
	testVRFName := "testVRF"

	defaultNetwork := &ovncnitypes.NetConf{
		NetConf: cnitypes.NetConf{
			Name: types.DefaultNetworkName,
			Type: "ovn-k8s-cni-overlay",
		},
		MTU: 1400,
	}

	podNetworkRA := &ratypes.RouteAdvertisements{
		ObjectMeta: v1.ObjectMeta{
			Name: testRAName,
		},
		Spec: ratypes.RouteAdvertisementsSpec{
			TargetVRF: testVRFName,
			Advertisements: ratypes.Advertisements{
				PodNetwork: true,
			},
		},
	}
	nonPodNetworkRA := &ratypes.RouteAdvertisements{
		ObjectMeta: v1.ObjectMeta{
			Name: testRAName,
		},
		Spec: ratypes.RouteAdvertisementsSpec{
			TargetVRF: testVRFName,
		},
	}

	testNode := corev1.Node{
		ObjectMeta: v1.ObjectMeta{
			Name: testNodeName,
		},
	}
	testNodeOnZone := corev1.Node{
		ObjectMeta: v1.ObjectMeta{
			Name: testNodeOnZoneName,
			Annotations: map[string]string{
				util.OvnNodeZoneName: testZoneName,
			},
		},
	}
	otherNode := corev1.Node{
		ObjectMeta: v1.ObjectMeta{
			Name: "otherNode",
		},
	}

	tests := []struct {
		name     string
		network  *ovncnitypes.NetConf
		ra       *ratypes.RouteAdvertisements
		node     corev1.Node
		expected map[string][]string
	}{
		{
			name:    "reconciles VRF for selected node of default node network controller",
			network: defaultNetwork,
			ra:      podNetworkRA,
			node:    testNode,
			expected: map[string][]string{
				testNodeName: {testVRFName},
			},
		},
		{
			name:    "reconciles VRF for selected node in same zone as default OVN network controller",
			network: defaultNetwork,
			ra:      podNetworkRA,
			node:    testNodeOnZone,
			expected: map[string][]string{
				testNodeOnZoneName: {testVRFName},
			},
		},
		{
			name:    "ignores a route advertisement that is not for the pod network",
			network: defaultNetwork,
			ra:      nonPodNetworkRA,
			node:    testNode,
		},
		{
			name:    "ignores a route advertisement that is not for applicable node",
			network: defaultNetwork,
			ra:      podNetworkRA,
			node:    otherNode,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := gomega.NewWithT(t)

			config.OVNKubernetesFeature.EnableMultiNetwork = true
			config.OVNKubernetesFeature.EnableRouteAdvertisements = true
			fakeClient := util.GetOVNClientset().GetOVNKubeControllerClientset()
			wf, err := factory.NewOVNKubeControllerWatchFactory(fakeClient)
			g.Expect(err).ToNot(gomega.HaveOccurred())

			tncm := &testNetworkControllerManager{
				controllers: map[string]NetworkController{},
				defaultNetwork: &testNetworkController{
					NetInfo: &util.DefaultNetInfo{},
				},
			}
			nm := newNetworkManager("", testZoneName, testNodeName, tncm, wf)

			namespace, name, err := cache.SplitMetaNamespaceKey(testNADName)
			g.Expect(err).ToNot(gomega.HaveOccurred())
			nad, err := buildNAD(name, namespace, tt.network)
			g.Expect(err).ToNot(gomega.HaveOccurred())
			nad.Annotations = map[string]string{
				util.OvnRouteAdvertisements: "[\"" + tt.ra.Name + "\"]",
			}

			_, err = fakeClient.KubeClient.CoreV1().Nodes().Create(context.Background(), &tt.node, v1.CreateOptions{})
			g.Expect(err).ToNot(gomega.HaveOccurred())
			_, err = fakeClient.RouteAdvertisementsClient.K8sV1().RouteAdvertisements().Create(context.Background(), tt.ra, v1.CreateOptions{})
			g.Expect(err).ToNot(gomega.HaveOccurred())
			_, err = fakeClient.NetworkAttchDefClient.K8sCniCncfIoV1().NetworkAttachmentDefinitions(namespace).Create(context.Background(), nad, v1.CreateOptions{})
			g.Expect(err).ToNot(gomega.HaveOccurred())

			err = wf.Start()
			g.Expect(err).ToNot(gomega.HaveOccurred())
			defer wf.Shutdown()
			g.Expect(nm.Start()).To(gomega.Succeed())
			defer nm.Stop()

			netInfo, err := util.NewNetInfo(tt.network)
			g.Expect(err).ToNot(gomega.HaveOccurred())
			netInfo.AddNADs(testNADName)

			nm.EnsureNetwork(netInfo)

			meetsExpectations := func(g gomega.Gomega) {
				var reconcilable util.ReconcilableNetInfo
				switch tt.network.Name {
				case types.DefaultNetworkName:
					reconcilable = tncm.GetDefaultNetworkController().(util.ReconcilableNetInfo)
				default:
					reconcilable = tncm.controllers[testNetworkKey(netInfo)]
				}

				g.Expect(reconcilable).ToNot(gomega.BeNil())
				g.Expect(reconcilable.GetVRFs()).To(gomega.Equal(tt.expected))
			}

			g.Eventually(meetsExpectations)
			g.Consistently(meetsExpectations)
		})
	}
}
