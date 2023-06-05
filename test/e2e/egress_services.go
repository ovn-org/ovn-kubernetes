package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/onsi/ginkgo"
	ginkgotable "github.com/onsi/ginkgo/extensions/table"
	"github.com/onsi/gomega"
	"golang.org/x/sync/errgroup"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	utilnet "k8s.io/utils/net"
)

var _ = ginkgo.Describe("Egress Services", func() {
	const (
		egressServiceYAML     = "egress_service.yaml"
		externalContainerName = "external-container-for-egress-service"
		podHTTPPort           = 8080
		serviceName           = "test-egress-service"
		customRoutingTable    = "100"
	)

	command := []string{"/agnhost", "netexec", fmt.Sprintf("--http-port=%d", podHTTPPort)}
	pods := []string{"pod1", "pod2", "pod3"}
	podsLabels := map[string]string{"egress": "please"}

	var (
		externalIPv4 string
		externalIPv6 string
		nodes        []v1.Node
	)

	f := wrappedTestFramework("egress-services")

	ginkgo.BeforeEach(func() {
		var err error
		clientSet := f.ClientSet
		n, err := e2enode.GetBoundedReadySchedulableNodes(clientSet, 3)
		framework.ExpectNoError(err)
		if len(n.Items) < 3 {
			framework.Failf(
				"Test requires >= 3 Ready nodes, but there are only %v nodes",
				len(n.Items))
		}
		nodes = n.Items
		ginkgo.By("Creating an external container to send the traffic to/from")
		externalIPv4, externalIPv6 = createClusterExternalContainer(externalContainerName, agnhostImage,
			[]string{"--privileged", "--network", "kind"}, []string{"netexec", fmt.Sprintf("--http-port=%d", podHTTPPort)})

	})

	ginkgo.AfterEach(func() {
		deleteClusterExternalContainer(externalContainerName)
		flushCustomRoutingTableOnNodes(nodes, customRoutingTable)
	})

	ginkgotable.DescribeTable("Should validate pods' egress is SNATed to the LB's ingress ip without selectors",
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
`)

			if err := os.WriteFile(egressServiceYAML, []byte(egressServiceConfig), 0644); err != nil {
				framework.Failf("Unable to write CRD config to disk: %v", err)
			}
			defer func() {
				if err := os.Remove(egressServiceYAML); err != nil {
					framework.Logf("Unable to remove the CRD config from disk: %v", err)
				}
			}()
			framework.RunKubectlOrDie(f.Namespace.Name, "create", "-f", egressServiceYAML)
			svc := createLBServiceWithIngressIP(f.ClientSet, f.Namespace.Name, serviceName, protocol, podsLabels, podHTTPPort)
			svcIP := svc.Status.LoadBalancer.Ingress[0].IP

			ginkgo.By("Getting the IPs of the node in charge of the service")
			_, egressHostV4IP, egressHostV6IP := getEgressSVCHost(f.ClientSet, f.Namespace.Name, serviceName)

			ginkgo.By("Setting the static route on the external container for the service via the egress host ip")
			setSVCRouteOnContainer(externalContainerName, svcIP, egressHostV4IP, egressHostV6IP)

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
			reachAllServiceBackendsFromExternalContainer(externalContainerName, svcIP, podHTTPPort, pods)

			ginkgo.By("Creating the custom network")
			setCustomRoutingTableOnNodes(nodes, customRoutingTable, externalIPv4, externalIPv6, protocol == v1.IPv4Protocol)

			ginkgo.By("Updating the resource to contain a Network")
			egressServiceConfig = fmt.Sprintf(`
apiVersion: k8s.ovn.org/v1
kind: EgressService
metadata:
  name: ` + serviceName + `
  namespace: ` + f.Namespace.Name + `
spec:
  network: "100"
`)
			if err := os.WriteFile(egressServiceYAML, []byte(egressServiceConfig), 0644); err != nil {
				framework.Failf("Unable to write CRD config to disk: %v", err)
			}
			framework.RunKubectlOrDie(f.Namespace.Name, "apply", "-f", egressServiceYAML)

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
			delExternalClientBlackholeFromNodes(nodes, customRoutingTable, externalIPv4, externalIPv6, protocol == v1.IPv4Protocol)
			gomega.Consistently(func() error {
				for _, pod := range pods {
					if err := curlAgnHostClientIPFromPod(f.Namespace.Name, pod, svcIP, *dstIP, podHTTPPort); err != nil {
						return err
					}
				}
				return nil
			}, 2*time.Second, 400*time.Millisecond).ShouldNot(gomega.HaveOccurred(), "failed to reach external container with loadbalancer's ingress ip")

			ginkgo.By("Deleting the EgressService the backend pods should exit with their node's IP")
			framework.RunKubectlOrDie(f.Namespace.Name, "delete", "-f", egressServiceYAML)

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
		ginkgotable.Entry("ipv4 pods", v1.IPv4Protocol, &externalIPv4),
		ginkgotable.Entry("ipv6 pods", v1.IPv6Protocol, &externalIPv6),
	)

	ginkgotable.DescribeTable("Should validate the egress SVC SNAT functionality against host-networked pods",
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
`)

			if err := os.WriteFile(egressServiceYAML, []byte(egressServiceConfig), 0644); err != nil {
				framework.Failf("Unable to write CRD config to disk: %v", err)
			}
			defer func() {
				if err := os.Remove(egressServiceYAML); err != nil {
					framework.Logf("Unable to remove the CRD config from disk: %v", err)
				}
			}()
			framework.RunKubectlOrDie(f.Namespace.Name, "create", "-f", egressServiceYAML)
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
		ginkgotable.Entry("ipv4 pods", v1.IPv4Protocol),
		ginkgotable.Entry("ipv6 pods", v1.IPv6Protocol),
	)

	ginkgotable.DescribeTable("Should validate pods' egress is SNATed to the LB's ingress ip with selectors",
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
			framework.RunKubectlOrDie(f.Namespace.Name, "create", "-f", egressServiceYAML)
			svc := createLBServiceWithIngressIP(f.ClientSet, f.Namespace.Name, serviceName, protocol, podsLabels, podHTTPPort)
			svcIP := svc.Status.LoadBalancer.Ingress[0].IP

			ginkgo.By("Verifying the first node was picked for handling the service's egress traffic")
			node, egressHostV4IP, egressHostV6IP := getEgressSVCHost(f.ClientSet, f.Namespace.Name, serviceName)
			framework.ExpectEqual(node.Name, firstNode, "the wrong node got selected for egress service")

			ginkgo.By("Setting the static route on the external container for the service via the first node's ip")
			setSVCRouteOnContainer(externalContainerName, svcIP, egressHostV4IP, egressHostV6IP)

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
			reachAllServiceBackendsFromExternalContainer(externalContainerName, svcIP, podHTTPPort, pods)

			ginkgo.By("Updating the egress service to select the second node")
			secondNode := nodes[1].Name
			egressServiceConfig = fmt.Sprintf(`
apiVersion: k8s.ovn.org/v1
kind: EgressService
metadata:
  name: ` + serviceName + `
  namespace: ` + f.Namespace.Name + `
spec:
  nodeSelector:
    matchLabels:
      kubernetes.io/hostname: ` + secondNode + `
`)
			if err := os.WriteFile(egressServiceYAML, []byte(egressServiceConfig), 0644); err != nil {
				framework.Failf("Unable to write CRD config to disk: %v", err)
			}
			framework.RunKubectlOrDie(f.Namespace.Name, "apply", "-f", egressServiceYAML)

			ginkgo.By("Verifying the second node now handles the service's egress traffic")
			node, egressHostV4IP, egressHostV6IP = getEgressSVCHost(f.ClientSet, f.Namespace.Name, serviceName)
			framework.ExpectEqual(node.Name, secondNode, "the wrong node got selected for egress service")
			nodeList, err := f.ClientSet.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{LabelSelector: fmt.Sprintf("egress-service.k8s.ovn.org/%s-%s=", f.Namespace.Name, serviceName)})
			framework.ExpectNoError(err, "failed to list nodes")
			framework.ExpectEqual(len(nodeList.Items), 1, fmt.Sprintf("expected only one node labeled for the service, got %v", nodeList.Items))

			ginkgo.By("Setting the static route on the external container for the service via the second node's ip")
			setSVCRouteOnContainer(externalContainerName, svcIP, egressHostV4IP, egressHostV6IP)

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
			reachAllServiceBackendsFromExternalContainer(externalContainerName, svcIP, podHTTPPort, pods)

			ginkgo.By("Updating the egress service selector to match no node")
			egressServiceConfig = fmt.Sprintf(`
apiVersion: k8s.ovn.org/v1
kind: EgressService
metadata:
  name: ` + serviceName + `
  namespace: ` + f.Namespace.Name + `
spec:
  nodeSelector:
    matchLabels:
      perfect: match
`)
			if err := os.WriteFile(egressServiceYAML, []byte(egressServiceConfig), 0644); err != nil {
				framework.Failf("Unable to write CRD config to disk: %v", err)
			}
			framework.RunKubectlOrDie(f.Namespace.Name, "apply", "-f", egressServiceYAML)

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
			framework.ExpectEqual(node.Name, thirdNode, "the wrong node got selected for egress service")
			nodeList, err = f.ClientSet.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{LabelSelector: fmt.Sprintf("egress-service.k8s.ovn.org/%s-%s=", f.Namespace.Name, serviceName)})
			framework.ExpectNoError(err, "failed to list nodes")
			framework.ExpectEqual(len(nodeList.Items), 1, fmt.Sprintf("expected only one node labeled for the service, got %v", nodeList.Items))

			ginkgo.By("Setting the static route on the external container for the service via the third node's ip")
			setSVCRouteOnContainer(externalContainerName, svcIP, egressHostV4IP, egressHostV6IP)

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

			reachAllServiceBackendsFromExternalContainer(externalContainerName, svcIP, podHTTPPort, pods)
		},
		ginkgotable.Entry("ipv4 pods", v1.IPv4Protocol, &externalIPv4),
		ginkgotable.Entry("ipv6 pods", v1.IPv6Protocol, &externalIPv6),
	)

	ginkgotable.DescribeTable("Should validate egress service has higher priority than EgressIP when not assigned to the same node",
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
			framework.RunKubectlOrDie(f.Namespace.Name, "create", "-f", egressServiceYAML)
			svc := createLBServiceWithIngressIP(f.ClientSet, f.Namespace.Name, serviceName, protocol, labels, podHTTPPort)
			svcIP := svc.Status.LoadBalancer.Ingress[0].IP

			ginkgo.By("Getting the IPs of the node in charge of the service")
			_, egressHostV4IP, egressHostV6IP := getEgressSVCHost(f.ClientSet, f.Namespace.Name, serviceName)

			ginkgo.By("Setting the static route on the external container for the service via the egress host ip")
			setSVCRouteOnContainer(externalContainerName, svcIP, egressHostV4IP, egressHostV6IP)

			// Assign the egress IP without conflicting with any node IP,
			// the kind subnet is /16 or /64 so the following should be fine.
			ginkgo.By("Assigning the EgressIP to a different node")
			eipNode := nodes[0]
			framework.AddOrUpdateLabelOnNode(f.ClientSet, eipNode.Name, "k8s.ovn.org/egress-assignable", "dummy")
			defer func() {
				framework.RunKubectlOrDie("default", "label", "node", eipNode.Name, "k8s.ovn.org/egress-assignable-")
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
			framework.RunKubectlOrDie("default", "create", "-f", egressIPYaml)
			defer func() {
				framework.RunKubectlOrDie("default", "delete", "eip", "egress-svc-test-eip")
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
			reachAllServiceBackendsFromExternalContainer(externalContainerName, svcIP, podHTTPPort, pods)

			ginkgo.By("Deleting the EgressService the backend pods should exit with the EgressIP")
			framework.RunKubectlOrDie(f.Namespace.Name, "delete", "-f", egressServiceYAML)

			for _, pod := range pods {
				gomega.Eventually(func() error {
					return curlAgnHostClientIPFromPod(f.Namespace.Name, pod, egressIP.String(), *dstIP, podHTTPPort)
				}, 3*time.Second, 500*time.Millisecond).ShouldNot(gomega.HaveOccurred(), "failed to reach external container with eip")

				gomega.Consistently(func() error {
					return curlAgnHostClientIPFromPod(f.Namespace.Name, pod, egressIP.String(), *dstIP, podHTTPPort)
				}, 1*time.Second, 200*time.Millisecond).ShouldNot(gomega.HaveOccurred(), "failed to reach external container with eip")
			}
		},
		ginkgotable.Entry("ipv4 pods", v1.IPv4Protocol, &externalIPv4),
		ginkgotable.Entry("ipv6 pods", v1.IPv6Protocol, &externalIPv6),
	)

	ginkgotable.DescribeTable("Should validate a node with a local ep is selected when ETP=Local",
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
			framework.RunKubectlOrDie(f.Namespace.Name, "create", "-f", egressServiceYAML)
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
			framework.ExpectEqual(node.Name, firstNode, "the wrong node got selected for egress service")

			ginkgo.By("Setting the static route on the external container for the service via the first node's ip")
			setSVCRouteOnContainer(externalContainerName, svcIP, egressHostV4IP, egressHostV6IP)

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
		ginkgotable.Entry("ipv4 pods", v1.IPv4Protocol, &externalIPv4),
		ginkgotable.Entry("ipv6 pods", v1.IPv6Protocol, &externalIPv6),
	)
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
	egressServiceStdout, err := framework.RunKubectl(ns, "get", "egressservice", "-o", "json", name)
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
	curlCmd := fmt.Sprintf("curl -s --retry-connrefused --retry 3 --max-time 0.5 http://%s/clientip", dst)
	out, err := framework.RunHostCmd(namespace, pod, curlCmd)
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

// Tries to reach all of the backends of the given service from the container.
func reachAllServiceBackendsFromExternalContainer(container, svcIP string, svcPort int32, svcPods []string) {
	backends := map[string]bool{}
	for _, pod := range svcPods {
		backends[pod] = true
	}

	dst := net.JoinHostPort(svcIP, fmt.Sprint(svcPort))
	for i := 0; i < 10*len(svcPods); i++ {
		out, err := runCommand(containerRuntime, "exec", container, "curl", "-s", fmt.Sprintf("http://%s/hostname", dst))
		framework.ExpectNoError(err, "failed to curl service ingress IP")
		out = strings.ReplaceAll(out, "\n", "")
		delete(backends, out)
		if len(backends) == 0 {
			break
		}
	}

	framework.ExpectEqual(len(backends), 0, fmt.Sprintf("did not reach all pods from outside, missed: %v", backends))
}

// Sets the "dummy" custom routing table on all of the nodes (this heavily relies on the environment to be a kind cluster)
// We create a new routing table with 2 routes to the external container:
// 1) The one from the default routing table.
// 2) A blackhole with a higher priority
// Then in the actual test we first verify that when the pods are using the custom routing table they can't reach the external container,
// remove the blackhole route and verify that they can reach it now. This shows that they actually use a different routing table than the main one.
func setCustomRoutingTableOnNodes(nodes []v1.Node, routingTable, externalV4, externalV6 string, useV4 bool) {
	for _, node := range nodes {
		if useV4 {
			setRoutesOnCustomRoutingTable(node.Name, externalV4, routingTable)
			continue
		}
		if externalV6 != "" {
			setRoutesOnCustomRoutingTable(node.Name, externalV6, routingTable)
		}
	}
}

// Sets the regular+blackhole routes on the nodes to the external container.
func setRoutesOnCustomRoutingTable(container, ip, table string) {
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
	out, err = runCommand(containerRuntime, "exec", container, "ip", "route", "add", ip, "dev", routeTo.Dev, "table", table, "prio", "100")
	framework.ExpectNoError(err, fmt.Sprintf("failed to set route to %s on node %s table %s, out: %s", ip, container, table, out))

	out, err = runCommand(containerRuntime, "exec", container, "ip", "route", "add", "blackhole", ip, "table", table, "prio", "50")
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
func flushCustomRoutingTableOnNodes(nodes []v1.Node, routingTable string) {
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
