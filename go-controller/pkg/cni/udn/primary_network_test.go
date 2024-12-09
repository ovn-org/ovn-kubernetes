package udn

import (
	"testing"

	nadapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	v1nadmocks "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/mocks/github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/listers/k8s.cni.cncf.io/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing/networkmanager"
	types "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

func TestWaitForPrimaryAnnotationFn(t *testing.T) {

	config.OVNKubernetesFeature.EnableMultiNetwork = true
	config.OVNKubernetesFeature.EnableNetworkSegmentation = true

	tests := []struct {
		description           string
		namespace             string
		nadName               string
		annotationFromFn      *util.PodAnnotation
		isReadyFromFn         bool
		annotations           map[string]string
		nads                  []*nadapi.NetworkAttachmentDefinition
		getActiveNetworkError error
		expectedIsReady       bool
		expectedFound         bool
		expectedAnnotation    *util.PodAnnotation
		expectedNADName       string
		expectedNetworkName   string
		expectedMTU           int
	}{
		{
			description: "With non default nad should be ready",
			nadName:     "red",
			annotationFromFn: &util.PodAnnotation{
				Role: types.NetworkRoleSecondary,
			},
			isReadyFromFn:   true,
			expectedIsReady: true,
			expectedAnnotation: &util.PodAnnotation{
				Role: types.NetworkRoleSecondary,
			},
		},
		{
			description:        "With no ovn annotation should force return not ready",
			nadName:            types.DefaultNetworkName,
			annotationFromFn:   nil,
			isReadyFromFn:      true,
			expectedAnnotation: nil,
			expectedIsReady:    false,
		},
		{
			description: "With primary default should be ready",
			nadName:     types.DefaultNetworkName,
			annotationFromFn: &util.PodAnnotation{
				Role: types.NetworkRolePrimary,
			},
			isReadyFromFn: true,
			expectedAnnotation: &util.PodAnnotation{
				Role: types.NetworkRolePrimary,
			},
			expectedIsReady: true,
		},
		{
			description: "With default network without role should be ready",
			nadName:     types.DefaultNetworkName,
			annotationFromFn: &util.PodAnnotation{
				Role: "",
			},
			isReadyFromFn: true,
			expectedAnnotation: &util.PodAnnotation{
				Role: types.NetworkRolePrimary,
			},
			expectedIsReady: true,
		},

		{
			description: "With missing primary annotation and active network should return not ready",
			nadName:     types.DefaultNetworkName,
			annotationFromFn: &util.PodAnnotation{
				Role: types.NetworkRoleInfrastructure,
			},
			isReadyFromFn:   true,
			expectedIsReady: false,
		},
		{
			description: "With primary network annotation and missing active network should return not ready",
			namespace:   "ns1",
			nadName:     types.DefaultNetworkName,
			annotationFromFn: &util.PodAnnotation{
				Role: types.NetworkRoleInfrastructure,
			},
			isReadyFromFn: true,
			annotations: map[string]string{
				"k8s.ovn.org/pod-networks": `{"ns1/nad1": {
					"role": "primary",
			        "mac_address": "0a:58:fd:98:00:01"
				}}`,
			},
			expectedIsReady: false,
		},
		{
			description: "With missing primary network annotation and active network should return not ready",
			nadName:     types.DefaultNetworkName,
			annotationFromFn: &util.PodAnnotation{
				Role: types.NetworkRoleInfrastructure,
			},
			isReadyFromFn: true,
			nads: []*nadapi.NetworkAttachmentDefinition{
				ovntest.GenerateNAD("blue", "nad1", "ns1",
					types.Layer2Topology, "10.100.200.0/24", types.NetworkRolePrimary),
			},
			expectedIsReady: false,
		},
		{
			description: "With primary network annotation and active network should return ready",
			namespace:   "ns1",
			nadName:     types.DefaultNetworkName,
			annotationFromFn: &util.PodAnnotation{
				Role: types.NetworkRoleInfrastructure,
			},
			isReadyFromFn: true,
			annotations: map[string]string{
				"k8s.ovn.org/pod-networks": `{"ns1/nad1": {
					"role": "primary",
			        "mac_address": "0a:58:fd:98:00:01"
				}}`,
			},
			nads: []*nadapi.NetworkAttachmentDefinition{
				ovntest.GenerateNAD("blue", "nad1", "ns1",
					types.Layer2Topology, "10.100.200.0/24", types.NetworkRolePrimary),
			},
			expectedIsReady:     true,
			expectedFound:       true,
			expectedNetworkName: "blue",
			expectedNADName:     "ns1/nad1",
			expectedAnnotation: &util.PodAnnotation{
				Role: types.NetworkRoleInfrastructure,
			},
			expectedMTU: 1300,
		},
		{
			description: "With primary network annotation and active network and no MTU should return ready with default MTU",
			namespace:   "ns1",
			nadName:     types.DefaultNetworkName,
			annotationFromFn: &util.PodAnnotation{
				Role: types.NetworkRoleInfrastructure,
			},
			isReadyFromFn: true,
			annotations: map[string]string{
				"k8s.ovn.org/pod-networks": `{"ns1/nad1": {
					"role": "primary",
			        "mac_address": "0a:58:fd:98:00:01"
				}}`,
			},
			nads: []*nadapi.NetworkAttachmentDefinition{
				ovntest.GenerateNADWithoutMTU("blue", "nad1", "ns1",
					types.Layer2Topology, "10.100.200.0/24", types.NetworkRolePrimary),
			},
			expectedIsReady:     true,
			expectedFound:       true,
			expectedNetworkName: "blue",
			expectedNADName:     "ns1/nad1",
			expectedAnnotation: &util.PodAnnotation{
				Role: types.NetworkRoleInfrastructure,
			},
			expectedMTU: 1400,
		},
	}
	for _, tt := range tests {
		t.Run(tt.description, func(t *testing.T) {
			g := NewWithT(t)
			// needs to be set so the primary user defined networks can use ipfamilies supported by the underlying cluster
			config.IPv4Mode = true
			config.IPv6Mode = true
			nadLister := v1nadmocks.NetworkAttachmentDefinitionLister{}
			nadNamespaceLister := v1nadmocks.NetworkAttachmentDefinitionNamespaceLister{}
			nadLister.On("NetworkAttachmentDefinitions", tt.namespace).Return(&nadNamespaceLister)
			nadNamespaceLister.On("List", labels.Everything()).Return(tt.nads, nil)
			waitCond := func(map[string]string, string) (*util.PodAnnotation, bool) {
				return tt.annotationFromFn, tt.isReadyFromFn
			}

			fakeNetworkManager := &networkmanager.FakeNetworkManager{
				PrimaryNetworks: map[string]util.NetInfo{},
			}
			for _, nad := range tt.nads {
				nadNetwork, _ := util.ParseNADInfo(nad)
				mutableNetInfo := util.NewMutableNetInfo(nadNetwork)
				mutableNetInfo.SetNADs(util.GetNADName(nad.Namespace, nad.Name))
				nadNetwork = mutableNetInfo
				if nadNetwork.IsPrimaryNetwork() {
					if _, loaded := fakeNetworkManager.PrimaryNetworks[nad.Namespace]; !loaded {
						fakeNetworkManager.PrimaryNetworks[nad.Namespace] = nadNetwork
					}
				}
			}

			userDefinedPrimaryNetwork := NewPrimaryNetwork(fakeNetworkManager)
			obtainedAnnotation, obtainedIsReady := userDefinedPrimaryNetwork.WaitForPrimaryAnnotationFn(tt.namespace, waitCond)(tt.annotations, tt.nadName)
			obtainedFound := userDefinedPrimaryNetwork.Found()
			obtainedNetworkName := userDefinedPrimaryNetwork.NetworkName()
			obtainedNADName := userDefinedPrimaryNetwork.NADName()
			obtainedMTU := userDefinedPrimaryNetwork.MTU()
			g.Expect(obtainedIsReady).To(Equal(tt.expectedIsReady), "should return expected readiness")
			g.Expect(obtainedFound).To(Equal(tt.expectedFound), "should return expected found flag")
			g.Expect(obtainedNetworkName).To(Equal(tt.expectedNetworkName), "should return expected network name")
			g.Expect(obtainedNADName).To(Equal(tt.expectedNADName), "should return expected nad name")
			g.Expect(obtainedAnnotation).To(Equal(tt.expectedAnnotation), "should return expected ovn pod annotation")
			g.Expect(obtainedMTU).To(Equal(tt.expectedMTU), "should return expected MTU")
		})
	}
}
