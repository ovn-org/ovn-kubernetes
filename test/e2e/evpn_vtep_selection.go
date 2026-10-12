// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/kubernetes/test/e2e/framework"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"

	vtepv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/vtep/v1"
	vtepclientset "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/vtep/v1/apis/clientset/versioned"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/feature"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider"
	infraapi "github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/api"
)

// On-prem clusters keep the API/Ingress virtual IPs inside the node subnet,
// where an unmanaged VTEP spanning that subnet also looks for node addresses.
// A VIP that is not keepalived-labelled (kube-vip, or any IPv6 VIP since
// labels do not exist there) must not be chosen over the node's own address.
// This emulates such a VIP with a plain host address numerically below the
// node IP, which is where VIPs conventionally sit, and forces a fresh VTEP IP
// selection with the VIP present.
//
// Serial: the node-subnet VTEP is shared with the other EVPN tests and is
// temporarily recreated here to force the selection, then restored.
var _ = ginkgo.Describe("EVPN: unmanaged VTEP IP selection", feature.RouteAdvertisements, feature.EVPN, ginkgo.Serial, func() {
	const fakeVIPIf = "lo"

	f := wrappedTestFramework("evpn-vtep-ip-selection")
	f.SkipNamespaceCreation = true

	var ictx infraapi.Context

	ginkgo.BeforeEach(func() {
		if !isLocalGWModeEnabled() {
			e2eskipper.Skipf("Test case only supported in Local Gateway mode")
		}
		ictx = infraprovider.Get().NewTestContext()
	})

	// apiCtx bounds a single Kubernetes API call so a stalled API server fails
	// the step instead of hanging the test (and the Eventually polling it).
	apiCtx := func() (context.Context, context.CancelFunc) {
		return context.WithTimeout(context.Background(), 30*time.Second)
	}

	ginkgo.It("prefers the node primary address over an unlabelled VIP in the node subnet", func() {
		listCtx, listCancel := apiCtx()
		defer listCancel()
		nodeList, err := f.ClientSet.CoreV1().Nodes().List(listCtx, metav1.ListOptions{})
		gomega.Expect(err).NotTo(gomega.HaveOccurred(), "list nodes for VTEP IP selection")
		gomega.Expect(nodeList.Items).NotTo(gomega.BeEmpty(), "VTEP IP selection requires a node")
		node := nodeList.Items[0]

		primary, err := util.GetNodeIfAddrAnnotation(&node)
		gomega.Expect(err).NotTo(gomega.HaveOccurred(), "read primary interface address on node %s", node.Name)
		gomega.Expect(primary.IPv4).NotTo(gomega.BeEmpty(), "test needs an IPv4 primary address")
		primaryIP, primaryNet, err := net.ParseCIDR(primary.IPv4)
		gomega.Expect(err).NotTo(gomega.HaveOccurred(), "parse primary IPv4 address %q on node %s", primary.IPv4, node.Name)

		// The fake VIP must sort before the node IP (so a lowest-address rule
		// would pick it) and must not be in use on the node subnet: skip the
		// network and gateway addresses and every address configured on any
		// node. Node.Status.Addresses only lists the node IP, so also read the
		// host-cidrs annotation, which enumerates every interface address
		// (including existing VIPs) that ovnkube-node discovered.
		inUse := sets.New[string](primaryNet.IP.String(), incrementIP(primaryNet.IP, 1).String())
		for i := range nodeList.Items {
			n := &nodeList.Items[i]
			for _, a := range n.Status.Addresses {
				inUse.Insert(a.Address)
			}
			hostCIDRs, err := util.ParseNodeHostCIDRs(n)
			gomega.Expect(err).NotTo(gomega.HaveOccurred(), "parse host CIDRs on node %s", n.Name)
			for _, cidr := range hostCIDRs.UnsortedList() {
				ip, _, err := net.ParseCIDR(cidr)
				gomega.Expect(err).NotTo(gomega.HaveOccurred(), "parse host CIDR %q on node %s", cidr, n.Name)
				inUse.Insert(ip.String())
			}
		}
		var fakeVIP net.IP
		for off := 2; off < 64; off++ {
			candidate := incrementIP(primaryNet.IP, off)
			if !primaryNet.Contains(candidate) || inUse.Has(candidate.String()) ||
				bytes.Compare(candidate.To16(), primaryIP.To16()) >= 0 {
				continue
			}
			fakeVIP = candidate
			break
		}
		if fakeVIP == nil {
			e2eskipper.Skipf("no free address below the node IP %s in %s to emulate a VIP", primaryIP, primaryNet)
		}

		vtepName := sharedNodeIPsVTEPName
		vtepClient, err := vtepclientset.NewForConfig(f.ClientConfig())
		gomega.Expect(err).NotTo(gomega.HaveOccurred(), "create client for VTEP %s", vtepName)
		getCtx, getCancel := apiCtx()
		defer getCancel()
		originalVTEP, err := vtepClient.K8sV1().VTEPs().Get(getCtx, vtepName, metav1.GetOptions{})
		if !apierrors.IsNotFound(err) {
			gomega.Expect(err).NotTo(gomega.HaveOccurred(), "save shared VTEP %s before replacing it", vtepName)
		}
		sharedVTEPRemoved := false
		// Restore last: test VTEP deletion and VIP removal must complete first.
		ictx.AddCleanUpFn(func() error {
			if originalVTEP == nil || !sharedVTEPRemoved {
				return nil
			}
			restored := originalVTEP.DeepCopy()
			restored.ResourceVersion = ""
			restored.UID = ""
			restored.Generation = 0
			restored.CreationTimestamp = metav1.Time{}
			restored.ManagedFields = nil
			restored.Status = vtepv1.VTEPStatus{}
			createCtx, createCancel := apiCtx()
			defer createCancel()
			if _, err := vtepClient.K8sV1().VTEPs().Create(createCtx, restored, metav1.CreateOptions{}); err != nil {
				return fmt.Errorf("restore shared VTEP %s: %w", vtepName, err)
			}
			if err := waitForVTEPAccepted(f, vtepName); err != nil {
				return fmt.Errorf("wait for restored shared VTEP %s to be accepted: %w", vtepName, err)
			}
			return nil
		})

		ginkgo.By(fmt.Sprintf("Adding unlabelled VIP %s to node %s", fakeVIP, node.Name))
		_, err = infraprovider.Get().ExecK8NodeCommand(node.Name, []string{"ip", "addr", "add", fakeVIP.String() + "/32", "dev", fakeVIPIf})
		gomega.Expect(err).NotTo(gomega.HaveOccurred(), "add VIP %s to %s on node %s", fakeVIP, fakeVIPIf, node.Name)
		ictx.AddCleanUpFn(func() error {
			_, err := infraprovider.Get().ExecK8NodeCommand(node.Name, []string{"ip", "addr", "del", fakeVIP.String() + "/32", "dev", fakeVIPIf})
			if err != nil {
				return fmt.Errorf("remove VIP %s from %s on node %s: %w", fakeVIP, fakeVIPIf, node.Name, err)
			}
			return wait.PollUntilContextTimeout(context.Background(), time.Second, 30*time.Second, true, func(ctx context.Context) (bool, error) {
				n, err := f.ClientSet.CoreV1().Nodes().Get(ctx, node.Name, metav1.GetOptions{})
				if err != nil {
					return false, fmt.Errorf("get node %s after VIP removal: %w", node.Name, err)
				}
				hostCIDRs, err := util.ParseNodeHostCIDRs(n)
				if err != nil {
					return false, fmt.Errorf("parse host CIDRs on node %s after VIP removal: %w", node.Name, err)
				}
				return !hostCIDRs.Has(fakeVIP.String() + "/32"), nil
			})
		})

		ginkgo.By("Waiting for the VIP to be known to the node address manager")
		gomega.Expect(wait.PollUntilContextTimeout(context.Background(), 1*time.Second, 30*time.Second, true, func(ctx context.Context) (bool, error) {
			n, err := f.ClientSet.CoreV1().Nodes().Get(ctx, node.Name, metav1.GetOptions{})
			if err != nil {
				return false, fmt.Errorf("get node %s while waiting for VIP %s: %w", node.Name, fakeVIP, err)
			}
			hostCIDRs, err := util.ParseNodeHostCIDRs(n)
			if err != nil {
				if util.IsAnnotationNotSetError(err) {
					return false, nil
				}
				return false, fmt.Errorf("parse host CIDRs on node %s while waiting for VIP %s: %w", node.Name, fakeVIP, err)
			}
			return hostCIDRs.Has(fakeVIP.String() + "/32"), nil
		})).To(gomega.Succeed(), "wait for VIP %s in host CIDRs on node %s", fakeVIP, node.Name)

		// A VTEP IP already annotated for the node is kept while it stays
		// valid, so the selection has to be forced: remove the node-subnet
		// VTEP if a previous test left it, wait until the node dropped its
		// annotation entry, then create it again with the VIP present.
		deleteVTEPAndWait := func() error {
			deleteCtx, deleteCancel := apiCtx()
			defer deleteCancel()
			err := vtepClient.K8sV1().VTEPs().Delete(deleteCtx, vtepName, metav1.DeleteOptions{})
			if err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("delete VTEP %s: %w", vtepName, err)
			}
			return wait.PollUntilContextTimeout(context.Background(), time.Second, 2*time.Minute, true, func(ctx context.Context) (bool, error) {
				if _, err := vtepClient.K8sV1().VTEPs().Get(ctx, vtepName, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
					if err != nil {
						return false, fmt.Errorf("get VTEP %s during deletion: %w", vtepName, err)
					}
					return false, nil
				}
				nodes, err := f.ClientSet.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
				if err != nil {
					return false, fmt.Errorf("list nodes after deleting VTEP %s: %w", vtepName, err)
				}
				for _, n := range nodes.Items {
					vteps, err := util.ParseNodeVTEPs(&n)
					if err != nil {
						if util.IsAnnotationNotSetError(err) {
							continue
						}
						return false, fmt.Errorf("parse VTEPs on node %s during deletion of %s: %w", n.Name, vtepName, err)
					}
					if _, present := vteps[vtepName]; present {
						return false, nil
					}
				}
				return true, nil
			})
		}
		ginkgo.By(fmt.Sprintf("Removing VTEP %s to force a fresh VTEP IP selection", vtepName))
		// Register cleanup before deletion so failures while waiting still restore state.
		ictx.AddCleanUpFn(deleteVTEPAndWait)
		sharedVTEPRemoved = true
		gomega.Expect(deleteVTEPAndWait()).To(gomega.Succeed(), "remove VTEP %s and its node annotations", vtepName)

		ginkgo.By(fmt.Sprintf("Creating unmanaged VTEP %s over the node subnet %s", vtepName, primaryNet))
		gomega.Expect(createVTEP(f, ictx, vtepName, []string{primaryNet.String()}, vtepv1.VTEPModeUnmanaged)).To(gomega.Succeed(), "create test VTEP %s over %s", vtepName, primaryNet)
		gomega.Expect(waitForVTEPAccepted(f, vtepName)).To(gomega.Succeed(), "wait for test VTEP %s to be accepted", vtepName)

		ginkgo.By("Checking the node selected its primary address, not the VIP, as VTEP IP")
		gomega.Eventually(func() ([]string, error) {
			getCtx, getCancel := apiCtx()
			defer getCancel()
			n, err := f.ClientSet.CoreV1().Nodes().Get(getCtx, node.Name, metav1.GetOptions{})
			if err != nil {
				return nil, fmt.Errorf("get node %s while checking VTEP %s: %w", node.Name, vtepName, err)
			}
			vteps, err := util.ParseNodeVTEPs(n)
			if err != nil {
				return nil, fmt.Errorf("parse VTEPs on node %s while checking %s: %w", node.Name, vtepName, err)
			}
			return vteps[vtepName].IPs, nil
		}).WithTimeout(60*time.Second).WithPolling(2*time.Second).Should(gomega.ConsistOf(primaryIP.String()),
			"VTEP IP must be the node primary address %s; %s is a VIP", primaryIP, fakeVIP)
		framework.Logf("node %s selected %s as VTEP IP with VIP %s present", node.Name, primaryIP, fakeVIP)
	})
})
