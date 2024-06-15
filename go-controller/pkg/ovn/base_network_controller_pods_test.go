package ovn

import (
	"fmt"
	"net"
	"testing"
	"time"

	nadapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	"github.com/onsi/gomega"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	lsm "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/logical_switch_manager"
	ovntest "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/testing"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/kubernetes/fake"
)

func TestBaseNetworkController_trackPodsReleasedBeforeStartup(t *testing.T) {
	tests := []struct {
		name           string
		podAnnotations map[*corev1.Pod]map[string]*util.PodAnnotation
		expected       map[string]sets.Set[string]
	}{
		{
			name: "a scheduled/running annotated pod should not be considered released",
			podAnnotations: map[*corev1.Pod]map[string]*util.PodAnnotation{
				{
					ObjectMeta: metav1.ObjectMeta{
						UID: "running",
					},
				}: {
					"default": {
						IPs: []*net.IPNet{ovntest.MustParseIPNet("192.168.0.1/24")},
					},
				},
			},
			expected: map[string]sets.Set[string]{},
		},
		{
			name: "a completed annotated pod should not be considered released",
			podAnnotations: map[*corev1.Pod]map[string]*util.PodAnnotation{
				{
					ObjectMeta: metav1.ObjectMeta{
						UID: "running",
					},
					Status: corev1.PodStatus{
						Phase: corev1.PodSucceeded,
					},
				}: {
					"default": {
						IPs: []*net.IPNet{ovntest.MustParseIPNet("192.168.0.1/24")},
					},
				},
			},
			expected: map[string]sets.Set[string]{},
		},
		{
			// consider dual-stack IPs individually but only track at the pod level based
			// on a couple of assumptions:
			// - while the same pair of IPs released for a pod will most likely be
			//   assigned to a different pod, assume that one of those IPs might be
			//   assigned to a pod and the other IP to a different pod. This is easy to
			//   handle so better take a safe approach
			// - assume that there is no error path leading to one of the IPs of the
			//   pair to be released while the other is not. This is based on the fact
			//   that both IPs are released in block.
			name: "a completed pod sharing at least one IP with a running Pod should be considered released",
			podAnnotations: map[*corev1.Pod]map[string]*util.PodAnnotation{
				{
					ObjectMeta: metav1.ObjectMeta{
						UID: "completed",
					},
					Status: corev1.PodStatus{
						Phase: corev1.PodSucceeded,
					},
				}: {
					"default": {
						IPs: []*net.IPNet{
							ovntest.MustParseIPNet("192.168.0.1/24"),
							ovntest.MustParseIPNet("fd11::1/64"),
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						UID: "running",
					},
				}: {
					"default": {
						IPs: []*net.IPNet{
							ovntest.MustParseIPNet("192.168.0.2/24"),
							ovntest.MustParseIPNet("fd11::1/64"),
						},
					},
				},
			},
			expected: map[string]sets.Set[string]{
				"default": sets.New("completed"),
			},
		},
		{
			name: "only the last completed pod of multiple completed pods sharing at least one IP should not be considered released",
			podAnnotations: map[*corev1.Pod]map[string]*util.PodAnnotation{
				{
					ObjectMeta: metav1.ObjectMeta{
						UID: "completed-third",
					},
					Status: corev1.PodStatus{
						Phase: corev1.PodSucceeded,
						Conditions: []corev1.PodCondition{
							{
								Type: corev1.PodInitialized,
								LastTransitionTime: metav1.Time{
									Time: time.Time{}.Add(time.Second * 2),
								},
							},
						},
					},
				}: {
					"default": {
						IPs: []*net.IPNet{
							ovntest.MustParseIPNet("192.168.0.1/24"),
							ovntest.MustParseIPNet("fd11::1/64"),
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						UID: "completed-first",
					},
					Status: corev1.PodStatus{
						Phase: corev1.PodSucceeded,
						Conditions: []corev1.PodCondition{
							{
								Type: corev1.PodInitialized,
								LastTransitionTime: metav1.Time{
									Time: time.Time{},
								},
							},
						},
					},
				}: {
					"default": {
						IPs: []*net.IPNet{
							ovntest.MustParseIPNet("192.168.0.1/24"),
							ovntest.MustParseIPNet("fd11::2/64"),
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						UID: "completed-second",
					},
					Status: corev1.PodStatus{
						Phase: corev1.PodSucceeded,
						Conditions: []corev1.PodCondition{
							{
								Type: corev1.PodInitialized,
								LastTransitionTime: metav1.Time{
									Time: time.Time{}.Add(time.Second),
								},
							},
						},
					},
				}: {
					"default": {
						IPs: []*net.IPNet{
							ovntest.MustParseIPNet("192.168.0.2/24"),
							ovntest.MustParseIPNet("fd11::1/64"),
						},
					},
				},
			},
			expected: map[string]sets.Set[string]{
				"default": sets.New("completed-first", "completed-second"),
			},
		},
		{
			name: "a completed pod sharing at least one IP from nad1 with a running Pod on nad2 should be considered released on nad1",
			podAnnotations: map[*corev1.Pod]map[string]*util.PodAnnotation{
				{
					ObjectMeta: metav1.ObjectMeta{
						UID: "completed",
					},
					Status: corev1.PodStatus{
						Phase: corev1.PodSucceeded,
					},
				}: {
					"nad1": {
						IPs: []*net.IPNet{
							ovntest.MustParseIPNet("192.168.0.1/24"),
							ovntest.MustParseIPNet("fd11::1/64"),
						},
					},
					"nad2": {
						IPs: []*net.IPNet{
							ovntest.MustParseIPNet("192.168.0.2/24"),
							ovntest.MustParseIPNet("fd11::2/64"),
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						UID: "running",
					},
				}: {
					"nad1": {
						IPs: []*net.IPNet{
							ovntest.MustParseIPNet("192.168.0.3/24"),
							ovntest.MustParseIPNet("fd11::3/64"),
						},
					},
					"nad2": {
						IPs: []*net.IPNet{
							ovntest.MustParseIPNet("192.168.0.4/24"),
							ovntest.MustParseIPNet("fd11::1/64"),
						},
					},
				},
			},
			expected: map[string]sets.Set[string]{
				"nad1": sets.New("completed"),
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			bnc := &BaseNetworkController{}

			bnc.trackPodsReleasedBeforeStartup(tt.podAnnotations)

			g.Expect(bnc.releasedPodsBeforeStartup).To(gomega.Equal(tt.expected))
		})
	}
}

func TestBaseNetworkController_DefaultNetwork_PodAnnotationPrimaryField(t *testing.T) {
	config.OVNKubernetesFeature.EnableMultiNetwork = true
	tests := []struct {
		name               string
		namespaceAnnotaton map[string]string
		expectedPrimaryVal bool
	}{
		{
			name:               "no annotation on namespace",
			namespaceAnnotaton: map[string]string{},
			expectedPrimaryVal: true,
		},
		{
			name: "default network annotation on namespace",
			namespaceAnnotaton: map[string]string{
				"k8s.ovn.org/active-network": "default",
			},
			expectedPrimaryVal: true,
		},
		{
			name: "secondary l3-network annotation on namespace",
			namespaceAnnotaton: map[string]string{
				"k8s.ovn.org/active-network": "l3-network",
			},
			expectedPrimaryVal: false,
		},
	}
	for i := 1; i <= 2; i++ {
		if i%2 == 0 {
			config.OVNKubernetesFeature.EnableNetworkSegmentation = true
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				g := gomega.NewWithT(t)
				var err error
				netInfo := &util.DefaultNetInfo{}
				bnc := &BaseNetworkController{
					NetInfo:   netInfo,
					lsManager: lsm.NewLogicalSwitchManager(),
				}
				bnc.lsManager.AddOrUpdateSwitch("node1", []*net.IPNet{ovntest.MustParseIPNet("10.128.1.0/24")})
				namespace := &corev1.Namespace{
					ObjectMeta: metav1.ObjectMeta{
						Name:        "namespace",
						Annotations: tt.namespaceAnnotaton,
					},
				}
				pod := &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "pod",
						Namespace: namespace.Name,
					},
					Spec: corev1.PodSpec{
						NodeName: "node1",
					},
				}
				kubeClient := fake.NewSimpleClientset(namespace, pod)
				bnc.kube = &kube.KubeOVN{
					Kube: kube.Kube{KClient: kubeClient},
				}
				bnc.watchFactory, err = factory.NewMasterWatchFactory(&util.OVNMasterClientset{KubeClient: kubeClient})
				g.Expect(err).To(gomega.BeNil())
				g.Expect(bnc.watchFactory.Start()).To(gomega.Succeed())
				nadName := types.DefaultNetworkName
				podDesc := fmt.Sprintf("%s/%s/%s", nadName, pod.Namespace, pod.Name)
				portName := bnc.GetLogicalPortName(pod, nadName)
				existingLSP := &nbdb.LogicalSwitchPort{
					UUID:      "lsp1",
					Addresses: []string{"0a:58:0a:80:01:03 10.128.1.3"},
					ExternalIDs: map[string]string{
						"pod":       "true",
						"namespace": pod.Namespace,
					},
					Name: portName,
					Options: map[string]string{
						"iface-id-ver":      "myPod",
						"requested-chassis": "node1",
					},
					PortSecurity: []string{"0a:58:0a:80:01:03 10.128.1.3"},
				}
				network := &nadapi.NetworkSelectionElement{
					Name: "boo",
				}
				podAnnotation, _, err := bnc.allocatePodAnnotation(pod, existingLSP, podDesc, nadName, network)
				g.Expect(err).To(gomega.BeNil())
				if config.OVNKubernetesFeature.EnableNetworkSegmentation {
					g.Expect(podAnnotation.Primary).To(gomega.Equal(tt.expectedPrimaryVal))
				} else {
					// if feature is not enabled then default network is always true
					g.Expect(podAnnotation.Primary).To(gomega.BeTrue())
				}
			})
		}
	}
}
