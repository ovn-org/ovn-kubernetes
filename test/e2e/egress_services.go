package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"golang.org/x/sync/errgroup"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2epodoutput "k8s.io/kubernetes/test/e2e/framework/pod/output"
	utilnet "k8s.io/utils/net"
)

var _ = ginkgo.Describe("EgressService", func() {
	const (
		egressServiceYAML         = "egress_service.yaml"
		externalKindContainerName = "kind-external-container-for-egress-service"
		podHTTPPort               = 8080
		serviceName               = "test-egress-service"
		blackholeRoutingTable     = "100"
	)

	command := []string{"/agnhost", "netexec", fmt.Sprintf("--http-port=%d", podHTTPPort)}
	pods := []string{"pod1", "pod2", "pod3"}
	podsLabels := map[string]string{"egress": "please"}

	var (
		externalKindIPv4 string
		externalKindIPv6 string
		nodes            []v1.Node
	)

	f := wrappedTestFramework("egress-services")

	ginkgo.BeforeEach(func() {
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
		ginkgo.By("Creating the external kind container to send the traffic to/from")
		externalKindIPv4, externalKindIPv6 = createClusterExternalContainer(externalKindContainerName, agnhostImage,
			[]string{"--privileged", "--network", "kind"}, []string{"netexec", fmt.Sprintf("--http-port=%d", podHTTPPort)})

	})

	ginkgo.AfterEach(func() {
		deleteClusterExternalContainer(externalKindContainerName)
		flushCustomRoutingTablesOnNodes(nodes, blackholeRoutingTable)
	})

	ginkgo.DescribeTable("Should validate pods' egress is SNATed to the LB's ingress ip without selectors",
		func(protocol v1.IPFamily, dstIP *string) {
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
			egressServiceConfig := fmt.Sprintf(`
apiVersion: k8s.ovn.org/v1
kind: EgressService
metadata:
  name: ` + serviceName + `
  namespace: ` + f.Namespace.Name + `
spec:
  sourceIPBy: "LoadBalancerIP"
`)

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
			setSVCRouteOnContainer(externalKindContainerName, svcIP, egressHostV4IP, egressHostV6IP)

			ginkgo.By("Verifying the pods reach the external container with the service's ingress ip")
			for _, pod := range pods {
				gomega.Eventually(func() error {
					return curlAgnHostClientIPFromPod(f.Namespace.Name, pod, svcIP, *dstIP, podHTTPPort)
				}, 5*time.Second, 500*time.Millisecond).ShouldNot(gomega.HaveOccurred(), "failed to reach external container with loadbalancer's ingress ip")
			}

			gomega.Consistently(func() error {
				for _, pod := range pods {
					if err := curlAgnHostClientIPFromPod(f.Namespace.Name, pod, svcIP, *dstIP, podHTTPPort); err != nil {
						return err
					}
				}
				return nil
			}, 2*time.Second, 400*time.Millisecond).ShouldNot(gomega.HaveOccurred(), "failed to reach external container with loadbalancer's ingress ip")

			ginkgo.By("Verifying the external container can reach all of the service's backend pods")
			// This is to be sure we did not break ingress traffic for the service
			reachAllServiceBackendsFromExternalContainer(externalKindContainerName, svcIP, podHTTPPort, pods)

			ginkgo.By("Creating the custom network")
			setBlackholeRoutingTableOnNodes(nodes, blackholeRoutingTable, externalKindIPv4, externalKindIPv6, protocol == v1.IPv4Protocol)

			ginkgo.By("Updating the resource to contain a Network")
			egressServiceConfig = fmt.Sprintf(`
apiVersion: k8s.ovn.org/v1
kind: EgressService
metadata:
  name: ` + serviceName + `
  namespace: ` + f.Namespace.Name + `
spec:
  sourceIPBy: "LoadBalancerIP"
  network: "100"
`)
			if err := os.WriteFile(egressServiceYAML, []byte(egressServiceConfig), 0644); err != nil {
				framework.Failf("Unable to write CRD config to disk: %v", err)
			}
			e2ekubectl.RunKubectlOrDie(f.Namespace.Name, "apply", "-f", egressServiceYAML)

			ginkgo.By("Verifying the pods can't reach the external container due to the blackhole in the custom network")
			gomega.Consistently(func() error {
				for _, pod := range pods {
					err := curlAgnHostClientIPFromPod(f.Namespace.Name, pod, svcIP, *dstIP, podHTTPPort)
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
			delExternalClientBlackholeFromNodes(nodes, blackholeRoutingTable, externalKindIPv4, externalKindIPv6, protocol == v1.IPv4Protocol)
			gomega.Consistently(func() error {
				for _, pod := range pods {
					if err := curlAgnHostClientIPFromPod(f.Namespace.Name, pod, svcIP, *dstIP, podHTTPPort); err != nil {
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
					return curlAgnHostClientIPFromPod(f.Namespace.Name, pod, expected, *dstIP, podHTTPPort)
				}, 3*time.Second, 500*time.Millisecond).ShouldNot(gomega.HaveOccurred(), "failed to reach external container with node's ip")

				gomega.Consistently(func() error {
					return curlAgnHostClientIPFromPod(f.Namespace.Name, pod, expected, *dstIP, podHTTPPort)
				}, 1*time.Second, 200*time.Millisecond).ShouldNot(gomega.HaveOccurred(), "failed to reach external container with node's ip")
			}
		},
		ginkgo.Entry("ipv4 pods", v1.IPv4Protocol, &externalKindIPv4),
		ginkgo.Entry("ipv6 pods", v1.IPv6Protocol, &externalKindIPv6),
	)

	ginkgo.DescribeTable("[LGW] Should validate pods' egress uses node's IP when setting Network without SNAT",
		func(protocol v1.IPFamily, dstIP *string) {
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
			egressServiceConfig := fmt.Sprintf(`
apiVersion: k8s.ovn.org/v1
kind: EgressService
metadata:
  name: ` + serviceName + `
  namespace: ` + f.Namespace.Name + `
spec:
  sourceIPBy: "Network"
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
			e2ekubectl.RunKubectlOrDie(f.Namespace.Name, "create", "-f", egressServiceYAML)
			svc := createLBServiceWithIngressIP(f.ClientSet, f.Namespace.Name, serviceName, protocol, podsLabels, podHTTPPort)
			svcIP := svc.Status.LoadBalancer.Ingress[0].IP

			ginkgo.By("Creating the custom network")
			setBlackholeRoutingTableOnNodes(nodes, blackholeRoutingTable, externalKindIPv4, externalKindIPv6, protocol == v1.IPv4Protocol)

			ginkgo.By("Verifying the pods can't reach the external container due to the blackhole in the custom network")
			gomega.Consistently(func() error {
				for _, pod := range pods {
					err := curlAgnHostClientIPFromPod(f.Namespace.Name, pod, svcIP, *dstIP, podHTTPPort)
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
			delExternalClientBlackholeFromNodes(nodes, blackholeRoutingTable, externalKindIPv4, externalKindIPv6, protocol == v1.IPv4Protocol)
			for i, pod := range pods {
				node := &nodes[i]
				v4, v6 := getNodeAddresses(node)
				expected := v4
				if utilnet.IsIPv6String(svcIP) {
					expected = v6
				}

				gomega.Eventually(func() error {
					return curlAgnHostClientIPFromPod(f.Namespace.Name, pod, expected, *dstIP, podHTTPPort)
				}, 3*time.Second, 500*time.Millisecond).ShouldNot(gomega.HaveOccurred(), "failed to reach external container with node's ip")

				gomega.Consistently(func() error {
					return curlAgnHostClientIPFromPod(f.Namespace.Name, pod, expected, *dstIP, podHTTPPort)
				}, 1*time.Second, 200*time.Millisecond).ShouldNot(gomega.HaveOccurred(), "failed to reach external container with node's ip")
			}

			// Re-adding the blackhole and deleting the EgressService to verify that the pods go back to use the main network.
			ginkgo.By("Re-adding the blackhole the pods should not be able to reach the external container")
			setBlackholeRoutingTableOnNodes(nodes, blackholeRoutingTable, externalKindIPv4, externalKindIPv6, protocol == v1.IPv4Protocol)

			ginkgo.By("Verifying the pods can't reach the external container due to the blackhole in the custom network")
			gomega.Consistently(func() error {
				for _, pod := range pods {
					err := curlAgnHostClientIPFromPod(f.Namespace.Name, pod, svcIP, *dstIP, podHTTPPort)
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
					return curlAgnHostClientIPFromPod(f.Namespace.Name, pod, expected, *dstIP, podHTTPPort)
				}, 3*time.Second, 500*time.Millisecond).ShouldNot(gomega.HaveOccurred(), "failed to reach external container with node's ip")

				gomega.Consistently(func() error {
					return curlAgnHostClientIPFromPod(f.Namespace.Name, pod, expected, *dstIP, podHTTPPort)
				}, 1*time.Second, 200*time.Millisecond).ShouldNot(gomega.HaveOccurred(), "failed to reach external container with node's ip")
			}
		},
		ginkgo.Entry("ipv4 pods", v1.IPv4Protocol, &externalKindIPv4),
		ginkgo.Entry("ipv6 pods", v1.IPv6Protocol, &externalKindIPv6),
	)

	ginkgo.DescribeTable("Should validate the egress SVC SNAT functionality against host-networked pods",
		func(protocol v1.IPFamily) {
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
			egressServiceConfig := fmt.Sprintf(`
apiVersion: k8s.ovn.org/v1
kind: EgressService
metadata:
  name: ` + serviceName + `
  namespace: ` + f.Namespace.Name + `
spec:
  sourceIPBy: "LoadBalancerIP"
`)

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
			var otherDstIP string
			if protocol == v1.IPv6Protocol {
				otherDstIP = "fc00:f853:ccd:e793:ffff::1"
			} else {
				// TODO(mk): replace with non-repeating IP allocator
				otherDstIP = "172.18.1.1"
			}
			_, err = runCommand(containerRuntime, "exec", dstNode.Name, "ip", "addr", "add", otherDstIP, "dev", "breth0")
			if err != nil {
				framework.Failf("failed to add address to node %s: %v", dstNode.Name, err)
			}
			defer func() {
				_, err = runCommand(containerRuntime, "exec", dstNode.Name, "ip", "addr", "delete", otherDstIP, "dev", "breth0")
				if err != nil {
					framework.Failf("failed to remove address from node %s: %v", dstNode.Name, err)
				}
			}()
			ginkgo.By("Creating host-networked pod on non-egress node acting as \"another node\"")
			_, err = createPod(f, hostNetPod, dstNode.Name, f.Namespace.Name, []string{"/agnhost", "netexec", fmt.Sprintf("--http-port=%d", podHTTPPort)}, map[string]string{}, func(p *v1.Pod) {
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
					return curlAgnHostClientIPFromPod(f.Namespace.Name, pod, expectedsrcIP, otherDstIP, podHTTPPort)
				}, 1*time.Second, 200*time.Millisecond).ShouldNot(gomega.HaveOccurred(), "failed to reach other node with node's secondary ip")
			}
		},
		ginkgo.Entry("ipv4 pods", v1.IPv4Protocol),
		ginkgo.Entry("ipv6 pods", v1.IPv6Protocol),
	)

	ginkgo.DescribeTable("Should validate pods' egress is SNATed to the LB's ingress ip with selectors",
		func(protocol v1.IPFamily, dstIP *string) {
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
			egressServiceConfig := fmt.Sprintf(`
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
`)

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
			setSVCRouteOnContainer(externalKindContainerName, svcIP, egressHostV4IP, egressHostV6IP)

			ginkgo.By("Verifying the pods reach the external container with the service's ingress ip")
			for _, pod := range pods {
				gomega.Eventually(func() error {
					return curlAgnHostClientIPFromPod(f.Namespace.Name, pod, svcIP, *dstIP, podHTTPPort)
				}, 5*time.Second, 500*time.Millisecond).ShouldNot(gomega.HaveOccurred(), "failed to reach external container with loadbalancer's ingress ip")
			}

			gomega.Consistently(func() error {
				for _, pod := range pods {
					if err := curlAgnHostClientIPFromPod(f.Namespace.Name, pod, svcIP, *dstIP, podHTTPPort); err != nil {
						return err
					}
				}
				return nil
			}, 2*time.Second, 400*time.Millisecond).ShouldNot(gomega.HaveOccurred(), "failed to reach external container with loadbalancer's ingress ip")

			ginkgo.By("Verifying the external container can reach all of the service's backend pods")
			// This is to be sure we did not break ingress traffic for the service
			reachAllServiceBackendsFromExternalContainer(externalKindContainerName, svcIP, podHTTPPort, pods)

			ginkgo.By("Updating the egress service to select the second node")
			secondNode := nodes[1].Name
			egressServiceConfig = fmt.Sprintf(`
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
`)
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
			setSVCRouteOnContainer(externalKindContainerName, svcIP, egressHostV4IP, egressHostV6IP)

			ginkgo.By("Verifying the pods reach the external container with the service's ingress ip again")
			for _, pod := range pods {
				gomega.Eventually(func() error {
					return curlAgnHostClientIPFromPod(f.Namespace.Name, pod, svcIP, *dstIP, podHTTPPort)
				}, 5*time.Second, 500*time.Millisecond).ShouldNot(gomega.HaveOccurred(), "failed to reach external container with loadbalancer's ingress ip")
			}

			gomega.Consistently(func() error {
				for _, pod := range pods {
					if err := curlAgnHostClientIPFromPod(f.Namespace.Name, pod, svcIP, *dstIP, podHTTPPort); err != nil {
						return err
					}
				}
				return nil
			}, 2*time.Second, 400*time.Millisecond).ShouldNot(gomega.HaveOccurred(), "failed to reach external container with loadbalancer's ingress ip")

			ginkgo.By("Verifying the external container can reach all of the service's backend pods")
			reachAllServiceBackendsFromExternalContainer(externalKindContainerName, svcIP, podHTTPPort, pods)

			ginkgo.By("Updating the egress service selector to match no node")
			egressServiceConfig = fmt.Sprintf(`
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
`)
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
					return curlAgnHostClientIPFromPod(f.Namespace.Name, pod, expected, *dstIP, podHTTPPort)
				}, 3*time.Second, 500*time.Millisecond).ShouldNot(gomega.HaveOccurred(), "failed to reach external container with node's ip")

				gomega.Consistently(func() error {
					return curlAgnHostClientIPFromPod(f.Namespace.Name, pod, expected, *dstIP, podHTTPPort)
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
			setSVCRouteOnContainer(externalKindContainerName, svcIP, egressHostV4IP, egressHostV6IP)

			ginkgo.By("Verifying the pods reach the external container with the service's ingress ip again")
			for _, pod := range pods {
				gomega.Eventually(func() error {
					return curlAgnHostClientIPFromPod(f.Namespace.Name, pod, svcIP, *dstIP, podHTTPPort)
				}, 5*time.Second, 500*time.Millisecond).ShouldNot(gomega.HaveOccurred(), "failed to reach external container with loadbalancer's ingress ip")
			}

			gomega.Consistently(func() error {
				for _, pod := range pods {
					if err := curlAgnHostClientIPFromPod(f.Namespace.Name, pod, svcIP, *dstIP, podHTTPPort); err != nil {
						return err
					}
				}
				return nil
			}, 2*time.Second, 400*time.Millisecond).ShouldNot(gomega.HaveOccurred(), "failed to reach external container with loadbalancer's ingress ip")

			reachAllServiceBackendsFromExternalContainer(externalKindContainerName, svcIP, podHTTPPort, pods)
		},
		ginkgo.Entry("ipv4 pods", v1.IPv4Protocol, &externalKindIPv4),
		ginkgo.Entry("ipv6 pods", v1.IPv6Protocol, &externalKindIPv6),
	)

	ginkgo.DescribeTable("Should validate egress service has higher priority than EgressIP when not assigned to the same node",
		func(protocol v1.IPFamily, dstIP *string) {
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
			egressServiceConfig := fmt.Sprintf(`
apiVersion: k8s.ovn.org/v1
kind: EgressService
metadata:
  name: ` + serviceName + `
  namespace: ` + f.Namespace.Name + `
spec:
  sourceIPBy: "LoadBalancerIP"
  nodeSelector:
    matchLabels:
      kubernetes.io/hostname: ` + nodes[1].Name)

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
			setSVCRouteOnContainer(externalKindContainerName, svcIP, egressHostV4IP, egressHostV6IP)

			// Assign the egress IP without conflicting with any node IP,
			// the kind subnet is /16 or /64 so the following should be fine.
			ginkgo.By("Assigning the EgressIP to a different node")
			eipNode := nodes[0]
			e2enode.AddOrUpdateLabelOnNode(f.ClientSet, eipNode.Name, "k8s.ovn.org/egress-assignable", "dummy")
			defer func() {
				e2ekubectl.RunKubectlOrDie("default", "label", "node", eipNode.Name, "k8s.ovn.org/egress-assignable-")
			}()
			nodev4IP, nodev6IP := getNodeAddresses(&eipNode)
			egressNodeIP := net.ParseIP(nodev4IP)
			if utilnet.IsIPv6String(svcIP) {
				egressNodeIP = net.ParseIP(nodev6IP)
			}
			egressIP := make(net.IP, len(egressNodeIP))
			copy(egressIP, egressNodeIP)
			egressIP[len(egressIP)-2]++

			egressIPYaml := "egressip.yaml"
			egressIPConfig := fmt.Sprintf(`apiVersion: k8s.ovn.org/v1
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
`)

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

			ginkgo.By("Verifying the pods reach the external container with the service's ingress ip")
			for _, pod := range pods {
				gomega.Eventually(func() error {
					return curlAgnHostClientIPFromPod(f.Namespace.Name, pod, svcIP, *dstIP, podHTTPPort)
				}, 5*time.Second, 500*time.Millisecond).ShouldNot(gomega.HaveOccurred(), "failed to reach external container with loadbalancer's ingress ip")
			}

			gomega.Consistently(func() error {
				for _, pod := range pods {
					if err := curlAgnHostClientIPFromPod(f.Namespace.Name, pod, svcIP, *dstIP, podHTTPPort); err != nil {
						return err
					}
				}
				return nil
			}, 2*time.Second, 400*time.Millisecond).ShouldNot(gomega.HaveOccurred(), "failed to reach external container with loadbalancer's ingress ip")

			ginkgo.By("Verifying the external container can reach all of the service's backend pods")
			// This is to be sure we did not break ingress traffic for the service
			reachAllServiceBackendsFromExternalContainer(externalKindContainerName, svcIP, podHTTPPort, pods)

			ginkgo.By("Deleting the EgressService the backend pods should exit with the EgressIP")
			e2ekubectl.RunKubectlOrDie(f.Namespace.Name, "delete", "-f", egressServiceYAML)

			for _, pod := range pods {
				gomega.Eventually(func() error {
					return curlAgnHostClientIPFromPod(f.Namespace.Name, pod, egressIP.String(), *dstIP, podHTTPPort)
				}, 3*time.Second, 500*time.Millisecond).ShouldNot(gomega.HaveOccurred(), "failed to reach external container with eip")

				gomega.Consistently(func() error {
					return curlAgnHostClientIPFromPod(f.Namespace.Name, pod, egressIP.String(), *dstIP, podHTTPPort)
				}, 1*time.Second, 200*time.Millisecond).ShouldNot(gomega.HaveOccurred(), "failed to reach external container with eip")
			}
		},
		ginkgo.Entry("ipv4 pods", v1.IPv4Protocol, &externalKindIPv4),
		ginkgo.Entry("ipv6 pods", v1.IPv6Protocol, &externalKindIPv6),
	)

	ginkgo.DescribeTable("Should validate a node with a local ep is selected when ETP=Local",
		func(protocol v1.IPFamily, dstIP *string) {
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
			egressServiceConfig := fmt.Sprintf(`
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
`)

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
			setSVCRouteOnContainer(externalKindContainerName, svcIP, egressHostV4IP, egressHostV6IP)

			ginkgo.By("Verifying the pods reach the external container with the service's ingress ip")
			for _, pod := range pods {
				gomega.Eventually(func() error {
					return curlAgnHostClientIPFromPod(f.Namespace.Name, pod, svcIP, *dstIP, podHTTPPort)
				}, 5*time.Second, 500*time.Millisecond).ShouldNot(gomega.HaveOccurred(), "failed to reach external container with loadbalancer's ingress ip")
			}

			gomega.Consistently(func() error {
				for _, pod := range pods {
					if err := curlAgnHostClientIPFromPod(f.Namespace.Name, pod, svcIP, *dstIP, podHTTPPort); err != nil {
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
					return curlAgnHostClientIPFromPod(f.Namespace.Name, pod, expected, *dstIP, podHTTPPort)
				}, 3*time.Second, 500*time.Millisecond).ShouldNot(gomega.HaveOccurred(), "failed to reach external container with node's ip")

				gomega.Consistently(func() error {
					return curlAgnHostClientIPFromPod(f.Namespace.Name, pod, expected, *dstIP, podHTTPPort)
				}, 1*time.Second, 200*time.Millisecond).ShouldNot(gomega.HaveOccurred(), "failed to reach external container with node's ip")
			}
		},
		ginkgo.Entry("ipv4 pods", v1.IPv4Protocol, &externalKindIPv4),
		ginkgo.Entry("ipv6 pods", v1.IPv6Protocol, &externalKindIPv6),
	)

	ginkgo.DescribeTable("[LGW] Should validate ingress reply traffic uses the Network",
		func(protocol v1.IPFamily, dstIP *string) {
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
				v4Via, v6Via := getContainerAddressesForNetwork(createdPods[0].Spec.NodeName, primaryNetworkName) // if it's host=ALL, just pick a node with an ep
				if sourceIPBy == "LoadBalancerIP" {
					_, v4Via, v6Via = getEgressSVCHost(f.ClientSet, f.Namespace.Name, serviceName)
				}
				setSVCRouteOnContainer(externalKindContainerName, svcIP, v4Via, v6Via)

				ginkgo.By("Verifying the external client can reach the service")
				gomega.Eventually(func() error {
					_, err := curlServiceAgnHostHostnameFromExternalContainer(externalKindContainerName, svcIP, podHTTPPort)
					return err
				}, 3*time.Second, 500*time.Millisecond).ShouldNot(gomega.HaveOccurred(), "failed to eventually reach service from external container")

				gomega.Consistently(func() error {
					_, err := curlServiceAgnHostHostnameFromExternalContainer(externalKindContainerName, svcIP, podHTTPPort)
					return err
				}, 1*time.Second, 200*time.Millisecond).ShouldNot(gomega.HaveOccurred(), "failed to reach service from external container")

				ginkgo.By("Setting the blackhole on the custom network")
				setBlackholeRoutingTableOnNodes(nodes, blackholeRoutingTable, externalKindIPv4, externalKindIPv6, protocol == v1.IPv4Protocol)

				ginkgo.By("Verifying the external client can't reach the pods due to reply traffic hitting the blackhole in the custom network")
				gomega.Consistently(func() error {
					out, err := curlServiceAgnHostHostnameFromExternalContainer(externalKindContainerName, svcIP, podHTTPPort)
					if err != nil && !strings.Contains(err.Error(), "exit status 28") {
						return fmt.Errorf("expected err to be a connection timed out due to blackhole, got: %w", err)
					}

					if err == nil {
						return fmt.Errorf("external container managed to reach pod %s despite blackhole", out)
					}
					return nil
				}, 3*time.Second, 400*time.Millisecond).ShouldNot(gomega.HaveOccurred(), "managed to reach service despite blackhole")

				ginkgo.By("Removing the blackhole to the external container it should be able to reach the pods")
				delExternalClientBlackholeFromNodes(nodes, blackholeRoutingTable, externalKindIPv4, externalKindIPv6, protocol == v1.IPv4Protocol)

				gomega.Eventually(func() error {
					_, err := curlServiceAgnHostHostnameFromExternalContainer(externalKindContainerName, svcIP, podHTTPPort)
					return err
				}, 3*time.Second, 500*time.Millisecond).ShouldNot(gomega.HaveOccurred(), "failed to eventually reach service from external container")

				gomega.Consistently(func() error {
					_, err := curlServiceAgnHostHostnameFromExternalContainer(externalKindContainerName, svcIP, podHTTPPort)
					return err
				}, 1*time.Second, 200*time.Millisecond).ShouldNot(gomega.HaveOccurred(), "failed to reach service from external container")
			}

			updateEgressServiceAndCheck("LoadBalancerIP", v1.ServiceExternalTrafficPolicyCluster)
			updateEgressServiceAndCheck("LoadBalancerIP", v1.ServiceExternalTrafficPolicyLocal)
			updateEgressServiceAndCheck("Network", v1.ServiceExternalTrafficPolicyCluster)
			updateEgressServiceAndCheck("Network", v1.ServiceExternalTrafficPolicyLocal)

			ginkgo.By("Setting the blackhole on the custom network")
			setBlackholeRoutingTableOnNodes(nodes, blackholeRoutingTable, externalKindIPv4, externalKindIPv6, protocol == v1.IPv4Protocol)
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
				_, err := curlServiceAgnHostHostnameFromExternalContainer(externalKindContainerName, svcIP, podHTTPPort)
				return err
			}, 3*time.Second, 500*time.Millisecond).ShouldNot(gomega.HaveOccurred(), "failed to eventually reach service from external container")

			gomega.Consistently(func() error {
				_, err := curlServiceAgnHostHostnameFromExternalContainer(externalKindContainerName, svcIP, podHTTPPort)
				return err
			}, 1*time.Second, 200*time.Millisecond).ShouldNot(gomega.HaveOccurred(), "failed to reach service from external container")
		},
		ginkgo.Entry("ipv4 pods", v1.IPv4Protocol, &externalKindIPv4),
		ginkgo.Entry("ipv6 pods", v1.IPv6Protocol, &externalKindIPv6),
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
			containerIPv4     string            // IPv4 assigned to the created container
			containerIPv6     string            // IPv6 assigned to the created container
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
				out, err := runCommand(containerRuntime, "network", "create", net.name, "--ipv6", "--subnet", net.IPv4CIDR, "--subnet", net.IPv6CIDR)
				framework.ExpectNoError(err, "failed to create external network %s, out: %s", net.name, out)

				ginkgo.By(fmt.Sprintf("Creating container %s", net.containerName))
				// Setting the --hostname here is important since later we poke the container's /hostname endpoint
				net.containerIPv4, net.containerIPv6 = createClusterExternalContainer(net.containerName, agnhostImage,
					[]string{"--privileged", "--network", net.name, "--hostname", net.containerName}, []string{"netexec", fmt.Sprintf("--http-port=%d", podHTTPPort)})

				ginkgo.By(fmt.Sprintf("Adding a listener for the shared IPv4 %s on %s", sharedIPv4, net.containerName))
				out, err = runCommand(containerRuntime, "exec", net.containerName, "ip", "address", "add", sharedIPv4+"/32", "dev", "lo")
				framework.ExpectNoError(err, "failed to add the loopback ip to dev lo on the container %s, out: %s", net.containerName, out)

				ginkgo.By(fmt.Sprintf("Adding a listener for the shared IPv6 %s on %s", sharedIPv6, net.containerName))
				out, err = runCommand(containerRuntime, "exec", net.containerName, "ip", "address", "add", sharedIPv6+"/128", "dev", "lo")
				framework.ExpectNoError(err, "failed to add the ipv6 loopback ip to dev lo on the container %s, out: %s", net.containerName, out)

				// Connecting the nodes (kind containers) to the networks and creating the routing table
				for _, node := range nodes {
					ginkgo.By(fmt.Sprintf("Connecting container %s to network %s", node.Name, net.name))
					out, err = runCommand(containerRuntime, "network", "connect", net.name, node.Name)
					framework.ExpectNoError(err, "failed to connect container %s to external network %s, out: %s", node.Name, net.name, out)

					ginkgo.By(fmt.Sprintf("Setting routes on node %s for network %s (table id %s)", node.Name, net.name, net.routingTable))
					out, err = runCommand(containerRuntime, "exec", node.Name, "ip", "route", "add", sharedIPv4, "via", net.containerIPv4, "table", net.routingTable)
					framework.ExpectNoError(err, fmt.Sprintf("failed to add route to %s on node %s table %s, out: %s", net.containerIPv4, node.Name, net.routingTable, out))

					out, err = runCommand(containerRuntime, "exec", node.Name, "ip", "-6", "route", "add", sharedIPv6, "via", net.containerIPv6, "table", net.routingTable)
					framework.ExpectNoError(err, fmt.Sprintf("failed to add route to %s on node %s table %s, out: %s", net.containerIPv6, node.Name, net.routingTable, out))

					v4, v6 := getContainerAddressesForNetwork(node.Name, net.name)
					net.nodesV4IPs[node.Name] = v4
					net.nodesV6IPs[node.Name] = v6
				}
			}

		})

		ginkgo.AfterEach(func() {
			for _, net := range []*netSettings{net1, net2} {
				deleteClusterExternalContainer(net.containerName)
				for _, node := range nodes {
					out, err := runCommand(containerRuntime, "network", "disconnect", net.name, node.Name)
					framework.ExpectNoError(err, "failed to disconnect container %s from external network %s, out: %s", node.Name, net.name, out)
				}
				// Remove network after removing the external container and disconnecting the nodes so nothing is attached to it on deletion.
				out, err := runCommand(containerRuntime, "network", "rm", net.name)
				framework.ExpectNoError(err, "failed to remove external network %s, out: %s", net.name, out)

				flushCustomRoutingTablesOnNodes(nodes, net.routingTable)
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
					egressServiceConfig := fmt.Sprintf(`
apiVersion: k8s.ovn.org/v1
kind: EgressService
metadata:
  name: ` + net.serviceName + `
  namespace: ` + f.Namespace.Name + `
spec:
  sourceIPBy: "Network"
  network: ` + fmt.Sprintf("\"%s\"", net.routingTable) + `
`)

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
					setSVCRouteOnContainer(net.containerName, svcIP, net.nodesV4IPs[nodes[0].Name], net.nodesV6IPs[nodes[0].Name])
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
					reachAllServiceBackendsFromExternalContainer(net.containerName, net.serviceIP, podHTTPPort, net.createdPods)
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
						err := curlAgnHostClientIPFromPod(f.Namespace.Name, pod, "", dst, podHTTPPort)
						if err != nil && (strings.Contains(err.Error(), fmt.Sprintf("exit code 28")) ||
							// github runners don't have any routes for IPv6, so we get CURLE_COULDNT_CONNECT
							(protocol == v1.IPv6Protocol && strings.Contains(err.Error(), fmt.Sprintf("exit code 7")))) {
							return nil
						}

						return fmt.Errorf("pod %s did not get a timeout error, err %w", pod, err)
					}
					return nil
				}, 2*time.Second, 400*time.Millisecond).ShouldNot(gomega.HaveOccurred(), "managed to reach external container despite having no routes")

				reachAllServiceBackendsFromExternalContainer(net1.containerName, net1.serviceIP, podHTTPPort, net1.createdPods)
				reachAllServiceBackendsFromExternalContainer(net2.containerName, net2.serviceIP, podHTTPPort, net2.createdPods)
			},
			ginkgo.Entry("ipv4 pods", v1.IPv4Protocol),
			ginkgo.Entry("ipv6 pods", v1.IPv6Protocol),
		)
	})
})

// Creates a LoadBalancer service with the given IP and verifies it was set correctly.
func createLBServiceWithIngressIP(cs kubernetes.Interface, namespace, name string, protocol v1.IPFamily, selector map[string]string, port int32, tweak ...func(svc *v1.Service)) *v1.Service {
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
					Port:     port,
				},
			},
			Type:       v1.ServiceTypeLoadBalancer,
			IPFamilies: []v1.IPFamily{protocol},
		},
	}

	for _, f := range tweak {
		f(svc)
	}

	svc, err := cs.CoreV1().Services(namespace).Create(context.TODO(), svc, metav1.CreateOptions{})
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
// from the LoadBalancer provider.
func setSVCRouteOnContainer(container, svcIP, v4Via, v6Via string) {
	if utilnet.IsIPv4String(svcIP) {
		out, err := runCommand(containerRuntime, "exec", container, "ip", "route", "replace", svcIP, "via", v4Via)
		framework.ExpectNoError(err, "failed to add the service host route on the external container %s, out: %s", container, out)
		return
	}

	out, err := runCommand(containerRuntime, "exec", container, "ip", "-6", "route", "replace", svcIP, "via", v6Via)
	framework.ExpectNoError(err, "failed to add the service host route on the external container %s, out: %s", container, out)
}

// Sends a request to an agnhost destination's /clientip which returns the source IP of the packet.
// Returns an error if the expectedIP is different than the response.
func curlAgnHostClientIPFromPod(namespace, pod, expectedIP, dstIP string, containerPort int) error {
	dst := net.JoinHostPort(dstIP, fmt.Sprint(containerPort))
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

func curlServiceAgnHostHostnameFromExternalContainer(container, svcIP string, svcPort int32) (string, error) {
	dst := net.JoinHostPort(svcIP, fmt.Sprint(svcPort))
	out, err := runCommand(containerRuntime, "exec", container, "curl", "-s", "--retry-connrefused", "--retry", "2", "--max-time", "0.5",
		"--connect-timeout", "0.5", "--retry-delay", "1", fmt.Sprintf("http://%s/hostname", dst))
	if err != nil {
		return out, err
	}

	return strings.ReplaceAll(out, "\n", ""), nil
}

// Sends a request to an agnhost destination's /hostname which returns the hostname of the server.
// Returns an error if the expectedHostname is different than the response.
func curlAgnHostHostnameFromPod(namespace, pod, expectedHostname, dstIP string, containerPort int) error {
	dst := net.JoinHostPort(dstIP, fmt.Sprint(containerPort))
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
func reachAllServiceBackendsFromExternalContainer(container, svcIP string, svcPort int32, svcPods []string) {
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
func setBlackholeRoutingTableOnNodes(nodes []v1.Node, routingTable, externalV4, externalV6 string, useV4 bool) {
	for _, node := range nodes {
		if useV4 {
			setBlackholeRoutesOnRoutingTable(node.Name, externalV4, routingTable)
			continue
		}
		if externalV6 != "" {
			setBlackholeRoutesOnRoutingTable(node.Name, externalV6, routingTable)
		}
	}
}

// Sets the regular+blackhole routes on the nodes to the external container.
func setBlackholeRoutesOnRoutingTable(container, ip, table string) {
	type route struct {
		Dst string `json:"dst"`
		Dev string `json:"dev"`
	}
	out, err := runCommand(containerRuntime, "exec", container, "ip", "--json", "route", "get", ip)
	framework.ExpectNoError(err, fmt.Sprintf("failed to get default route to %s on node %s, out: %s", ip, container, out))

	routes := []route{}
	err = json.Unmarshal([]byte(out), &routes)
	framework.ExpectNoError(err, fmt.Sprintf("failed to parse route to %s on node %s", ip, container))
	gomega.Expect(routes).ToNot(gomega.HaveLen(0))

	routeTo := routes[0]
	out, err = runCommand(containerRuntime, "exec", container, "ip", "route", "replace", ip, "dev", routeTo.Dev, "table", table, "prio", "100")
	framework.ExpectNoError(err, fmt.Sprintf("failed to set route to %s on node %s table %s, out: %s", ip, container, table, out))

	out, err = runCommand(containerRuntime, "exec", container, "ip", "route", "replace", "blackhole", ip, "table", table, "prio", "50")
	framework.ExpectNoError(err, fmt.Sprintf("failed to set blackhole route to %s on node %s table %s, out: %s", ip, container, table, out))
}

// Removes the blackhole route to the external container on the nodes.
func delExternalClientBlackholeFromNodes(nodes []v1.Node, routingTable, externalV4, externalV6 string, useV4 bool) {
	for _, node := range nodes {
		if useV4 {
			out, err := runCommand(containerRuntime, "exec", node.Name, "ip", "route", "del", "blackhole", externalV4, "table", routingTable)
			framework.ExpectNoError(err, fmt.Sprintf("failed to delete blackhole route to %s on node %s table %s, out: %s", externalV4, node.Name, routingTable, out))
			continue
		}
		out, err := runCommand(containerRuntime, "exec", node.Name, "ip", "route", "del", "blackhole", externalV6, "table", routingTable)
		framework.ExpectNoError(err, fmt.Sprintf("failed to delete blackhole route to %s on node %s table %s, out: %s", externalV6, node.Name, routingTable, out))
	}
}

// Flush the custom routing table from all of the nodes.
func flushCustomRoutingTablesOnNodes(nodes []v1.Node, routingTable string) {
	for _, node := range nodes {
		out, err := runCommand(containerRuntime, "exec", node.Name, "ip", "route", "flush", "table", routingTable)
		if err != nil && !strings.Contains(err.Error(), "FIB table does not exist") {
			framework.Failf("Unable to flush table %s on node %s: out: %s, err: %v", routingTable, node.Name, out, err)
		}
		out, err = runCommand(containerRuntime, "exec", node.Name, "ip", "-6", "route", "flush", "table", routingTable)
		if err != nil && !strings.Contains(err.Error(), "FIB table does not exist") {
			framework.Failf("Unable to flush table %s on node %s: out: %s err: %v", routingTable, node.Name, out, err)
		}
	}
}
