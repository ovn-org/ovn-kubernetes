// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/feature"

	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/images"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider"
	infraapi "github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/api"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/ipalloc"

	"golang.org/x/sync/errgroup"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/kubectl/pkg/util/podutils"
	"k8s.io/kubernetes/test/e2e/framework"
	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2epodoutput "k8s.io/kubernetes/test/e2e/framework/pod/output"
	utilnet "k8s.io/utils/net"
)

var _ = ginkgo.Describe("EgressService", feature.EgressService, func() {
	const (
		egressServiceYAML     = "egress_service.yaml"
		externalContainerName = "external-container-for-egress-service"
		podHTTPPort           = "8080"
		serviceName           = "test-egress-service"
		blackholeRoutingTable = "100"
	)

	var (
		command           = []string{"/agnhost", "netexec", fmt.Sprintf("--http-port=%s", podHTTPPort)}
		pods              = []string{"pod1", "pod2", "pod3"}
		podsLabels        = map[string]string{"egress": "please"}
		nodes             []v1.Node
		externalContainer infraapi.ExternalContainer
		f                 = wrappedTestFramework("egress-services")
		providerCtx       infraapi.Context
	)

	skipIfProtoNotAvailableFn := func(protocol v1.IPFamily, container infraapi.ExternalContainer) {
		if protocol == v1.IPv4Protocol && !container.IsIPv4() {
			ginkgo.Skip("skipped because external container does not have an IPv4 address")
		}
		if protocol == v1.IPv6Protocol && !container.IsIPv6() {
			ginkgo.Skip("skipped because external container does not have an IPv6 address")
		}
		// FIXME(mk): consider dualstack clusters
		if IsIPv6Cluster(f.ClientSet) && protocol == v1.IPv4Protocol {
			ginkgo.Skip("skipped because cluster is IPv6")
		}
		if !IsIPv6Cluster(f.ClientSet) && protocol == v1.IPv6Protocol {
			ginkgo.Skip("skipped because cluster is IPv4")
		}
	}

	ginkgo.BeforeEach(func() {
		providerCtx = infraprovider.Get().NewTestContext()
		var err error
		clientSet := f.ClientSet
		n, err := e2enode.GetBoundedReadySchedulableNodes(context.TODO(), clientSet, 3)
		framework.ExpectNoError(err)
		if len(n.Items) < 3 {
			framework.Failf(
				"Test requires >= 3 Ready nodes, but there are only %v nodes",
				len(n.Items))
		}
		nodes = n.Items
		ginkgo.By("Creating the external component to send the traffic to/from")
		primaryProviderNetwork, err := infraprovider.Get().PrimaryNetwork()
		framework.ExpectNoError(err, "failed to get primary provider network")
		externalContainer = infraapi.ExternalContainer{Name: externalContainerName, Image: images.AgnHost(),
			Network: primaryProviderNetwork, ExtPort: 8080,
			CmdArgs: getAgnHostHTTPPortBindCMDArgs(8080)}
		externalContainer, err = providerCtx.CreateExternalContainer(externalContainer)
		framework.ExpectNoError(err, "failed to create external container")
	})

	ginkgo.DescribeTable("Should validate pods' egress is SNATed to the LB's ingress ip without selectors",
		func(protocol v1.IPFamily) {
			skipIfProtoNotAvailableFn(protocol, externalContainer)
			dstIP := externalContainer.GetIPv4()
			if protocol == v1.IPv6Protocol {
				dstIP = externalContainer.GetIPv6()
			}
			ginkgo.By("Creating the backend pods")
			podsCreateSync := errgroup.Group{}
			for i, name := range pods {
				name := name
				i := i
				podsCreateSync.Go(func() error {
					p, err := createGenericPodWithLabel(f, name, nodes[i].Name, f.Namespace.Name, command, podsLabels)
					if p != nil {
						framework.Logf("%s podIPs are: %v", p.Name, p.Status.PodIPs)
					}
					return err
				})
			}

			err := podsCreateSync.Wait()
			framework.ExpectNoError(err, "failed to create backend pods")

			ginkgo.By("Creating an egress service without node selectors")
			egressServiceConfig := `
apiVersion: k8s.ovn.org/v1
kind: EgressService
metadata:
  name: ` + serviceName + `
  namespace: ` + f.Namespace.Name + `
spec:
  sourceIPBy: "LoadBalancerIP"
`

			if err := os.WriteFile(egressServiceYAML, []byte(egressServiceConfig), 0644); err != nil {
				framework.Failf("Unable to write CRD config to disk: %v", err)
			}
			defer func() {
				if err := os.Remove(egressServiceYAML); err != nil {
					framework.Logf("Unable to remove the CRD config from disk: %v", err)
				}
			}()
			e2ekubectl.RunKubectlOrDie(f.Namespace.Name, "create", "-f", egressServiceYAML)
			svc := createLBServiceWithIngressIP(f.ClientSet, f.Namespace.Name, serviceName, protocol, podsLabels, podHTTPPort)
			svcIP := svc.Status.LoadBalancer.Ingress[0].IP

			ginkgo.By("Getting the IPs of the node in charge of the service")
			_, egressHostV4IP, egressHostV6IP := getEgressSVCHost(f.ClientSet, f.Namespace.Name, serviceName)

			ginkgo.By("Setting the static route on the external container for the service via the egress host ip")
			setSVCRouteOnExternalContainer(externalContainer, svcIP, egressHostV4IP, egressHostV6IP)

			ginkgo.By("Verifying the pods reach the external container with the service's ingress ip")
			for _, pod := range pods {
				gomega.Eventually(func() error {
					return curlAgnHostClientIPFromPod(f.Namespace.Name, pod, svcIP, dstIP, externalContainer.GetPortStr())
				}, 5*time.Second, 500*time.Millisecond).ShouldNot(gomega.HaveOccurred(), "failed to reach external container with loadbalancer's ingress ip")
			}

			gomega.Consistently(func() error {
				for _, pod := range pods {
					if err := curlAgnHostClientIPFromPod(f.Namespace.Name, pod, svcIP, dstIP, externalContainer.GetPortStr()); err != nil {
						return err
					}
				}
				return nil
			}, 2*time.Second, 400*time.Millisecond).ShouldNot(gomega.HaveOccurred(), "failed to reach external container with loadbalancer's ingress ip")

			ginkgo.By("Verifying the external container can reach all of the service's backend pods")
			// This is to be sure we did not break ingress traffic for the service
			reachAllServiceBackendsFromExternalContainer(externalContainer, svcIP, podHTTPPort, pods)

			ginkgo.By("Creating the custom network")
			setBlackholeRoutingTableOnNodes(providerCtx, nodes, externalContainer, blackholeRoutingTable, protocol == v1.IPv4Protocol)

			ginkgo.By("Updating the resource to contain a Network")
			egressServiceConfig = `
apiVersion: k8s.ovn.org/v1
kind: EgressService
metadata:
  name: ` + serviceName + `
  namespace: ` + f.Namespace.Name + `
spec:
  sourceIPBy: "LoadBalancerIP"
  network: "100"
`
			if err := os.WriteFile(egressServiceYAML, []byte(egressServiceConfig), 0644); err != nil {
				framework.Failf("Unable to write CRD config to disk: %v", err)
			}
			e2ekubectl.RunKubectlOrDie(f.Namespace.Name, "apply", "-f", egressServiceYAML)

			ginkgo.By("Verifying the pods can't reach the external container due to the blackhole in the custom network")
			gomega.Consistently(func() error {
				for _, pod := range pods {
					err := curlAgnHostClientIPFromPod(f.Namespace.Name, pod, svcIP, dstIP, externalContainer.GetPortStr())
					if err != nil && !strings.Contains(err.Error(), "exit code 28") {
						return fmt.Errorf("expected err to be a connection timed out due to blackhole, got: %w", err)
					}

					if err == nil {
						return fmt.Errorf("pod %s managed to reach external client despite blackhole", pod)
					}
				}
				return nil
			}, 2*time.Second, 400*time.Millisecond).ShouldNot(gomega.HaveOccurred(), "managed to reach external container despite blackhole")

			ginkgo.By("Removing the blackhole to the external container the pods should be able to reach it with the loadbalancer's ingress ip")
			delExternalClientBlackholeFromNodes(nodes, blackholeRoutingTable, externalContainer.GetIPv4(), externalContainer.GetIPv6(), protocol == v1.IPv4Protocol)
			gomega.Consistently(func() error {
				for _, pod := range pods {
					if err := curlAgnHostClientIPFromPod(f.Namespace.Name, pod, svcIP, dstIP, externalContainer.GetPortStr()); err != nil {
						return err
					}
				}
				return nil
			}, 2*time.Second, 400*time.Millisecond).ShouldNot(gomega.HaveOccurred(), "failed to reach external container with loadbalancer's ingress ip")

			ginkgo.By("Deleting the EgressService the backend pods should exit with their node's IP")
			e2ekubectl.RunKubectlOrDie(f.Namespace.Name, "delete", "-f", egressServiceYAML)

			for i, pod := range pods {
				node := &nodes[i]
				v4, v6 := getNodeAddresses(node)
				expected := v4
				if utilnet.IsIPv6String(svcIP) {
					expected = v6
				}

				gomega.Eventually(func() error {
					return curlAgnHostClientIPFromPod(f.Namespace.Name, pod, expected, dstIP, externalContainer.GetPortStr())
				}, 3*time.Second, 500*time.Millisecond).ShouldNot(gomega.HaveOccurred(), "failed to reach external container with node's ip")

				gomega.Consistently(func() error {
					return curlAgnHostClientIPFromPod(f.Namespace.Name, pod, expected, dstIP, externalContainer.GetPortStr())
				}, 1*time.Second, 200*time.Millisecond).ShouldNot(gomega.HaveOccurred(), "failed to reach external container with node's ip")
			}
		},
		ginkgo.Entry("ipv4 pods", v1.IPv4Protocol),
		ginkgo.Entry("ipv6 pods", v1.IPv6Protocol),
	)

	ginkgo.DescribeTable("[LGW] Should validate pods' egress uses node's IP when setting Network without SNAT",
		func(protocol v1.IPFamily) {
			skipIfProtoNotAvailableFn(protocol, externalContainer)
			dstIP := externalContainer.GetIPv4()
			if protocol == v1.IPv6Protocol {
				dstIP = externalContainer.GetIPv6()
			}
			ginkgo.By("Creating the backend pods")
			podsCreateSync := errgroup.Group{}
			for i, name := range pods {
				name := name
				i := i
				podsCreateSync.Go(func() error {
					p, err := createGenericPodWithLabel(f, name, nodes[i].Name, f.Namespace.Name, command, podsLabels)
					if p != nil {
						framework.Logf("%s podIPs are: %v", p.Name, p.Status.PodIPs)
					}
					return err
				})
			}

			err := podsCreateSync.Wait()
			framework.ExpectNoError(err, "failed to create backend pods")

			ginkgo.By("Creating an egress service with custom network without SNAT")
			egressServiceConfig := `
apiVersion: k8s.ovn.org/v1
kind: EgressService
metadata:
  name: ` + serviceName + `
  namespace: ` + f.Namespace.Name + `
spec:
  sourceIPBy: "Network"
  network: "100"
`

			if err := os.WriteFile(egressServiceYAML, []byte(egressServiceConfig), 0644); err != nil {
				framework.Failf("Unable to write CRD config to disk: %v", err)
			}
			defer func() {
				if err := os.Remove(egressServiceYAML); err != nil {
					framework.Logf("Unable to remove the CRD config from disk: %v", err)
				}
			}()
			e2ekubectl.RunKubectlOrDie(f.Namespace.Name, "create", "-f", egressServiceYAML)
			svc := createLBServiceWithIngressIP(f.ClientSet, f.Namespace.Name, serviceName, protocol, podsLabels, podHTTPPort)
			svcIP := svc.Status.LoadBalancer.Ingress[0].IP

			ginkgo.By("Creating the custom network")
			setBlackholeRoutingTableOnNodes(providerCtx, nodes, externalContainer, blackholeRoutingTable, protocol == v1.IPv4Protocol)

			ginkgo.By("Verifying the pods can't reach the external container due to the blackhole in the custom network")
			gomega.Consistently(func() error {
				for _, pod := range pods {
					err := curlAgnHostClientIPFromPod(f.Namespace.Name, pod, svcIP, dstIP, externalContainer.GetPortStr())
					if err != nil && !strings.Contains(err.Error(), "exit code 28") {
						return fmt.Errorf("expected err to be a connection timed out due to blackhole, got: %w", err)
					}

					if err == nil {
						return fmt.Errorf("pod %s managed to reach external client despite blackhole", pod)
					}
				}
				return nil
			}, 2*time.Second, 400*time.Millisecond).ShouldNot(gomega.HaveOccurred(), "managed to reach external container despite blackhole")

			ginkgo.By("Removing the blackhole to the external container the pods should be able to reach it with the node's IP")
			delExternalClientBlackholeFromNodes(nodes, blackholeRoutingTable, externalContainer.GetIPv4(), externalContainer.GetIPv6(), protocol == v1.IPv4Protocol)
			for i, pod := range pods {
				node := &nodes[i]
				v4, v6 := getNodeAddresses(node)
				expected := v4
				if utilnet.IsIPv6String(svcIP) {
					expected = v6
				}

				gomega.Eventually(func() error {
					return curlAgnHostClientIPFromPod(f.Namespace.Name, pod, expected, dstIP, externalContainer.GetPortStr())
				}, 3*time.Second, 500*time.Millisecond).ShouldNot(gomega.HaveOccurred(), "failed to reach external container with node's ip")

				gomega.Consistently(func() error {
					return curlAgnHostClientIPFromPod(f.Namespace.Name, pod, expected, dstIP, externalContainer.GetPortStr())
				}, 1*time.Second, 200*time.Millisecond).ShouldNot(gomega.HaveOccurred(), "failed to reach external container with node's ip")
			}

			// Re-adding the blackhole and deleting the EgressService to verify that the pods go back to use the main network.
			ginkgo.By("Re-adding the blackhole the pods should not be able to reach the external container")
			setBlackholeRoutingTableOnNodes(providerCtx, nodes, externalContainer, blackholeRoutingTable, protocol == v1.IPv4Protocol)

			ginkgo.By("Verifying the pods can't reach the external container due to the blackhole in the custom network")
			gomega.Consistently(func() error {
				for _, pod := range pods {
					err := curlAgnHostClientIPFromPod(f.Namespace.Name, pod, svcIP, dstIP, externalContainer.GetPortStr())
					if err != nil && !strings.Contains(err.Error(), "exit code 28") {
						return fmt.Errorf("expected err to be a connection timed out due to blackhole, got: %w", err)
					}

					if err == nil {
						return fmt.Errorf("pod %s managed to reach external client despite blackhole", pod)
					}
				}
				return nil
			}, 2*time.Second, 400*time.Millisecond).ShouldNot(gomega.HaveOccurred(), "managed to reach external container despite blackhole")

			ginkgo.By("Deleting the EgressService the backend pods should exit with their node's IP (via the main network)")
			e2ekubectl.RunKubectlOrDie(f.Namespace.Name, "delete", "-f", egressServiceYAML)

			for i, pod := range pods {
				node := &nodes[i]
				v4, v6 := getNodeAddresses(node)
				expected := v4
				if utilnet.IsIPv6String(svcIP) {
					expected = v6
				}

				gomega.Eventually(func() error {
					return curlAgnHostClientIPFromPod(f.Namespace.Name, pod, expected, dstIP, externalContainer.GetPortStr())
				}, 3*time.Second, 500*time.Millisecond).ShouldNot(gomega.HaveOccurred(), "failed to reach external container with node's ip")

				gomega.Consistently(func() error {
					return curlAgnHostClientIPFromPod(f.Namespace.Name, pod, expected, dstIP, externalContainer.GetPortStr())
				}, 1*time.Second, 200*time.Millisecond).ShouldNot(gomega.HaveOccurred(), "failed to reach external container with node's ip")
			}
		},
		ginkgo.Entry("ipv4 pods", v1.IPv4Protocol),
		ginkgo.Entry("ipv6 pods", v1.IPv6Protocol),
	)

	ginkgo.DescribeTable("Should validate the egress SVC SNAT functionality against host-networked pods",
		func(protocol v1.IPFamily) {
			skipIfProtoNotAvailableFn(protocol, externalContainer)
			ginkgo.By("Creating the backend pods")
			podsCreateSync := errgroup.Group{}
			podsToNodeMapping := make(map[string]v1.Node, 3)
			for i, name := range pods {
				name := name
				i := i
				podsCreateSync.Go(func() error {
					p, err := createGenericPodWithLabel(f, name, nodes[i].Name, f.Namespace.Name, command, podsLabels)
					framework.Logf("%s podIPs are: %v", p.Name, p.Status.PodIPs)
					return err
				})
				podsToNodeMapping[name] = nodes[i]
			}

			err := podsCreateSync.Wait()
			framework.ExpectNoError(err, "failed to create backend pods")

			ginkgo.By("Creating an egress service without node selectors")
			egressServiceConfig := `
apiVersion: k8s.ovn.org/v1
kind: EgressService
metadata:
  name: ` + serviceName + `
  namespace: ` + f.Namespace.Name + `
spec:
  sourceIPBy: "LoadBalancerIP"
`

			if err := os.WriteFile(egressServiceYAML, []byte(egressServiceConfig), 0644); err != nil {
				framework.Failf("Unable to write CRD config to disk: %v", err)
			}
			defer func() {
				if err := os.Remove(egressServiceYAML); err != nil {
					framework.Logf("Unable to remove the CRD config from disk: %v", err)
				}
			}()
			e2ekubectl.RunKubectlOrDie(f.Namespace.Name, "create", "-f", egressServiceYAML)
			_ = createLBServiceWithIngressIP(f.ClientSet, f.Namespace.Name, serviceName, protocol, podsLabels, podHTTPPort)

			ginkgo.By("Getting the IPs of the node in charge of the service")
			egressHost, _, _ := getEgressSVCHost(f.ClientSet, f.Namespace.Name, serviceName)
			framework.Logf("Egress node is %s", egressHost.Name)

			var dstNode v1.Node
			hostNetPod := "host-net-pod"
			for i := range nodes {
				if nodes[i].Name != egressHost.Name { // note that we deliberately pick a non-egress-host as dst node
					dstNode = nodes[i]
					break
				}
			}
			ginkgo.By("By setting a secondary IP on non-egress node acting as \"another node\"")
			var otherDstIP net.IP
			if protocol == v1.IPv6Protocol {
				otherDstIP, err = ipalloc.NewPrimaryIPv6()
			} else {
				otherDstIP, err = ipalloc.NewPrimaryIPv4()
			}
			framework.ExpectNoError(err, "failed to allocate secondary node IP")
			otherDst := otherDstIP.String()
			otherDstCIDR := otherDst + util.GetIPFullMaskString(otherDst)
			ginkgo.By(fmt.Sprintf("adding secondary IP %q to node %s", otherDst, dstNode.Name))
			extBridgeName := deploymentconfig.Get().ExternalBridgeName()
			_, err = infraprovider.Get().ExecK8NodeCommand(dstNode.Name, []string{"ip", "addr", "add", otherDstCIDR, "dev", extBridgeName})
			if err != nil {
				framework.Failf("failed to add address to node %s: %v", dstNode.Name, err)
			}
			providerCtx.AddCleanUpFn(func() error {
				_, err = infraprovider.Get().ExecK8NodeCommand(dstNode.Name, []string{"ip", "addr", "delete", otherDstCIDR, "dev", extBridgeName})
				return err
			})
			ginkgo.By(fmt.Sprintf("Creating host-networked pod on non-egress node %s acting as \"another node\"", dstNode.Name))
			_, err = createPod(f, hostNetPod, dstNode.Name, f.Namespace.Name, []string{"/agnhost", "netexec", fmt.Sprintf("--http-port=%s", podHTTPPort)}, map[string]string{}, func(p *v1.Pod) {
				p.Spec.HostNetwork = true
			})
			framework.ExpectNoError(err)
			framework.Logf("Created pod %s on node %s", hostNetPod, dstNode.Name)

			v4, v6 := getNodeAddresses(&dstNode)
			dstIP := v4
			if protocol == v1.IPv6Protocol {
				dstIP = v6
			}
			ginkgo.By("Verifying traffic from all the 3 backend pods should exit with their node's IP when going towards other nodes in cluster")
			for _, pod := range pods { // loop through all the pods, ensure the curl to other node is always going via nodeIP of the node where the pod lives
				srcNode := podsToNodeMapping[pod]
				if srcNode.Name == dstNode.Name {
					framework.Logf("Skipping check for pod %s because its on the destination node; srcIP will be podIP", pod)
					continue // traffic flow is pod -> mp0 -> local host: This won't have nodeIP as SNAT
				}
				v4, v6 = getNodeAddresses(&srcNode)
				expectedsrcIP := v4
				if protocol == v1.IPv6Protocol {
					expectedsrcIP = v6
				}
				gomega.Consistently(func() error {
					return curlAgnHostClientIPFromPod(f.Namespace.Name, pod, expectedsrcIP, dstIP, podHTTPPort)
				}, 1*time.Second, 200*time.Millisecond).ShouldNot(gomega.HaveOccurred(), "failed to reach other node with node's primary ip")
				gomega.Consistently(func() error {
					return curlAgnHostClientIPFromPod(f.Namespace.Name, pod, expectedsrcIP, otherDst, podHTTPPort)
				}, 1*time.Second, 200*time.Millisecond).ShouldNot(gomega.HaveOccurred(), "failed to reach other node with node's secondary ip")
			}
		},
		ginkgo.Entry("ipv4 pods", v1.IPv4Protocol),
		ginkgo.Entry("ipv6 pods", v1.IPv6Protocol),
	)

	ginkgo.DescribeTable("Should validate pods' egress is SNATed to the LB's ingress ip with selectors",
		func(protocol v1.IPFamily) {
			skipIfProtoNotAvailableFn(protocol, externalContainer)
			externalContainerIP := externalContainer.GetIPv4()
			if protocol == v1.IPv6Protocol {
				externalContainerIP = externalContainer.GetIPv6()
			}
			ginkgo.By("Creating the backend pods")
			podsCreateSync := errgroup.Group{}
			index := 0
			for i, name := range pods {
				name := name
				i := i
				podsCreateSync.Go(func() error {
					_, err := createGenericPodWithLabel(f, name, nodes[i].Name, f.Namespace.Name, command, podsLabels)
					return err
				})
				index++
			}

			err := podsCreateSync.Wait()
			framework.ExpectNoError(err, "failed to create backend pods")

			ginkgo.By("Creating an egress service selecting the first node")
			firstNode := nodes[0].Name
			egressServiceConfig := `
apiVersion: k8s.ovn.org/v1
kind: EgressService
metadata:
  name: ` + serviceName + `
  namespace: ` + f.Namespace.Name + `
spec:
  sourceIPBy: "LoadBalancerIP"
  nodeSelector:
    matchLabels:
      kubernetes.io/hostname: ` + firstNode + `
`

			if err := os.WriteFile(egressServiceYAML, []byte(egressServiceConfig), 0644); err != nil {
				framework.Failf("Unable to write CRD config to disk: %v", err)
			}
			defer func() {
				if err := os.Remove(egressServiceYAML); err != nil {
					framework.Logf("Unable to remove the CRD config from disk: %v", err)
				}
			}()
			e2ekubectl.RunKubectlOrDie(f.Namespace.Name, "create", "-f", egressServiceYAML)
			svc := createLBServiceWithIngressIP(f.ClientSet, f.Namespace.Name, serviceName, protocol, podsLabels, podHTTPPort)
			svcIP := svc.Status.LoadBalancer.Ingress[0].IP

			ginkgo.By("Verifying the first node was picked for handling the service's egress traffic")
			node, egressHostV4IP, egressHostV6IP := getEgressSVCHost(f.ClientSet, f.Namespace.Name, serviceName)
			gomega.Expect(node.Name).To(gomega.Equal(firstNode), "the wrong node got selected for egress service")
			ginkgo.By("Setting the static route on the external container for the service via the first node's ip")
			setSVCRouteOnExternalContainer(externalContainer, svcIP, egressHostV4IP, egressHostV6IP)

			ginkgo.By("Verifying the pods reach the external container with the service's ingress ip")
			for _, pod := range pods {
				gomega.Eventually(func() error {
					return curlAgnHostClientIPFromPod(f.Namespace.Name, pod, svcIP, externalContainerIP, externalContainer.GetPortStr())
				}, 5*time.Second, 500*time.Millisecond).ShouldNot(gomega.HaveOccurred(), "failed to reach external container with loadbalancer's ingress ip")
			}

			gomega.Consistently(func() error {
				for _, pod := range pods {
					if err := curlAgnHostClientIPFromPod(f.Namespace.Name, pod, svcIP, externalContainerIP, externalContainer.GetPortStr()); err != nil {
						return err
					}
				}
				return nil
			}, 2*time.Second, 400*time.Millisecond).ShouldNot(gomega.HaveOccurred(), "failed to reach external container with loadbalancer's ingress ip")

			ginkgo.By("Verifying the external container can reach all of the service's backend pods")
			// This is to be sure we did not break ingress traffic for the service
			reachAllServiceBackendsFromExternalContainer(externalContainer, svcIP, podHTTPPort, pods)

			ginkgo.By("Updating the egress service to select the second node")
			secondNode := nodes[1].Name
			egressServiceConfig = `
apiVersion: k8s.ovn.org/v1
kind: EgressService
metadata:
  name: ` + serviceName + `
  namespace: ` + f.Namespace.Name + `
spec:
  sourceIPBy: "LoadBalancerIP"
  nodeSelector:
    matchLabels:
      kubernetes.io/hostname: ` + secondNode + `
`
			if err := os.WriteFile(egressServiceYAML, []byte(egressServiceConfig), 0644); err != nil {
				framework.Failf("Unable to write CRD config to disk: %v", err)
			}
			e2ekubectl.RunKubectlOrDie(f.Namespace.Name, "apply", "-f", egressServiceYAML)

			ginkgo.By("Verifying the second node now handles the service's egress traffic")
			node, egressHostV4IP, egressHostV6IP = getEgressSVCHost(f.ClientSet, f.Namespace.Name, serviceName)
			gomega.Expect(node.Name).To(gomega.Equal(secondNode), "the wrong node got selected for egress service")
			nodeList, err := f.ClientSet.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{LabelSelector: fmt.Sprintf("egress-service.k8s.ovn.org/%s-%s=", f.Namespace.Name, serviceName)})
			framework.ExpectNoError(err, "failed to list nodes")
			gomega.Expect(len(nodeList.Items)).To(gomega.Equal(1), fmt.Sprintf("expected only one node labeled for the service, got %v", nodeList.Items))

			ginkgo.By("Setting the static route on the external container for the service via the second node's ip")
			setSVCRouteOnExternalContainer(externalContainer, svcIP, egressHostV4IP, egressHostV6IP)

			ginkgo.By("Verifying the pods reach the external container with the service's ingress ip again")
			for _, pod := range pods {
				gomega.Eventually(func() error {
					return curlAgnHostClientIPFromPod(f.Namespace.Name, pod, svcIP, externalContainerIP, externalContainer.GetPortStr())
				}, 5*time.Second, 500*time.Millisecond).ShouldNot(gomega.HaveOccurred(), "failed to reach external container with loadbalancer's ingress ip")
			}

			gomega.Consistently(func() error {
				for _, pod := range pods {
					if err := curlAgnHostClientIPFromPod(f.Namespace.Name, pod, svcIP, externalContainerIP, externalContainer.GetPortStr()); err != nil {
						return err
					}
				}
				return nil
			}, 2*time.Second, 400*time.Millisecond).ShouldNot(gomega.HaveOccurred(), "failed to reach external container with loadbalancer's ingress ip")

			ginkgo.By("Verifying the external container can reach all of the service's backend pods")
			reachAllServiceBackendsFromExternalContainer(externalContainer, svcIP, podHTTPPort, pods)

			ginkgo.By("Updating the egress service selector to match no node")
			egressServiceConfig = `
apiVersion: k8s.ovn.org/v1
kind: EgressService
metadata:
  name: ` + serviceName + `
  namespace: ` + f.Namespace.Name + `
spec:
  sourceIPBy: "LoadBalancerIP"
  nodeSelector:
    matchLabels:
      perfect: match
`
			if err := os.WriteFile(egressServiceYAML, []byte(egressServiceConfig), 0644); err != nil {
				framework.Failf("Unable to write CRD config to disk: %v", err)
			}
			e2ekubectl.RunKubectlOrDie(f.Namespace.Name, "apply", "-f", egressServiceYAML)

			gomega.Eventually(func() error {
				nodeList, err := f.ClientSet.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{LabelSelector: fmt.Sprintf("egress-service.k8s.ovn.org/%s-%s=", f.Namespace.Name, serviceName)})
				if err != nil {
					return err
				}
				if len(nodeList.Items) != 0 {
					return fmt.Errorf("expected no nodes to be labeled for the service, got %v", nodeList.Items)
				}
				return nil
			}, 3*time.Second, 500*time.Millisecond).ShouldNot(gomega.HaveOccurred())

			ginkgo.By("Verifying the backend pods exit with their node's IP")
			for i, pod := range pods {
				node := nodes[i]
				v4, v6 := getNodeAddresses(&node)
				expected := v4
				if utilnet.IsIPv6String(svcIP) {
					expected = v6
				}

				gomega.Eventually(func() error {
					return curlAgnHostClientIPFromPod(f.Namespace.Name, pod, expected, externalContainerIP, externalContainer.GetPortStr())
				}, 3*time.Second, 500*time.Millisecond).ShouldNot(gomega.HaveOccurred(), "failed to reach external container with node's ip")

				gomega.Consistently(func() error {
					return curlAgnHostClientIPFromPod(f.Namespace.Name, pod, expected, externalContainerIP, externalContainer.GetPortStr())
				}, 1*time.Second, 200*time.Millisecond).ShouldNot(gomega.HaveOccurred(), "failed to reach external container with node's ip")
			}

			ginkgo.By("Updating the third node to match the service's selector")
			thirdNode := nodes[2].Name
			node, err = f.ClientSet.CoreV1().Nodes().Get(context.TODO(), thirdNode, metav1.GetOptions{})
			framework.ExpectNoError(err, "failed to get node")
			oldLabels := map[string]string{}
			for k, v := range node.Labels {
				oldLabels[k] = v
			}
			node.Labels["perfect"] = "match"
			_, err = f.ClientSet.CoreV1().Nodes().Update(context.TODO(), node, metav1.UpdateOptions{})
			framework.ExpectNoError(err, "failed to update node's labels")
			defer func() {
				node, err = f.ClientSet.CoreV1().Nodes().Get(context.TODO(), thirdNode, metav1.GetOptions{})
				framework.ExpectNoError(err, "failed to get node")
				node.Labels = oldLabels
				_, err = f.ClientSet.CoreV1().Nodes().Update(context.TODO(), node, metav1.UpdateOptions{})
				framework.ExpectNoError(err, "failed to revert node's labels")
			}()

			ginkgo.By("Verifying the third node now handles the service's egress traffic")
			node, egressHostV4IP, egressHostV6IP = getEgressSVCHost(f.ClientSet, f.Namespace.Name, serviceName)
			gomega.Expect(node.Name).To(gomega.Equal(thirdNode), "the wrong node got selected for egress service")
			nodeList, err = f.ClientSet.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{LabelSelector: fmt.Sprintf("egress-service.k8s.ovn.org/%s-%s=", f.Namespace.Name, serviceName)})
			framework.ExpectNoError(err, "failed to list nodes")
			gomega.Expect(len(nodeList.Items)).To(gomega.Equal(1), fmt.Sprintf("expected only one node labeled for the service, got %v", nodeList.Items))

			ginkgo.By("Setting the static route on the external container for the service via the third node's ip")
			setSVCRouteOnExternalContainer(externalContainer, svcIP, egressHostV4IP, egressHostV6IP)

			ginkgo.By("Verifying the pods reach the external container with the service's ingress ip again")
			for _, pod := range pods {
				gomega.Eventually(func() error {
					return curlAgnHostClientIPFromPod(f.Namespace.Name, pod, svcIP, externalContainerIP, externalContainer.GetPortStr())
				}, 5*time.Second, 500*time.Millisecond).ShouldNot(gomega.HaveOccurred(), "failed to reach external container with loadbalancer's ingress ip")
			}

			gomega.Consistently(func() error {
				for _, pod := range pods {
					if err := curlAgnHostClientIPFromPod(f.Namespace.Name, pod, svcIP, externalContainerIP, externalContainer.GetPortStr()); err != nil {
						return err
					}
				}
				return nil
			}, 2*time.Second, 400*time.Millisecond).ShouldNot(gomega.HaveOccurred(), "failed to reach external container with loadbalancer's ingress ip")

			reachAllServiceBackendsFromExternalContainer(externalContainer, svcIP, podHTTPPort, pods)
		},
		ginkgo.Entry("ipv4 pods", v1.IPv4Protocol),
		ginkgo.Entry("ipv6 pods", v1.IPv6Protocol),
	)

	ginkgo.DescribeTable("Should validate egress service has higher priority than EgressIP when not assigned to the same node",
		func(protocol v1.IPFamily) {
			skipIfProtoNotAvailableFn(protocol, externalContainer)
			dstIP := externalContainer.GetIPv4()
			if protocol == v1.IPv6Protocol {
				dstIP = externalContainer.GetIPv6()
			}
			labels := map[string]string{"wants": "egress"}
			ginkgo.By("Creating the backend pods")
			podsCreateSync := errgroup.Group{}
			for i, name := range pods {
				name := name
				i := i
				podsCreateSync.Go(func() error {
					p, err := createGenericPodWithLabel(f, name, nodes[i].Name, f.Namespace.Name, command, labels)
					if p != nil {
						framework.Logf("%s podIPs are: %v", p.Name, p.Status.PodIPs)
					}
					return err
				})
			}

			err := podsCreateSync.Wait()
			framework.ExpectNoError(err, "failed to create backend pods")

			ginkgo.By("Creating an egress service with node selector")
			egressServiceConfig := `
apiVersion: k8s.ovn.org/v1
kind: EgressService
metadata:
  name: ` + serviceName + `
  namespace: ` + f.Namespace.Name + `
spec:
  sourceIPBy: "LoadBalancerIP"
  nodeSelector:
    matchLabels:
      kubernetes.io/hostname: ` + nodes[1].Name

			if err := os.WriteFile(egressServiceYAML, []byte(egressServiceConfig), 0644); err != nil {
				framework.Failf("Unable to write CRD config to disk: %v", err)
			}
			defer func() {
				if err := os.Remove(egressServiceYAML); err != nil {
					framework.Logf("Unable to remove the CRD config from disk: %v", err)
				}
			}()
			e2ekubectl.RunKubectlOrDie(f.Namespace.Name, "create", "-f", egressServiceYAML)
			svc := createLBServiceWithIngressIP(f.ClientSet, f.Namespace.Name, serviceName, protocol, labels, podHTTPPort)
			svcIP := svc.Status.LoadBalancer.Ingress[0].IP

			ginkgo.By("Getting the IPs of the node in charge of the service")
			_, egressHostV4IP, egressHostV6IP := getEgressSVCHost(f.ClientSet, f.Namespace.Name, serviceName)

			ginkgo.By("Setting the static route on the external container for the service via the egress host ip")
			setSVCRouteOnExternalContainer(externalContainer, svcIP, egressHostV4IP, egressHostV6IP)

			// Assign the egress IP without conflicting with any node IP,
			// the kind subnet is /16 or /64 so the following should be fine.
			ginkgo.By("Assigning the EgressIP to a different node")
			eipNode := nodes[0]
			e2enode.AddOrUpdateLabelOnNode(f.ClientSet, eipNode.Name, "k8s.ovn.org/egress-assignable", "dummy")
			defer func() {
				e2ekubectl.RunKubectlOrDie("default", "label", "node", eipNode.Name, "k8s.ovn.org/egress-assignable-")
			}()
			// allocate EIP IP
			var egressIP net.IP
			if IsIPv6Cluster(f.ClientSet) {
				egressIP, err = ipalloc.NewPrimaryIPv6()
			} else {
				egressIP, err = ipalloc.NewPrimaryIPv4()
			}
			framework.ExpectNoError(err, "must allocate new primary network IP address")
			egressIPYaml := "egressip.yaml"
			egressIPConfig := `apiVersion: k8s.ovn.org/v1
kind: EgressIP
metadata:
    name: egress-svc-test-eip
spec:
    egressIPs:
    - ` + egressIP.String() + `
    podSelector:
        matchLabels:
            wants: egress
    namespaceSelector:
        matchLabels:
            kubernetes.io/metadata.name: ` + f.Namespace.Name + `
`

			if err := os.WriteFile(egressIPYaml, []byte(egressIPConfig), 0644); err != nil {
				framework.Failf("Unable to write CRD config to disk: %v", err)
			}
			defer func() {
				if err := os.Remove(egressIPYaml); err != nil {
					framework.Logf("Unable to remove the CRD config from disk: %v", err)
				}
			}()

			framework.Logf("Create the EgressIP configuration")
			e2ekubectl.RunKubectlOrDie("default", "create", "-f", egressIPYaml)
			defer func() {
				e2ekubectl.RunKubectlOrDie("default", "delete", "eip", "egress-svc-test-eip")
			}()

			ginkgo.By("wait until egress IP is assigned")
			err = wait.PollImmediate(retryInterval, retryTimeout, func() (bool, error) {
				egressIPs := egressIPs{}
				egressIPStdout, err := e2ekubectl.RunKubectl("default", "get", "eip", "-o", "json")
				if err != nil {
					framework.Logf("Error: failed to get the EgressIP object, err: %v", err)
					return false, nil
				}
				err = json.Unmarshal([]byte(egressIPStdout), &egressIPs)
				if err != nil {
					panic(err.Error())
				}
				if len(egressIPs.Items) == 0 {
					return false, nil
				}
				if len(egressIPs.Items) > 1 {
					framework.Failf("Didn't expect to retrieve more than one egress IP during the execution of this test, saw: %v", len(egressIPs.Items))
				}
				return len(egressIPs.Items[0].Status.Items) > 0, nil
			})
			if err != nil {
				framework.Failf("Error: expected to have 1 egress IP assignment, got: 0")
			}

			ginkgo.By("Verifying the pods reach the external container with the service's ingress ip")
			for _, pod := range pods {
				gomega.Eventually(func() error {
					return curlAgnHostClientIPFromPod(f.Namespace.Name, pod, svcIP, dstIP, externalContainer.GetPortStr())
				}, 5*time.Second, 500*time.Millisecond).ShouldNot(gomega.HaveOccurred(), "failed to reach external container with loadbalancer's ingress ip")
			}

			gomega.Consistently(func() error {
				for _, pod := range pods {
					if err := curlAgnHostClientIPFromPod(f.Namespace.Name, pod, svcIP, dstIP, externalContainer.GetPortStr()); err != nil {
						return err
					}
				}
				return nil
			}, 2*time.Second, 400*time.Millisecond).ShouldNot(gomega.HaveOccurred(), "failed to reach external container with loadbalancer's ingress ip")

			ginkgo.By("Verifying the external container can reach all of the service's backend pods")
			// This is to be sure we did not break ingress traffic for the service
			reachAllServiceBackendsFromExternalContainer(externalContainer, svcIP, podHTTPPort, pods)

			ginkgo.By("Deleting the EgressService the backend pods should exit with the EgressIP")
			e2ekubectl.RunKubectlOrDie(f.Namespace.Name, "delete", "-f", egressServiceYAML)

			for _, pod := range pods {
				gomega.Eventually(func() error {
					return curlAgnHostClientIPFromPod(f.Namespace.Name, pod, egressIP.String(), dstIP, externalContainer.GetPortStr())
				}, 10*time.Second, 500*time.Millisecond).ShouldNot(gomega.HaveOccurred(), "failed to reach external container with eip")

				gomega.Consistently(func() error {
					return curlAgnHostClientIPFromPod(f.Namespace.Name, pod, egressIP.String(), dstIP, externalContainer.GetPortStr())
				}, 1*time.Second, 200*time.Millisecond).ShouldNot(gomega.HaveOccurred(), "failed to reach external container with eip")
			}
		},
		ginkgo.Entry("ipv4 pods", v1.IPv4Protocol),
		ginkgo.Entry("ipv6 pods", v1.IPv6Protocol),
	)

	ginkgo.DescribeTable("Should validate a node with a local ep is selected when ETP=Local",
		func(protocol v1.IPFamily) {
			skipIfProtoNotAvailableFn(protocol, externalContainer)
			dstIP := externalContainer.GetIPv4()
			if protocol == v1.IPv6Protocol {
				dstIP = externalContainer.GetIPv6()
			}
			ginkgo.By("Creating two backend pods on the second node")
			firstNode := nodes[0].Name
			secondNode := nodes[1].Name

			podsCreateSync := errgroup.Group{}
			for _, name := range pods[:2] {
				name := name
				podsCreateSync.Go(func() error {
					p, err := createGenericPodWithLabel(f, name, secondNode, f.Namespace.Name, command, podsLabels)
					if p != nil {
						framework.Logf("%s podIPs are: %v", p.Name, p.Status.PodIPs)
					}
					return err
				})
			}

			err := podsCreateSync.Wait()
			framework.ExpectNoError(err, "failed to create backend pods")

			ginkgo.By("Creating an ETP=Local egress service selecting the first node")
			egressServiceConfig := `
apiVersion: k8s.ovn.org/v1
kind: EgressService
metadata:
  name: ` + serviceName + `
  namespace: ` + f.Namespace.Name + `
spec:
  sourceIPBy: "LoadBalancerIP"
  nodeSelector:
    matchLabels:
      kubernetes.io/hostname: ` + firstNode + `
`

			if err := os.WriteFile(egressServiceYAML, []byte(egressServiceConfig), 0644); err != nil {
				framework.Failf("Unable to write CRD config to disk: %v", err)
			}
			defer func() {
				if err := os.Remove(egressServiceYAML); err != nil {
					framework.Logf("Unable to remove the CRD config from disk: %v", err)
				}
			}()
			e2ekubectl.RunKubectlOrDie(f.Namespace.Name, "create", "-f", egressServiceYAML)
			svc := createLBServiceWithIngressIP(f.ClientSet, f.Namespace.Name, serviceName, protocol, podsLabels, podHTTPPort,
				func(svc *v1.Service) {
					svc.Spec.ExternalTrafficPolicy = v1.ServiceExternalTrafficPolicyTypeLocal
				})
			svcIP := svc.Status.LoadBalancer.Ingress[0].IP

			gomega.Consistently(func() error {
				nodeList, err := f.ClientSet.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{LabelSelector: fmt.Sprintf("egress-service.k8s.ovn.org/%s-%s=", svc.Namespace, svc.Name)})
				if err != nil {
					return err
				}
				if len(nodeList.Items) != 0 {
					return fmt.Errorf("expected no nodes to be labeled for the service, got %v", nodeList.Items)
				}

				status, err := getEgressServiceStatus(f.Namespace.Name, serviceName)
				if err != nil {
					return err
				}
				if status.Host != "" {
					return fmt.Errorf("expected no host for egress service %s/%s got: %v", f.Namespace.Name, serviceName, status.Host)
				}

				return nil
			}, 3*time.Second, 500*time.Millisecond).ShouldNot(gomega.HaveOccurred())

			ginkgo.By("Creating the third backend pod on the first node")
			p, err := createGenericPodWithLabel(f, pods[2], firstNode, f.Namespace.Name, command, podsLabels)
			if p != nil {
				framework.Logf("%s podIPs are: %v", p.Name, p.Status.PodIPs)
			}
			framework.ExpectNoError(err)

			ginkgo.By("Verifying the first node was selected for the service")
			node, egressHostV4IP, egressHostV6IP := getEgressSVCHost(f.ClientSet, f.Namespace.Name, serviceName)
			gomega.Expect(node.Name).To(gomega.Equal(firstNode), "the wrong node got selected for egress service")

			ginkgo.By("Setting the static route on the external container for the service via the first node's ip")
			setSVCRouteOnExternalContainer(externalContainer, svcIP, egressHostV4IP, egressHostV6IP)

			ginkgo.By("Verifying the pods reach the external container with the service's ingress ip")
			for _, pod := range pods {
				gomega.Eventually(func() error {
					return curlAgnHostClientIPFromPod(f.Namespace.Name, pod, svcIP, dstIP, externalContainer.GetPortStr())
				}, 5*time.Second, 500*time.Millisecond).ShouldNot(gomega.HaveOccurred(), "failed to reach external container with loadbalancer's ingress ip")
			}

			gomega.Consistently(func() error {
				for _, pod := range pods {
					if err := curlAgnHostClientIPFromPod(f.Namespace.Name, pod, svcIP, dstIP, externalContainer.GetPortStr()); err != nil {
						return err
					}
				}
				return nil
			}, 2*time.Second, 400*time.Millisecond).ShouldNot(gomega.HaveOccurred(), "failed to reach external container with loadbalancer's ingress ip")

			ginkgo.By("Deleting the third pod the service should stop being an egress service")
			err = f.ClientSet.CoreV1().Pods(f.Namespace.Name).Delete(context.TODO(), p.Name, metav1.DeleteOptions{})
			framework.ExpectNoError(err)

			gomega.Eventually(func() error {
				nodeList, err := f.ClientSet.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{LabelSelector: fmt.Sprintf("egress-service.k8s.ovn.org/%s-%s=", svc.Namespace, svc.Name)})
				if err != nil {
					return err
				}
				if len(nodeList.Items) != 0 {
					return fmt.Errorf("expected no nodes to be labeled for the service, got %v", nodeList.Items)
				}

				status, err := getEgressServiceStatus(f.Namespace.Name, serviceName)
				if err != nil {
					return err
				}
				if status.Host != "" {
					return fmt.Errorf("expected no host for egress service %s/%s got: %v", f.Namespace.Name, serviceName, status.Host)
				}

				return nil
			}, 3*time.Second, 500*time.Millisecond).ShouldNot(gomega.HaveOccurred())

			ginkgo.By("Verifying the two backend pods left exit with their node's IP")
			for _, pod := range pods[:2] {
				node := nodes[1]
				v4, v6 := getNodeAddresses(&node)
				expected := v4
				if utilnet.IsIPv6String(svcIP) {
					expected = v6
				}

				gomega.Eventually(func() error {
					return curlAgnHostClientIPFromPod(f.Namespace.Name, pod, expected, dstIP, externalContainer.GetPortStr())
				}, 3*time.Second, 500*time.Millisecond).ShouldNot(gomega.HaveOccurred(), "failed to reach external container with node's ip")

				gomega.Consistently(func() error {
					return curlAgnHostClientIPFromPod(f.Namespace.Name, pod, expected, dstIP, externalContainer.GetPortStr())
				}, 1*time.Second, 200*time.Millisecond).ShouldNot(gomega.HaveOccurred(), "failed to reach external container with node's ip")
			}
		},
		ginkgo.Entry("ipv4 pods", v1.IPv4Protocol),
		ginkgo.Entry("ipv6 pods", v1.IPv6Protocol),
	)

	ginkgo.DescribeTable("[LGW] Should validate ingress reply traffic uses the Network",
		func(protocol v1.IPFamily, isIPv6 bool) {
			ginkgo.By("Creating the backend pods")
			podsCreateSync := errgroup.Group{}
			createdPods := []*v1.Pod{}
			createdPodsLock := sync.Mutex{}
			for i, name := range pods {
				name := name
				i := i
				podsCreateSync.Go(func() error {
					p, err := createGenericPodWithLabel(f, name, nodes[i].Name, f.Namespace.Name, command, podsLabels)
					if p != nil {
						framework.Logf("%s podIPs are: %v", p.Name, p.Status.PodIPs)
						createdPodsLock.Lock()
						createdPods = append(createdPods, p)
						createdPodsLock.Unlock()
					}
					return err
				})
			}

			err := podsCreateSync.Wait()
			framework.ExpectNoError(err, "failed to create backend pods")

			svc := createLBServiceWithIngressIP(f.ClientSet, f.Namespace.Name, serviceName, protocol, podsLabels, podHTTPPort)
			svcIP := svc.Status.LoadBalancer.Ingress[0].IP

			updateEgressServiceAndCheck := func(sourceIPBy string, etp v1.ServiceExternalTrafficPolicyType) {
				ginkgo.By(fmt.Sprintf("Updating with sourceIPBy=%s and ETP=%s", sourceIPBy, etp))
				ginkgo.By("Creating/Updating the egress service")
				egressServiceConfig := fmt.Sprint(`
apiVersion: k8s.ovn.org/v1
kind: EgressService
metadata:
  name: ` + serviceName + `
  namespace: ` + f.Namespace.Name + `
spec:
  sourceIPBy: ` + sourceIPBy + `
  network: "100"
`)

				if err := os.WriteFile(egressServiceYAML, []byte(egressServiceConfig), 0644); err != nil {
					framework.Failf("Unable to write CRD config to disk: %v", err)
				}
				defer func() {
					if err := os.Remove(egressServiceYAML); err != nil {
						framework.Logf("Unable to remove the CRD config from disk: %v", err)
					}
				}()
				e2ekubectl.RunKubectlOrDie(f.Namespace.Name, "apply", "-f", egressServiceYAML)

				ginkgo.By(fmt.Sprintf("Updating the service's ETP to %s", etp))
				svc.Spec.ExternalTrafficPolicy = etp
				svc, err = f.ClientSet.CoreV1().Services(svc.Namespace).Update(context.TODO(), svc, metav1.UpdateOptions{})
				gomega.Expect(err).ToNot(gomega.HaveOccurred())

				ginkgo.By("Setting the routes on the external container to reach the service")
				providerPrimaryNetwork, err := infraprovider.Get().PrimaryNetwork()
				framework.ExpectNoError(err, "provider primary network must be available")
				nodeNetworkInterface, err := infraprovider.Get().GetK8NodeNetworkInterface(createdPods[0].Spec.NodeName, providerPrimaryNetwork)
				framework.ExpectNoError(err, "Node %s network %s information must be available", createdPods[0].Spec.NodeName, providerPrimaryNetwork.Name())
				v4Via, v6Via := nodeNetworkInterface.IPv4, nodeNetworkInterface.IPv6 // if it's host=ALL, just pick a node with an ep
				if sourceIPBy == "LoadBalancerIP" {
					_, v4Via, v6Via = getEgressSVCHost(f.ClientSet, f.Namespace.Name, serviceName)
				}

				setSVCRouteOnContainer(externalContainer, svcIP, v4Via, v6Via)

				ginkgo.By("Verifying the external client can reach the service")
				gomega.Eventually(func() error {
					_, err := curlServiceAgnHostHostnameFromExternalContainer(externalContainer, svcIP, podHTTPPort)
					return err
				}, 3*time.Second, 500*time.Millisecond).ShouldNot(gomega.HaveOccurred(), "failed to eventually reach service from external container")

				gomega.Consistently(func() error {
					_, err := curlServiceAgnHostHostnameFromExternalContainer(externalContainer, svcIP, podHTTPPort)
					return err
				}, 1*time.Second, 200*time.Millisecond).ShouldNot(gomega.HaveOccurred(), "failed to reach service from external container")

				ginkgo.By("Setting the blackhole on the custom network")
				setBlackholeRoutingTableOnNodes(providerCtx, nodes, externalContainer, blackholeRoutingTable, protocol == v1.IPv4Protocol)

				ginkgo.By("Verifying the external client can't reach the pods due to reply traffic hitting the blackhole in the custom network")
				gomega.Consistently(func() error {
					out, err := curlServiceAgnHostHostnameFromExternalContainer(externalContainer, svcIP, podHTTPPort)
					if err != nil && !strings.Contains(err.Error(), "exit status 28") {
						return fmt.Errorf("expected err to be a connection timed out due to blackhole, got: %w", err)
					}

					if err == nil {
						return fmt.Errorf("external container managed to reach pod %s despite blackhole", out)
					}
					return nil
				}, 3*time.Second, 400*time.Millisecond).ShouldNot(gomega.HaveOccurred(), "managed to reach service despite blackhole")

				ginkgo.By("Removing the blackhole to the external container it should be able to reach the pods")
				delExternalClientBlackholeFromNodes(nodes, blackholeRoutingTable, externalContainer.GetIPv4(), externalContainer.GetIPv6(), protocol == v1.IPv4Protocol)

				gomega.Eventually(func() error {
					_, err := curlServiceAgnHostHostnameFromExternalContainer(externalContainer, svcIP, podHTTPPort)
					return err
				}, 3*time.Second, 500*time.Millisecond).ShouldNot(gomega.HaveOccurred(), "failed to eventually reach service from external container")

				gomega.Consistently(func() error {
					_, err := curlServiceAgnHostHostnameFromExternalContainer(externalContainer, svcIP, podHTTPPort)
					return err
				}, 1*time.Second, 200*time.Millisecond).ShouldNot(gomega.HaveOccurred(), "failed to reach service from external container")
			}

			updateEgressServiceAndCheck("LoadBalancerIP", v1.ServiceExternalTrafficPolicyCluster)
			updateEgressServiceAndCheck("LoadBalancerIP", v1.ServiceExternalTrafficPolicyLocal)
			updateEgressServiceAndCheck("Network", v1.ServiceExternalTrafficPolicyCluster)
			updateEgressServiceAndCheck("Network", v1.ServiceExternalTrafficPolicyLocal)

			ginkgo.By("Setting the blackhole on the custom network")
			setBlackholeRoutingTableOnNodes(providerCtx, nodes, externalContainer, blackholeRoutingTable, protocol == v1.IPv4Protocol)
			ginkgo.By("Deleting the EgressService the external client should be able to reach the service")
			egressServiceConfig := fmt.Sprint(`
apiVersion: k8s.ovn.org/v1
kind: EgressService
metadata:
  name: ` + serviceName + `
  namespace: ` + f.Namespace.Name + `
`)

			if err := os.WriteFile(egressServiceYAML, []byte(egressServiceConfig), 0644); err != nil {
				framework.Failf("Unable to write CRD config to disk: %v", err)
			}
			defer func() {
				if err := os.Remove(egressServiceYAML); err != nil {
					framework.Logf("Unable to remove the CRD config from disk: %v", err)
				}
			}()
			e2ekubectl.RunKubectlOrDie(f.Namespace.Name, "delete", "-f", egressServiceYAML)
			gomega.Eventually(func() error {
				_, err := curlServiceAgnHostHostnameFromExternalContainer(externalContainer, svcIP, podHTTPPort)
				return err
			}, 3*time.Second, 500*time.Millisecond).ShouldNot(gomega.HaveOccurred(), "failed to eventually reach service from external container")

			gomega.Consistently(func() error {
				_, err := curlServiceAgnHostHostnameFromExternalContainer(externalContainer, svcIP, podHTTPPort)
				return err
			}, 1*time.Second, 200*time.Millisecond).ShouldNot(gomega.HaveOccurred(), "failed to reach service from external container")
		},
		ginkgo.Entry("ipv4 pods", v1.IPv4Protocol, false),
		ginkgo.Entry("ipv6 pods", v1.IPv6Protocol, true),
	)

	ginkgo.DescribeTable("Should validate egress service backend pods on the egress host retain ClusterIP connectivity",
		func(ctx ginkgo.SpecContext, protocol v1.IPFamily) {
			if protocol == v1.IPv4Protocol && !externalContainer.IsIPv4() {
				ginkgo.Skip("skipped because external container does not have an IPv4 address")
			}
			if protocol == v1.IPv6Protocol && !externalContainer.IsIPv6() {
				ginkgo.Skip("skipped because external container does not have an IPv6 address")
			}

			egressNode := ""
			// Place the backend pod and EgressService host on the same worker node.
			// The API ClusterIP resolves to a different node in Kind.
			for _, node := range nodes {
				if _, isControlPlane := node.Labels["node-role.kubernetes.io/control-plane"]; !isControlPlane {
					egressNode = node.Name
					break
				}
			}
			gomega.Expect(egressNode).NotTo(gomega.BeEmpty(), "test requires a worker node for the EgressService host")

			ginkgo.By("Blackholing the custom routing table on the egress node")
			setBlackholeDefaultRouteOnRoutingTable(providerCtx, egressNode, blackholeRoutingTable, protocol)

			ginkgo.By("Getting the Kubernetes API server ClusterIP for the tested address family")
			apiCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			kubernetesService, err := f.ClientSet.CoreV1().Services(metav1.NamespaceDefault).Get(
				apiCtx, "kubernetes", metav1.GetOptions{})
			cancel()
			framework.ExpectNoError(err, "failed to get the Kubernetes API service")
			ipFamily := utilnet.IPv4
			if protocol == v1.IPv6Protocol {
				ipFamily = utilnet.IPv6
			}
			apiClusterIP := getFirstIPStringOfFamily(ipFamily, kubernetesService.Spec.ClusterIPs)
			if apiClusterIP == "" {
				ginkgo.Skip(fmt.Sprintf("Kubernetes API service has no %s ClusterIP", protocol))
			}

			ginkgo.By("Creating a backend pod for the EgressService on the egress node")
			egressPod, err := createGenericPodWithLabel(f, "egress-pod", egressNode, f.Namespace.Name, command, podsLabels,
				func(pod *v1.Pod) {
					pod.Spec.RestartPolicy = v1.RestartPolicyAlways
					pod.Spec.Containers[0].ReadinessProbe = &v1.Probe{
						ProbeHandler: v1.ProbeHandler{
							Exec: &v1.ExecAction{Command: []string{
								"curl", "-sk", "--connect-timeout", "2",
								fmt.Sprintf("https://%s/version", net.JoinHostPort(apiClusterIP, "443")),
							}},
						},
						TimeoutSeconds:   3,
						PeriodSeconds:    1,
						FailureThreshold: 1,
					}
				})
			framework.ExpectNoError(err, "failed to create egress backend pod")
			podHealthy := func() error {
				apiCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				defer cancel()
				pod, err := f.ClientSet.CoreV1().Pods(f.Namespace.Name).Get(apiCtx, egressPod.Name, metav1.GetOptions{})
				if err != nil {
					return err
				}
				if !podutils.IsPodReady(pod) {
					return fmt.Errorf("pod %s is not ready", pod.Name)
				}
				for _, status := range pod.Status.ContainerStatuses {
					if status.RestartCount != 0 {
						return fmt.Errorf("container %s restarted %d times", status.Name, status.RestartCount)
					}
				}
				return nil
			}

			ginkgo.By("Waiting for the backend pod to reach the API ClusterIP before EgressService is configured")
			gomega.Eventually(podHealthy, 30*time.Second, 500*time.Millisecond).Should(gomega.Succeed())

			ginkgo.By("Creating an EgressService with custom network pinned to the backend pod's node")
			egressServiceConfig := `
apiVersion: k8s.ovn.org/v1
kind: EgressService
metadata:
  name: ` + serviceName + `
  namespace: ` + f.Namespace.Name + `
spec:
  sourceIPBy: "LoadBalancerIP"
  network: "` + blackholeRoutingTable + `"
  nodeSelector:
    matchLabels:
      kubernetes.io/hostname: ` + egressNode + `
`
			if err := os.WriteFile(egressServiceYAML, []byte(egressServiceConfig), 0644); err != nil {
				framework.Failf("Unable to write CRD config to disk: %v", err)
			}
			defer func() {
				if err := os.Remove(egressServiceYAML); err != nil {
					framework.Logf("Unable to remove the CRD config from disk: %v", err)
				}
			}()
			e2ekubectl.RunKubectlOrDie(f.Namespace.Name, "create", "-f", egressServiceYAML)
			createLBServiceWithIngressIP(f.ClientSet, f.Namespace.Name, serviceName, protocol, podsLabels, podHTTPPort)

			ginkgo.By("Waiting for the backend pod's node to be selected as egress host")
			egressHost, _, _ := getEgressSVCHost(f.ClientSet, f.Namespace.Name, serviceName)
			gomega.Expect(egressHost.Name).To(gomega.Equal(egressNode), "expected backend pod's node to be EgressService host")

			ginkgo.By("Verifying the backend pod remains ready without restarts while EgressService is active")
			gomega.Consistently(podHealthy, 30*time.Second, time.Second).Should(gomega.Succeed(),
				"egress backend pod should keep reaching the Kubernetes API ClusterIP without restarting")
		},
		ginkgo.Entry("ipv4 pods", v1.IPv4Protocol),
		ginkgo.Entry("ipv6 pods", v1.IPv6Protocol),
	)

	ginkgo.Describe("Multiple Networks, external clients sharing ip", func() {
		/*
			Here we test the scenario in which we have two different networks (net1,net2), each having
			an external client, both sharing(serving) the same IP (e.g 1.2.3.4).
			We expect to see that the endpoints of different EgressServices (each using a different network)
			reach their "correct" external client when using the shared IP - that is when endpoints that back
			the "net1" EgressService target the shared IP they reach the "net1" external client and when endpoints
			that back the "net2" EgressService target the shared IP they reach the "net2" external client.
			We do that by creating the container networks, attaching them to the nodes and creating routing tables
			on the nodes that route the shared IP through the attached interface (for each network).
		*/
		const (
			sharedIPv4 = "1.2.3.4"
			sharedIPv6 = "1111:2222:3333:4444:5555:6666:7777:8888"
		)

		// netSettings contain the network parameters and is populated when the relevant container and k8s objects are created.
		type netSettings struct {
			name              string            // Name of the network
			IPv4CIDR          string            // IPv4CIDR for the container network
			IPv6CIDR          string            // IPv6CIDR for the container network
			containerName     string            // Container name to create on the network
			routingTable      string            // Routing table ID to set on nodes/EgressService
			nodesV4IPs        map[string]string // The v4 IPs of the nodes corresponding to this network
			nodesV6IPs        map[string]string // The v6 IPs of the nodes corresponding to this network
			podLabels         map[string]string // Labels to set on the pods for the network's Service
			serviceName       string            // Name of the LB service corresponding to this network
			serviceIP         string            // LoadBalancer ingress IP assigned to the Service
			egressServiceYAML string            // YAML file that holds the relevant EgressService
			createdPods       []string          // Pods that were created for the Service
		}

		var (
			net1 *netSettings
			net2 *netSettings
		)

		ginkgo.BeforeEach(func() {
			net1 = &netSettings{
				name:              "net1",
				IPv4CIDR:          "172.41.0.0/16",
				IPv6CIDR:          "fc00:f853:ccd:e401::/64",
				containerName:     "net1-external-container-for-egress-service",
				routingTable:      "101",
				nodesV4IPs:        map[string]string{},
				nodesV6IPs:        map[string]string{},
				podLabels:         map[string]string{"network": "net1"},
				serviceName:       "net1-service",
				egressServiceYAML: "net1-egress_service.yaml",
				createdPods:       []string{},
			}

			net2 = &netSettings{
				name:              "net2",
				IPv4CIDR:          "172.42.0.0/16",
				IPv6CIDR:          "fc00:f853:ccd:e402::/64",
				containerName:     "net2-external-container-for-egress-service",
				routingTable:      "102",
				nodesV4IPs:        map[string]string{},
				nodesV6IPs:        map[string]string{},
				podLabels:         map[string]string{"network": "net2"},
				serviceName:       "net2-service",
				egressServiceYAML: "net2-egress_service.yaml",
				createdPods:       []string{},
			}

			var err error
			clientSet := f.ClientSet
			n, err := e2enode.GetBoundedReadySchedulableNodes(context.TODO(), clientSet, 3)
			framework.ExpectNoError(err)
			if len(n.Items) < 3 {
				framework.Failf(
					"Test requires >= 3 Ready nodes, but there are only %v nodes",
					len(n.Items))
			}
			nodes = n.Items

			ginkgo.By("Setting up the external networks and containers")
			for _, net := range []*netSettings{net1, net2} {
				ginkgo.By(fmt.Sprintf("Creating network %s", net.name))
				network, err := providerCtx.CreateNetwork(net.name, net.IPv4CIDR, net.IPv6CIDR)
				framework.ExpectNoError(err, "failed to create external network %s, out: %s", net.name, err)
				ginkgo.By(fmt.Sprintf("Creating container %s", net.containerName))
				// Setting the --hostname here is important since later we poke the container's /hostname endpoint
				extContainerSecondaryNet := infraapi.ExternalContainer{Name: net.containerName, Image: images.AgnHost(), Network: network,
					CmdArgs: []string{"netexec", "--http-port=8080"}, ExtPort: 8080}
				extContainerSecondaryNet, err = providerCtx.CreateExternalContainer(extContainerSecondaryNet)
				ginkgo.By(fmt.Sprintf("Adding a listener for the shared IPv4 %s on %s", sharedIPv4, net.containerName))
				out, err := infraprovider.Get().ExecExternalContainerCommand(extContainerSecondaryNet, []string{"ip", "address", "add", sharedIPv4 + "/32", "dev", "lo"})
				framework.ExpectNoError(err, "failed to add the loopback ip to dev lo on the container %s, out: %s", net.containerName, out)

				ginkgo.By(fmt.Sprintf("Adding a listener for the shared IPv6 %s on %s", sharedIPv6, net.containerName))
				out, err = infraprovider.Get().ExecExternalContainerCommand(extContainerSecondaryNet, []string{"ip", "address", "add", sharedIPv6 + "/128", "dev", "lo"})
				framework.ExpectNoError(err, "failed to add the ipv6 loopback ip to dev lo on the container %s, out: %s", net.containerName, out)

				// Connecting the nodes (kind containers) to the networks and creating the routing table
				for _, node := range nodes {
					ginkgo.By(fmt.Sprintf("Connecting container %s to network %s", node.Name, net.name))
					_, err := providerCtx.AttachNetwork(network, node.Name)
					framework.ExpectNoError(err, "failed to connect container %s to external network %s", node.Name, net.name)

					ginkgo.By(fmt.Sprintf("Setting routes on node %s for network %s (table id %s)", node.Name, net.name, net.routingTable))
					_, err = infraprovider.Get().ExecK8NodeCommand(node.Name, []string{"ip", "route", "add", sharedIPv4, "via", extContainerSecondaryNet.GetIPv4(), "table", net.routingTable})
					framework.ExpectNoError(err, fmt.Sprintf("failed to add route to %s on node %s table %s", extContainerSecondaryNet.GetIPv4(), node.Name, net.routingTable))
					_, err = infraprovider.Get().ExecK8NodeCommand(node.Name, []string{"ip", "-6", "route", "add", sharedIPv6, "via", extContainerSecondaryNet.GetIPv6(), "table", net.routingTable})
					framework.ExpectNoError(err, fmt.Sprintf("failed to add route to %s on node %s table %s", extContainerSecondaryNet.GetIPv6(), node.Name, net.routingTable))
					providerCtx.AddCleanUpFn(func() error {
						out, err := infraprovider.Get().ExecK8NodeCommand(node.Name, []string{"ip", "route", "flush", "table", net.routingTable})
						if err != nil && !strings.Contains(err.Error(), "FIB table does not exist") {
							return fmt.Errorf("unable to flush table %s on node %s: out: %s, err: %v", net.routingTable, node.Name, out, err)
						}
						out, err = infraprovider.Get().ExecK8NodeCommand(node.Name, []string{"ip", "-6", "route", "flush", "table", net.routingTable})
						if err != nil && !strings.Contains(err.Error(), "FIB table does not exist") {
							return fmt.Errorf("unable to flush table %s on node %s: out: %s err: %v", net.routingTable, node.Name, out, err)
						}
						return nil
					})
					netNetworkInf, err := infraprovider.Get().GetK8NodeNetworkInterface(node.Name, network)
					framework.ExpectNoError(err, "failed to get network interface info for network (%s) on node %s", network, node.Name)
					net.nodesV4IPs[node.Name] = netNetworkInf.IPv4
					net.nodesV6IPs[node.Name] = netNetworkInf.IPv6
				}
			}
		})

		ginkgo.DescribeTable("[LGW] Should validate pods on different networks can reach different clients with same ip without SNAT",
			func(protocol v1.IPFamily) {
				ginkgo.By("Creating the backend pods for the networks")
				podsCreateSync := errgroup.Group{}
				for i, name := range pods {
					for _, net := range []*netSettings{net1, net2} {
						name := fmt.Sprintf("%s-%s", net.name, name)
						i := i
						labels := net.podLabels
						podsCreateSync.Go(func() error {
							p, err := createGenericPodWithLabel(f, name, nodes[i].Name, f.Namespace.Name, command, labels)
							if p != nil {
								framework.Logf("%s podIPs are: %v", p.Name, p.Status.PodIPs)
							}
							return err
						})
						net.createdPods = append(net.createdPods, name)
					}
				}

				err := podsCreateSync.Wait()
				framework.ExpectNoError(err, "failed to create backend pods")

				ginkgo.By("Creating the EgressServices for the networks")
				for _, net := range []*netSettings{net1, net2} {
					egressServiceConfig := `
apiVersion: k8s.ovn.org/v1
kind: EgressService
metadata:
  name: ` + net.serviceName + `
  namespace: ` + f.Namespace.Name + `
spec:
  sourceIPBy: "Network"
  network: ` + fmt.Sprintf("\"%s\"", net.routingTable) + `
`

					if err := os.WriteFile(net.egressServiceYAML, []byte(egressServiceConfig), 0644); err != nil {
						framework.Failf("Unable to write CRD config to disk: %v", err)
					}
					file := net.egressServiceYAML
					defer func() {
						if err := os.Remove(file); err != nil {
							framework.Logf("Unable to remove the CRD config from disk: %v", err)
						}
					}()
					e2ekubectl.RunKubectlOrDie(f.Namespace.Name, "create", "-f", net.egressServiceYAML)

					svc := createLBServiceWithIngressIP(f.ClientSet, f.Namespace.Name, net.serviceName, protocol, net.podLabels, podHTTPPort)
					svcIP := svc.Status.LoadBalancer.Ingress[0].IP

					// We set a route here on the external container to the LB's ingress IP via the first node so it could reach the Service.
					// In a real scenario an external client might have BGP routes to this IP (via a set of nodes), but setting the first node only
					// here is enough for the tests (this is different than the SNAT case, where we must set the route via the Service's host).
					setSVCRouteOnExternalContainer(infraapi.ExternalContainer{Name: net.containerName},
						svcIP, net.nodesV4IPs[nodes[0].Name], net.nodesV6IPs[nodes[0].Name])
					//TODO: figure out if this will persist on target container
					net.serviceIP = svcIP
				}

				for _, net := range []*netSettings{net1, net2} {
					for i, pod := range net.createdPods {
						// The pod should exit with the IP of the interface on the node corresponding to the network
						expected := net.nodesV4IPs[nodes[i].Name]
						dst := sharedIPv4
						if protocol == v1.IPv6Protocol {
							expected = net.nodesV6IPs[nodes[i].Name]
							dst = sharedIPv6
						}
						cleanUp, err := forwardIPWithIPTables(dst)
						ginkgo.DeferCleanup(cleanUp)
						framework.ExpectNoError(err, "must add rules to always forward IP")

						gomega.Eventually(func() error {
							return curlAgnHostClientIPFromPod(f.Namespace.Name, pod, expected, dst, podHTTPPort)
						}, 3*time.Second, 500*time.Millisecond).ShouldNot(gomega.HaveOccurred(), "failed to reach external container with node's ip in the network")

						gomega.Consistently(func() error {
							return curlAgnHostClientIPFromPod(f.Namespace.Name, pod, expected, dst, podHTTPPort)
						}, 1*time.Second, 200*time.Millisecond).ShouldNot(gomega.HaveOccurred(), "failed to reach external container with node's ip in the network")

						gomega.Consistently(func() error {
							return curlAgnHostHostnameFromPod(f.Namespace.Name, pod, net.containerName, dst, podHTTPPort)
						}, 1*time.Second, 200*time.Millisecond).ShouldNot(gomega.HaveOccurred(), "reached an external container with the wrong hostname")
					}
					cleanUp, err := forwardIPWithIPTables(net.serviceIP)
					ginkgo.DeferCleanup(cleanUp)
					framework.ExpectNoError(err, "must add rules to always forward IP")
					//FIXME(mk): whole test case is broken for multi platform
					reachAllServiceBackendsFromExternalContainer(infraapi.ExternalContainer{Name: net.containerName}, net.serviceIP, podHTTPPort, net.createdPods)
				}

				ginkgo.By("Deleting the EgressServices the backend pods should not be able to reach the client (no routes to the shared IPs)")
				e2ekubectl.RunKubectlOrDie(f.Namespace.Name, "delete", "-f", net1.egressServiceYAML)
				e2ekubectl.RunKubectlOrDie(f.Namespace.Name, "delete", "-f", net2.egressServiceYAML)

				dst := sharedIPv4
				if protocol == v1.IPv6Protocol {
					dst = sharedIPv6
				}

				gomega.Consistently(func() error {
					for _, pod := range append(net1.createdPods, net2.createdPods...) {
						err := curlAgnHostClientIPFromPod(f.Namespace.Name, pod, "", dst, externalContainer.GetPortStr())
						if err != nil && (strings.Contains(err.Error(), fmt.Sprintf("exit code 28")) ||
							// github runners don't have any routes for IPv6, so we get CURLE_COULDNT_CONNECT
							(protocol == v1.IPv6Protocol && strings.Contains(err.Error(), fmt.Sprintf("exit code 7")))) {
							return nil
						}

						return fmt.Errorf("pod %s did not get a timeout error, err %w", pod, err)
					}
					return nil
				}, 2*time.Second, 400*time.Millisecond).ShouldNot(gomega.HaveOccurred(), "managed to reach external container despite having no routes")

				reachAllServiceBackendsFromExternalContainer(infraapi.ExternalContainer{Name: net1.containerName}, net1.serviceIP, podHTTPPort, net1.createdPods)
				reachAllServiceBackendsFromExternalContainer(infraapi.ExternalContainer{Name: net2.containerName}, net2.serviceIP, podHTTPPort, net2.createdPods)
			},
			ginkgo.Entry("ipv4 pods", v1.IPv4Protocol),
			ginkgo.Entry("ipv6 pods", v1.IPv6Protocol),
		)
	})
})

// Creates a LoadBalancer service with the given IP and verifies it was set correctly.
func createLBServiceWithIngressIP(cs kubernetes.Interface, namespace, name string, protocol v1.IPFamily, selector map[string]string,
	port string, tweak ...func(svc *v1.Service)) *v1.Service {
	portInt, err := strconv.Atoi(port)
	framework.ExpectNoError(err, "port must be an integer", port)
	svc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      name,
		},
		Spec: v1.ServiceSpec{
			Selector: selector,
			Ports: []v1.ServicePort{
				{
					Protocol: v1.ProtocolTCP,
					Port:     int32(portInt),
				},
			},
			Type:       v1.ServiceTypeLoadBalancer,
			IPFamilies: []v1.IPFamily{protocol},
		},
	}

	for _, f := range tweak {
		f(svc)
	}

	svc, err = cs.CoreV1().Services(namespace).Create(context.TODO(), svc, metav1.CreateOptions{})
	framework.ExpectNoError(err, "failed to create loadbalancer service")

	gomega.Eventually(func() error {
		svc, err = cs.CoreV1().Services(namespace).Get(context.TODO(), name, metav1.GetOptions{})
		if err != nil {
			return err
		}

		if len(svc.Status.LoadBalancer.Ingress) != 1 {
			return fmt.Errorf("expected 1 lb ingress ip, got %v as ips", svc.Status.LoadBalancer.Ingress)
		}

		if len(svc.Status.LoadBalancer.Ingress[0].IP) == 0 {
			return fmt.Errorf("expected lb ingress to be set")
		}

		return nil
	}, 5*time.Second, 1*time.Second).ShouldNot(gomega.HaveOccurred(), "failed to set loadbalancer's ingress ip")

	return svc
}

type egressServiceStatus struct {
	Host string `json:"host"`
}

type egressService struct {
	Status egressServiceStatus `json:"status,omitempty"`
}

func getEgressServiceStatus(ns, name string) (egressServiceStatus, error) {
	egressService := &egressService{}
	egressServiceStdout, err := e2ekubectl.RunKubectl(ns, "get", "egressservice", "-o", "json", name)
	if err != nil {
		framework.Logf("Error: failed to get the EgressService object, err: %v", err)
		return egressServiceStatus{}, err
	}
	err = json.Unmarshal([]byte(egressServiceStdout), egressService)
	if err != nil {
		return egressServiceStatus{}, err
	}

	return egressService.Status, nil
}

// Returns the node in charge of the egress service's traffic and its v4/v6 addresses.
func getEgressSVCHost(cs kubernetes.Interface, svcNamespace, svcName string) (*v1.Node, string, string) {
	egressHost := &v1.Node{}
	egressHostV4IP := ""
	egressHostV6IP := ""
	gomega.Eventually(func() error {
		var err error
		svc, err := cs.CoreV1().Services(svcNamespace).Get(context.TODO(), svcName, metav1.GetOptions{})
		if err != nil {
			return err
		}

		egressServiceStatus, err := getEgressServiceStatus(svcNamespace, svcName)
		if err != nil {
			return err
		}

		svcEgressHost := egressServiceStatus.Host
		if svcEgressHost == "" {
			return fmt.Errorf("egress service %s/%s does not have a host", svcNamespace, svcName)
		}

		egressHost, err = cs.CoreV1().Nodes().Get(context.TODO(), svcEgressHost, metav1.GetOptions{})
		if err != nil {
			return err
		}

		_, found := egressHost.Labels[fmt.Sprintf("egress-service.k8s.ovn.org/%s-%s", svc.Namespace, svc.Name)]
		if !found {
			return fmt.Errorf("node %s does not have the label for egress service %s/%s, labels: %v",
				egressHost.Name, svc.Namespace, svc.Name, egressHost.Labels)
		}

		egressHostV4IP, egressHostV6IP = getNodeAddresses(egressHost)

		return nil
	}, 5*time.Second, 1*time.Second).ShouldNot(gomega.HaveOccurred(), "failed to get egress service host")

	return egressHost, egressHostV4IP, egressHostV6IP
}

// Sets the route to the service via the egress host on the container.
// In a real cluster an external client gets a route for the LoadBalancer service
// from the LoadBalancer infra.
func setSVCRouteOnExternalContainer(container infraapi.ExternalContainer, svcIP, v4Via, v6Via string) {
	if utilnet.IsIPv4String(svcIP) {
		out, err := infraprovider.Get().ExecExternalContainerCommand(container, []string{"ip", "route", "replace", svcIP, "via", v4Via})
		framework.ExpectNoError(err, "failed to add the service host route on the external container %s, out: %s", container, out)
		return
	}
	out, err := infraprovider.Get().ExecExternalContainerCommand(container, []string{"ip", "-6", "route", "replace", svcIP, "via", v6Via})
	framework.ExpectNoError(err, "failed to add the service host route on the external container %s, out: %s", container, out)
}

// Sets the route to the service via the egress host on the container.
// In a real cluster an external client gets a route for the LoadBalancer service
// from the LoadBalancer provider.
func setSVCRouteOnContainer(container infraapi.ExternalContainer, svcIP, v4Via, v6Via string) {
	var out string
	var err error
	if utilnet.IsIPv4String(svcIP) {
		out, err = infraprovider.Get().ExecExternalContainerCommand(container, []string{
			"ip", "route", "replace", svcIP, "via", v4Via,
		})
	} else {
		out, err = infraprovider.Get().ExecExternalContainerCommand(container, []string{
			"ip", "-6", "route", "replace", svcIP, "via", v6Via,
		})
	}
	framework.ExpectNoError(err, "failed to add the service host route on the external container %s, out: %s", container, out)
}

// Sends a request to an agnhost destination's /clientip which returns the source IP of the packet.
// Returns an error if the expectedIP is different than the response.
func curlAgnHostClientIPFromPod(namespace, pod, expectedIP, dstIP, containerPort string) error {
	dst := net.JoinHostPort(dstIP, containerPort)
	curlCmd := fmt.Sprintf("curl -s --retry-connrefused --retry 2 --max-time 0.5 --connect-timeout 0.5 --retry-delay 1 http://%s/clientip", dst)
	out, err := e2epodoutput.RunHostCmd(namespace, pod, curlCmd)
	if err != nil {
		return fmt.Errorf("failed to curl agnhost on %s from %s, err: %w", dstIP, pod, err)
	}
	ourip, _, err := net.SplitHostPort(out)
	if err != nil {
		return fmt.Errorf("failed to split agnhost's clientip host:port response, err: %w", err)
	}
	if ourip != expectedIP {
		return fmt.Errorf("reached agnhost %s with ip %s from %s instead of %s", dstIP, ourip, pod, expectedIP)
	}
	return nil
}

func curlServiceAgnHostHostnameFromExternalContainer(container infraapi.ExternalContainer, svcIP, svcPort string) (string, error) {
	dst := net.JoinHostPort(svcIP, svcPort)
	out, err := infraprovider.Get().ExecExternalContainerCommand(container, []string{"curl", "-s", "--retry-connrefused", "--retry", "2", "--max-time", "0.5",
		"--connect-timeout", "0.5", "--retry-delay", "1", fmt.Sprintf("http://%s/hostname", dst)})
	if err != nil {
		return out, err
	}

	return strings.ReplaceAll(out, "\n", ""), nil
}

// Sends a request to an agnhost destination's /hostname which returns the hostname of the server.
// Returns an error if the expectedHostname is different than the response.
func curlAgnHostHostnameFromPod(namespace, pod, expectedHostname, dstIP string, containerPort string) error {
	dst := net.JoinHostPort(dstIP, containerPort)
	curlCmd := fmt.Sprintf("curl -s --retry-connrefused --retry 2 --max-time 0.5 --connect-timeout 0.5 --retry-delay 1 http://%s/hostname", dst)
	out, err := e2epodoutput.RunHostCmd(namespace, pod, curlCmd)
	if err != nil {
		return fmt.Errorf("failed to curl agnhost on %s from %s, err: %w", dstIP, pod, err)
	}

	if out != expectedHostname {
		return fmt.Errorf("reached agnhost %s with hostname %s from %s instead of %s", dstIP, out, pod, expectedHostname)
	}
	return nil
}

// Tries to reach all of the backends of the given service from the container.
func reachAllServiceBackendsFromExternalContainer(container infraapi.ExternalContainer, svcIP, svcPort string, svcPods []string) {
	backends := map[string]bool{}
	for _, pod := range svcPods {
		backends[pod] = true
	}

	for i := 0; i < 10*len(svcPods); i++ {
		out, err := curlServiceAgnHostHostnameFromExternalContainer(container, svcIP, svcPort)
		framework.ExpectNoError(err, "failed to curl service ingress IP")
		delete(backends, out)
		if len(backends) == 0 {
			break
		}
	}

	gomega.Expect(len(backends)).To(gomega.Equal(0), fmt.Sprintf("did not reach all pods from outside, missed: %v", backends))
}

// Sets the "dummy" custom routing table on all of the nodes (this heavily relies on the environment to be a kind cluster)
// We create a new routing table with 2 routes to the external container:
// 1) The one from the default routing table.
// 2) A blackhole with a higher priority
// Then in the actual test we first verify that when the pods are using the custom routing table they can't reach the external container,
// remove the blackhole route and verify that they can reach it now. This shows that they actually use a different routing table than the main one.
func setBlackholeRoutingTableOnNodes(providerCtx infraapi.Context, nodes []v1.Node, extContainer infraapi.ExternalContainer, routingTable string, useV4 bool) {
	for _, node := range nodes {
		if useV4 {
			setBlackholeRoutesOnRoutingTable(providerCtx, node.Name, extContainer.GetIPv4(), routingTable)
			continue
		}
		if extContainer.IsIPv6() {
			setBlackholeRoutesOnRoutingTable(providerCtx, node.Name, extContainer.GetIPv6(), routingTable)
		}
	}
}

func setBlackholeDefaultRouteOnRoutingTable(providerCtx infraapi.Context, nodeName, table string, protocol v1.IPFamily) {
	family := "-4"
	if protocol == v1.IPv6Protocol {
		family = "-6"
	}

	out, err := infraprovider.Get().ExecK8NodeCommand(nodeName,
		[]string{"ip", family, "route", "replace", "blackhole", "default", "table", table})
	framework.ExpectNoError(err, "failed to set blackhole default route in table %s on node %s, out: %s", table, nodeName, out)

	providerCtx.AddCleanUpFn(func() error {
		out, err := infraprovider.Get().ExecK8NodeCommand(nodeName,
			[]string{"ip", family, "route", "del", "blackhole", "default", "table", table})
		if err != nil && !strings.Contains(err.Error(), "RTNETLINK answers: No such process") {
			return fmt.Errorf("failed to remove blackhole default route from table %s on node %s: stdout %q, err: %w", table, nodeName, out, err)
		}
		return nil
	})
}

// Sets the regular+blackhole routes on the nodes to the external container.
func setBlackholeRoutesOnRoutingTable(providerCtx infraapi.Context, nodeName, ip, table string) {
	type route struct {
		Dst string `json:"dst"`
		Dev string `json:"dev"`
	}
	out, err := infraprovider.Get().ExecK8NodeCommand(nodeName, []string{"ip", "--json", "route", "get", ip})
	framework.ExpectNoError(err, fmt.Sprintf("failed to get default route to %s on node %s, out: %s", ip, nodeName, out))

	routes := []route{}
	err = json.Unmarshal([]byte(out), &routes)
	framework.ExpectNoError(err, fmt.Sprintf("failed to parse route to %s on node %s", ip, nodeName))
	gomega.Expect(routes).ToNot(gomega.HaveLen(0))

	routeTo := routes[0]
	out, err = infraprovider.Get().ExecK8NodeCommand(nodeName, []string{"ip", "route", "replace", ip, "dev", routeTo.Dev, "table", table, "prio", "100"})
	framework.ExpectNoError(err, fmt.Sprintf("failed to set route to %s on node %s table %s, out: %s", ip, nodeName, table, out))

	doesNotExistMsg := "RTNETLINK answers: No such process"
	isAlreadyDeletedFn := func(s string) bool { return strings.Contains(s, doesNotExistMsg) }

	providerCtx.AddCleanUpFn(func() error {
		out, err = infraprovider.Get().ExecK8NodeCommand(nodeName, []string{"ip", "route", "del", "blackhole", ip, "table", table})
		if err != nil && !isAlreadyDeletedFn(err.Error()) {
			return fmt.Errorf("failed to remove black hole route in table 100: stdout %q, err: %q", out, err)
		}
		return nil
	})

	out, err = infraprovider.Get().ExecK8NodeCommand(nodeName, []string{"ip", "route", "replace", "blackhole", ip, "table", table, "prio", "50"})
	providerCtx.AddCleanUpFn(func() error {
		out, err = infraprovider.Get().ExecK8NodeCommand(nodeName, []string{"ip", "route", "del", "blackhole", ip, "table", table})
		if err != nil && !isAlreadyDeletedFn(err.Error()) {
			return fmt.Errorf("failed to remove black hole route in table 50: stdout %q, err: %q", out, err)
		}
		return nil
	})
	framework.ExpectNoError(err, fmt.Sprintf("failed to set blackhole route to %s on node %s table %s, out: %s", ip, nodeName, table, out))
}

// Removes the blackhole route to the external container on the nodes.
func delExternalClientBlackholeFromNodes(nodes []v1.Node, routingTable, externalV4, externalV6 string, useV4 bool) {
	for _, node := range nodes {
		if useV4 {
			out, err := infraprovider.Get().ExecK8NodeCommand(node.Name, []string{"ip", "route", "del", "blackhole", externalV4, "table", routingTable})
			framework.ExpectNoError(err, fmt.Sprintf("failed to delete blackhole route to %s on node %s table %s, out: %s", externalV4, node.Name, routingTable, out))
			continue
		}
		out, err := infraprovider.Get().ExecK8NodeCommand(node.Name, []string{"ip", "route", "del", "blackhole", externalV6, "table", routingTable})
		framework.ExpectNoError(err, fmt.Sprintf("failed to delete blackhole route to %s on node %s table %s, out: %s", externalV6, node.Name, routingTable, out))
	}
}
