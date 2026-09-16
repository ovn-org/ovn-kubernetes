// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovn

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	"github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"

	ovncnitypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/cni/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	logicalswitchmanager "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/ovn/logical_switch_manager"
	ovntest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing"
	ovntypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

func TestBaseNetworkController_GetLocalNode(t *testing.T) {
	g := gomega.NewWithT(t)
	clientSet := util.GetOVNClientset(&corev1.NodeList{Items: []corev1.Node{{
		ObjectMeta: metav1.ObjectMeta{Name: "node1"},
	}}}).GetOVNKubeControllerClientset()
	watchFactory, err := factory.NewOVNKubeControllerWatchFactory(clientSet, "test-node")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(watchFactory.Start()).To(gomega.Succeed())
	t.Cleanup(watchFactory.Shutdown)

	bnc := &BaseNetworkController{CommonNetworkControllerInfo: CommonNetworkControllerInfo{
		watchFactory: watchFactory,
		nodeName:     "node1",
	}}
	node, err := bnc.GetLocalNode()
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(node.Name).To(gomega.Equal("node1"))

	bnc.nodeName = "missing-node"
	_, err = bnc.GetLocalNode()
	g.Expect(apierrors.IsNotFound(err)).To(gomega.BeTrue())
}

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

func TestBaseNetworkController_shouldReleaseDeletedPod(t *testing.T) {
	tests := []struct {
		name string // description of this test case
		// Named input parameters for target function.
		pod        *corev1.Pod
		switchName string
		nad        string
		podIfAddrs []*net.IPNet
		want       bool
		wantErr    bool
	}{
		{
			name: "should release a running pod",
			pod:  &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodRunning}},
			want: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var bnc BaseNetworkController
			bnc.ReconcilableNetInfo = &util.DefaultNetInfo{}
			got, gotErr := bnc.shouldReleaseDeletedPod(tt.pod, tt.switchName, tt.nad, tt.podIfAddrs)
			if gotErr != nil {
				if !tt.wantErr {
					t.Errorf("shouldReleaseDeletedPod() failed: %v", gotErr)
				}
				return
			}
			if tt.wantErr {
				t.Fatal("shouldReleaseDeletedPod() succeeded unexpectedly")
			}
			if got != tt.want {
				t.Errorf("shouldReleaseDeletedPod() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPodDeleteRetryPreservesReallocatedIPs(t *testing.T) {
	for _, tc := range []struct{ sameName, annotationVisible bool }{
		{false, false}, {true, false}, {false, true}, {true, true},
	} {
		t.Run(fmt.Sprintf("same-name=%t/annotation-visible=%t", tc.sameName, tc.annotationVisible), func(t *testing.T) {
			g := gomega.NewWithT(t)
			pod := ovntest.NewPod("namespace", "old-pod", "node1", "10.128.0.3")
			pod.UID = "old-uid"
			pod.Status.Phase = corev1.PodRunning
			ips := ovntest.MustParseIPNets("10.128.0.3/24", "fd00::3/64")
			var err error
			pod.Annotations, err = util.MarshalPodAnnotation(pod.Annotations, &util.PodAnnotation{
				IPs: ips, MAC: ovntest.MustParseMAC("0a:58:0a:80:00:03"),
			}, ovntypes.DefaultNetworkName)
			g.Expect(err).NotTo(gomega.HaveOccurred())
			clients := util.GetOVNClientset(pod).GetOVNKubeControllerClientset()
			wf, err := factory.NewOVNKubeControllerWatchFactory(clients, pod.Spec.NodeName)
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Expect(wf.Start()).To(gomega.Succeed())
			t.Cleanup(wf.Shutdown)
			bnc := &BaseNetworkController{
				CommonNetworkControllerInfo: CommonNetworkControllerInfo{watchFactory: wf},
				ReconcilableNetInfo:         &util.DefaultNetInfo{},
				lsManager:                   logicalswitchmanager.NewLogicalSwitchManager(),
			}
			g.Expect(bnc.lsManager.AddOrUpdateSwitch("node1", ovntest.MustParseIPNets("10.128.0.0/24", "fd00::/64"), nil)).To(gomega.Succeed())
			g.Expect(bnc.lsManager.AllocateIPs("node1", ips)).To(gomega.Succeed())
			// The deleting UID may still be visible during the first cleanup pass.
			release, err := bnc.shouldReleaseDeletedPod(pod, "node1", ovntypes.DefaultNetworkName, ips)
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Expect(release).To(gomega.BeTrue())
			portInfo := &lpInfo{logicalSwitch: "node1", ips: ips}
			g.Expect(bnc.releasePodIPsOnce(pod, ovntypes.DefaultNetworkName, portInfo)).To(gomega.Succeed())

			// A later cleanup failure retries the running tombstone after another
			// pod has acquired its addresses, with or without reusing its name.
			g.Expect(clients.KubeClient.CoreV1().Pods(pod.Namespace).Delete(context.Background(), pod.Name, metav1.DeleteOptions{})).To(gomega.Succeed())
			owner := pod.DeepCopy()
			owner.UID = "new-uid"
			if !tc.sameName {
				owner.Name = "new-pod"
			}
			if !tc.annotationVisible {
				owner.Annotations = nil
				owner.Status.PodIP = ""
				owner.Status.PodIPs = nil
			}
			_, err = clients.KubeClient.CoreV1().Pods(owner.Namespace).Create(context.Background(), owner, metav1.CreateOptions{})
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Eventually(func() bool {
				current, err := wf.GetPod(owner.Namespace, owner.Name)
				return err == nil && current.UID == owner.UID
			}).Should(gomega.BeTrue())
			g.Expect(bnc.lsManager.AllocateIPs("node1", ips)).To(gomega.Succeed())
			// The allocation is already reserved even when the informer still has
			// the pre-allocation pod, with neither an annotation nor status IPs.
			release, err = bnc.shouldReleaseDeletedPod(pod, "node1", ovntypes.DefaultNetworkName, ips)
			g.Expect(err).NotTo(gomega.HaveOccurred())
			g.Expect(release).To(gomega.BeFalse(), "delete retry must preserve the new owner's allocation")
			// Also guard the release itself if a caller retained an earlier true
			// decision, before another cleanup pass released these addresses.
			g.Expect(bnc.releasePodIPsOnce(pod, ovntypes.DefaultNetworkName, portInfo)).To(gomega.Succeed())
			g.Expect(bnc.lsManager.AllocateIPs("node1", ips)).NotTo(gomega.Succeed(), "the new allocation must remain reserved")
		})
	}
}

func TestPodIPReleaseProgress(t *testing.T) {
	g := gomega.NewWithT(t)
	bnc := &BaseNetworkController{
		ReconcilableNetInfo: &util.DefaultNetInfo{},
		lsManager:           logicalswitchmanager.NewLogicalSwitchManager(),
	}
	pod := ovntest.NewPod("namespace", "pod", "node1", "10.128.0.3")
	pod.UID = "old-uid"
	ips := ovntest.MustParseIPNets("10.128.0.3/24", "fd00::3/64")
	g.Expect(bnc.lsManager.AddOrUpdateSwitch("node1", ovntest.MustParseIPNets("10.128.0.0/24", "fd00::/64"), nil)).To(gomega.Succeed())
	g.Expect(bnc.lsManager.AllocateIPs("node1", ips)).To(gomega.Succeed())
	// No watch factory is needed on the ordinary running-pod fast path.
	release, err := bnc.shouldReleaseDeletedPod(pod, "node1", "nad-a", ips)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(release).To(gomega.BeTrue())
	g.Expect(bnc.releasePodIPsOnce(pod, "nad-a", &lpInfo{logicalSwitch: "node1", ips: ips})).To(gomega.Succeed())
	g.Expect(bnc.wasPodIPReleased(pod, "nad-a")).To(gomega.BeTrue())
	g.Expect(bnc.wasPodIPReleased(pod, "nad-b")).To(gomega.BeFalse())
	replacement := pod.DeepCopy()
	replacement.UID = "new-uid"
	g.Expect(bnc.wasPodIPReleased(replacement, "nad-a")).To(gomega.BeFalse())

	bnc.forgetPodIPReleases(pod, "nad-b")
	g.Expect(bnc.wasPodIPReleased(pod, "nad-a")).To(gomega.BeTrue())
	bnc.forgetPodIPReleases(pod, "nad-a")
	g.Expect(bnc.podIPReleases).To(gomega.BeEmpty(), "reattachment starts a fresh release lifecycle")
	g.Expect(bnc.lsManager.AllocateIPs("node1", ips)).To(gomega.Succeed())
	g.Expect(bnc.releasePodIPsOnce(pod, "nad-a", &lpInfo{logicalSwitch: "node1", ips: ips})).To(gomega.Succeed())
	bnc.forgetPodIPReleases(pod)
	g.Expect(bnc.podIPReleases).To(gomega.BeEmpty(), "successful reconciliation retires retry progress")
}

// TestBaseNetworkController_allocatesPodAnnotation pins who writes the
// pod-networks annotation per topology/IPAM combination. The DHCP row is the
// single-writer contract: the CNI picks the MAC and reports the DHCP-learned
// IPs, so this controller must never allocate (nor overwrite) the entry.
func TestBaseNetworkController_allocatesPodAnnotation(t *testing.T) {
	tests := []struct {
		name     string
		netconf  *ovncnitypes.NetConf
		expected bool
	}{
		{
			name: "localnet with subnets (OVN-K IPAM): cluster manager allocates",
			netconf: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: "localnet-ipam"},
				Topology: ovntypes.LocalnetTopology,
				NADName:  "default/localnet-ipam",
				Subnets:  "10.128.0.0/16",
			},
			expected: false,
		},
		{
			name: "localnet without subnets (ipam-less): this controller allocates the MAC-only entry",
			netconf: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: "localnet-ipamless"},
				Topology: ovntypes.LocalnetTopology,
				NADName:  "default/localnet-ipamless",
			},
			expected: true,
		},
		{
			name: "localnet with DHCP IPAM: the CNI is the single writer",
			netconf: &ovncnitypes.NetConf{
				NetConf: cnitypes.NetConf{
					Name: "localnet-dhcp",
					IPAM: cnitypes.IPAM{Type: ovntypes.IPAMTypeDHCP},
				},
				Topology: ovntypes.LocalnetTopology,
				NADName:  "default/localnet-dhcp",
			},
			expected: false,
		},
		{
			name: "layer2: cluster manager allocates",
			netconf: &ovncnitypes.NetConf{
				NetConf:  cnitypes.NetConf{Name: "l2"},
				Topology: ovntypes.Layer2Topology,
				NADName:  "default/l2",
				Subnets:  "10.129.0.0/16",
			},
			expected: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := gomega.NewWithT(t)
			netInfo, err := util.NewNetInfo(tt.netconf)
			g.Expect(err).ToNot(gomega.HaveOccurred())
			bnc := &BaseNetworkController{ReconcilableNetInfo: util.NewReconcilableNetInfo(netInfo)}

			g.Expect(bnc.allocatesPodAnnotation()).To(gomega.Equal(tt.expected))
		})
	}
}
