// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/allocators"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/images"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider"
	infraapi "github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/api"
	e2eendpointslice "k8s.io/kubernetes/test/e2e/framework/endpointslice"

	"github.com/onsi/ginkgo/extensions/table"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/feature"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/kubernetes/test/e2e/framework"
	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
	utilnet "k8s.io/utils/net"
)

// egressFirewallPolicyValidationTests registers EgressFirewall coverage for
// the primary network and, when requested, a user-defined network topology.
func egressFirewallPolicyValidationTests(useUDN bool, udnTopology string) {
	testLabel := "e2e egress firewall policy validation"
	if useUDN {
		testLabel = fmt.Sprintf("Network Segmentation: Egress Firewall [%s UDN]", strings.ToUpper(udnTopology))
	}

	// Validate the egress firewall policies by applying a policy and verify
	// that both explicitly allowed traffic and implicitly denied traffic
	// is properly handled as defined in the crd configuration in the test.
	ginkgo.Describe(testLabel, feature.EgressFirewall, func() {
		const (
			svcname       string = "egress-firewall-policy"
			testTimeout   int    = 3
			retryInterval        = 1 * time.Second
			retryTimeout         = 30 * time.Second
		)

		type nodeInfo struct {
			name   string
			nodeIP string
		}

		var (
			serverNodeInfo nodeInfo
			denyAllCIDR    string
			netConfig      networkAttachmentConfig
		)

		waitForEFApplied := func(namespace string) {
			gomega.Eventually(func() bool {
				status, err := e2ekubectl.RunKubectl(namespace, "get", "egressfirewall", "default",
					"-o", "jsonpath={.status.status}")
				if err != nil {
					framework.Failf("could not get egressfirewall %q in namespace %s: %v", "default", namespace, err)
				}
				if status == "EgressFirewall Rules applied" {
					return true
				}
				framework.Logf("egressfirewall %q in namespace %s is not applied. Status: %s", "default", namespace, status)
				return false
			}, 10*time.Second).Should(gomega.BeTrue(),
				fmt.Sprintf("expected egress firewall in namespace %s to be successfully applied", namespace))
		}

		applyEF := func(egressFirewallConfig, namespace string) {
			// write the config to a file for application and defer the removal
			ftmp, err := os.CreateTemp("", "egress-fw-udn-*.yml")
			if err != nil {
				framework.Failf("Unable to create temp file for CRD config: %v", err)
			}
			if _, err := ftmp.WriteString(egressFirewallConfig); err != nil {
				_ = ftmp.Close()
				framework.Failf("Unable to write CRD config to disk: %v", err)
			}
			if err := ftmp.Close(); err != nil {
				framework.Failf("Unable to close temp file for CRD config: %v", err)
			}
			defer func(name string) {
				if err := os.Remove(name); err != nil {
					framework.Logf("Unable to remove the CRD config %q from disk: %v", name, err)
				}
			}(ftmp.Name())
			// create the CRD config parameters
			applyArgs := []string{
				"apply",
				"--namespace=" + namespace,
				"-f",
				ftmp.Name(),
			}
			framework.Logf("Applying EgressFirewall configuration: %s ", applyArgs)
			// apply the egress firewall configuration
			e2ekubectl.RunKubectlOrDie(namespace, applyArgs...)
			waitForEFApplied(namespace)
		}

		f := wrappedTestFramework(svcname)

		// Determine what mode the CI is running in and get relevant endpoint information for the tests
		ginkgo.BeforeEach(func(ctx ginkgo.SpecContext) {
			nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.TODO(), f.ClientSet, 2)
			framework.ExpectNoError(err)
			if len(nodes.Items) < 2 {
				framework.Failf(
					"Test requires >= 2 Ready nodes, but there are only %v nodes",
					len(nodes.Items))
			}

			serverNode := nodes.Items[1]
			nodeIPs := e2enode.GetAddresses(&serverNode, v1.NodeInternalIP)
			if len(nodeIPs) == 0 {
				framework.Failf("Node %s has no InternalIP", serverNode.Name)
			}
			serverNodeInfo = nodeInfo{
				name:   serverNode.Name,
				nodeIP: nodeIPs[0],
			}

			denyAllCIDR = "0.0.0.0/0"
			if IsIPv6Cluster(f.ClientSet) {
				denyAllCIDR = "::/0"
			}

			if useUDN {
				f.SkipNamespaceCreation = true

				if !isNetworkSegmentationEnabled() {
					e2eskipper.Skipf("Skipping UDN tests: ENABLE_NETWORK_SEGMENTATION not set")
				}

				namespace, err := f.CreateNamespace(context.TODO(), f.BaseName, map[string]string{
					"e2e-framework":           f.BaseName,
					RequiredUDNNamespaceLabel: "",
				})
				f.Namespace = namespace
				framework.ExpectNoError(err)

				userDefinedNetworkIPv4Subnets, userDefinedNetworkIPv6Subnets := allocators.GetNthFirstUDNSubnets(1)
				cidr := joinStrings(joinStrings(userDefinedNetworkIPv4Subnets...), joinStrings(userDefinedNetworkIPv6Subnets...))
				if udnTopology == "layer3" {
					// For Layer 3, use multiple CIDRs per IP family. The first
					// CIDR is just big enough to allocate hostSubnets for the
					// first two nodes, remaining nodes will be allocated
					// hostSubnets from the second CIDR
					cidr = primaryLayer3MultiCIDRs()
				}

				nadCfg := networkAttachmentConfigParams{
					name:     "tenant-red",
					topology: udnTopology,
					cidr:     cidr,
					role:     "primary",
				}

				netConfig = newNetworkAttachmentConfig(nadCfg)
				netConfig.namespace = f.Namespace.Name
				switch strings.ToLower(udnTopology) {
				case "layer2":
					createLayer2PrimaryUDNWithSubnets(f.ClientSet, f.Namespace.Name, netConfig.name,
						userDefinedNetworkIPv4Subnets, userDefinedNetworkIPv6Subnets)
				case "layer3":
					createPrimaryUDNWithSubnets(f.ClientSet, f.Namespace.Name, netConfig.name, "Layer3",
						userDefinedNetworkIPv4Subnets, userDefinedNetworkIPv6Subnets)
				default:
					framework.Failf("unsupported UDN topology %q", udnTopology)
				}
				gomega.Eventually(ctx, userDefinedNetworkReadyFunc(ctx, f.DynamicClient, f.Namespace.Name, netConfig.name),
					30*time.Second, time.Second).Should(gomega.Succeed())
			}
		})

		ginkgo.Context("with external containers", func() {
			const (
				externalContainerName1 = "e2e-egress-fw-external-container1"
				externalContainerName2 = "e2e-egress-fw-external-container2"
			)

			var (
				singleIPMask, subnetMask string
				providerCtx              infraapi.Context
				externalContainer1       infraapi.ExternalContainer
				externalContainer2       infraapi.ExternalContainer
			)

			getExternalContainerIP := func(externalContainer infraapi.ExternalContainer) string {
				if IsIPv6Cluster(f.ClientSet) {
					return externalContainer.GetIPv6()
				}
				return externalContainer.GetIPv4()
			}

			checkConnectivity := func(srcPodName, dstIP string, dstPort string, shouldSucceed bool) {
				testContainer := fmt.Sprintf("%s-container", srcPodName)
				testContainerFlag := fmt.Sprintf("--container=%s", testContainer)
				if shouldSucceed {
					gomega.Eventually(func() bool {
						_, err := e2ekubectl.RunKubectl(f.Namespace.Name, "exec", srcPodName, testContainerFlag, "--",
							"curl", "-s", "--connect-timeout", fmt.Sprint(testTimeout), net.JoinHostPort(dstIP, dstPort))
						return err == nil
					}, time.Duration(2*testTimeout)*time.Second).Should(gomega.BeTrue(),
						fmt.Sprintf("expected connection from %s to [%s]:%s to succeed", srcPodName, dstIP, dstPort))
				} else {
					gomega.Consistently(func() bool {
						_, err := e2ekubectl.RunKubectl(f.Namespace.Name, "exec", srcPodName, testContainerFlag, "--",
							"curl", "-s", "--connect-timeout", fmt.Sprint(testTimeout), net.JoinHostPort(dstIP, dstPort))
						return err != nil
					}, time.Duration(2*testTimeout)*time.Second).Should(gomega.BeTrue(),
						fmt.Sprintf("expected connection from %s to [%s]:%s to fail", srcPodName, dstIP, dstPort))
				}
			}

			checkExternalContainerConnectivity := func(externalContainer infraapi.ExternalContainer, dstIP string, dstPort int) {
				gomega.Eventually(func() error {
					_, err := infraprovider.Get().ExecExternalContainerCommand(externalContainer, []string{
						"curl", "-s", "--connect-timeout", fmt.Sprint(time.Second), net.JoinHostPort(dstIP, fmt.Sprint(dstPort)),
					})
					return err
				}, time.Duration(2*testTimeout)*time.Second).Should(gomega.Succeed(),
					fmt.Sprintf("Failed to connect from external container %s to %s:%d",
						externalContainer.GetName(), dstIP, dstPort))
			}

			// createSrcPodWithRetry creates a pod that can reach the specified destination with a given number of retries.
			// In our e2e tests, a strange behaviour for ipv6 was seen: newly created pod can't reach ipv6 destination.
			// But if the same pod is re-created, everything works.
			// We don't know what causes that behaviour, so given function is a workaround for this issue.
			// It also only historically fails for the first ef test "Should validate the egress firewall policy functionality for allowed IP",
			// so only used there for now.
			createSrcPodWithRetry := func(retries int, reachableDst string, reachablePort string,
				podName, nodeName string, ipCheckInterval, ipCheckTimeout time.Duration, f *framework.Framework) {
				for i := 0; i < retries; i++ {
					createSrcPod(podName, nodeName, ipCheckInterval, ipCheckTimeout, f)
					testContainer := fmt.Sprintf("%s-container", podName)
					testContainerFlag := fmt.Sprintf("--container=%s", testContainer)
					for connectRetry := 0; connectRetry < 5; connectRetry++ {
						_, err := e2ekubectl.RunKubectl(f.Namespace.Name, "exec", podName, testContainerFlag, "--",
							"curl", "-s", "--connect-timeout", fmt.Sprint(testTimeout), net.JoinHostPort(reachableDst, reachablePort))
						if err == nil {
							return
						}
					}
					err := deletePodWithWaitByName(context.TODO(), f.ClientSet, podName, f.Namespace.Name)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
				}
				framework.Failf("Failed to create pod %s that can reach %s:%s after %d retries", podName, reachableDst, reachablePort, retries)
			}

			ginkgo.BeforeEach(func() {
				providerCtx = infraprovider.Get().NewTestContext()
				primaryProviderNetwork, err := infraprovider.Get().PrimaryNetwork()
				// container 1
				externalContainer1Port := infraprovider.Get().GetExternalContainerPort()
				externalContainer1Spec := infraapi.ExternalContainer{
					Name:    externalContainerName1,
					Image:   images.AgnHost(),
					Network: primaryProviderNetwork,
					CmdArgs: []string{"netexec", fmt.Sprintf("--http-port=%d", externalContainer1Port)},
					ExtPort: externalContainer1Port,
				}
				externalContainer1, err = providerCtx.CreateExternalContainer(externalContainer1Spec)
				framework.ExpectNoError(err, "must create external container 1")

				// container 2
				externalContainer2Port := infraprovider.Get().GetExternalContainerPort()
				externalContainer2Spec := infraapi.ExternalContainer{
					Name:    externalContainerName2,
					Image:   images.AgnHost(),
					Network: primaryProviderNetwork,
					CmdArgs: []string{"netexec", fmt.Sprintf("--http-port=%d", externalContainer2Port)},
					ExtPort: externalContainer2Port,
				}
				externalContainer2, err = providerCtx.CreateExternalContainer(externalContainer2Spec)
				framework.ExpectNoError(err, "must create external container 2")

				gomega.Eventually(func() bool {
					_, err := infraprovider.Get().ExecExternalContainerCommand(externalContainer1, []string{
						"curl", "-s", "--connect-timeout", fmt.Sprint(testTimeout), net.JoinHostPort(getExternalContainerIP(externalContainer2), fmt.Sprint(externalContainer2.GetPortStr())),
					})
					if err != nil {
						framework.Logf("Failed to connect to container 2 from container 1: %v", err)
						return false
					}
					_, err = infraprovider.Get().ExecExternalContainerCommand(externalContainer2, []string{
						"curl", "-s", "--connect-timeout", fmt.Sprint(testTimeout), net.JoinHostPort(getExternalContainerIP(externalContainer1), fmt.Sprint(externalContainer1.GetPortStr())),
					})
					if err != nil {
						framework.Logf("Failed to connect to container 1 from container 2: %v", err)
						return false
					}
					return true
				}, 10*time.Second, 500*time.Millisecond).Should(gomega.BeTrue(), "expected external containers %s to be connected")

				singleIPMask = "32"
				subnetMask = "24"
				if IsIPv6Cluster(f.ClientSet) {
					singleIPMask = "128"
					subnetMask = "64"
				}
			})

			ginkgo.It("Should validate the egress firewall policy functionality for allowed IP", func() {
				srcPodName := "e2e-egress-fw-src-pod"
				// create the pod that will be used as the source for the connectivity test
				createSrcPodWithRetry(3, getExternalContainerIP(externalContainer1), externalContainer1.GetPortStr(),
					srcPodName, serverNodeInfo.name, retryInterval, retryTimeout, f)

				// egress firewall crd yaml configuration
				var egressFirewallConfig = fmt.Sprintf(`kind: EgressFirewall
apiVersion: k8s.ovn.org/v1
metadata:
  name: default
  namespace: %s
spec:
  egress:
  - type: Allow
    to:
      cidrSelector: %s/%s
  - type: Deny
    to:
      cidrSelector: %s
`, f.Namespace.Name, getExternalContainerIP(externalContainer1), singleIPMask, denyAllCIDR)
				applyEF(egressFirewallConfig, f.Namespace.Name)

				// Verify the remote host/port as explicitly allowed by the firewall policy is reachable
				ginkgo.By(fmt.Sprintf("Verifying connectivity to an explicitly allowed host %s is permitted as defined "+
					"by the external firewall policy", getExternalContainerIP(externalContainer1)))
				checkConnectivity(srcPodName, getExternalContainerIP(externalContainer1), externalContainer1.GetPortStr(), true)

				// Verify the remote host/port as implicitly denied by the firewall policy is not reachable
				ginkgo.By(fmt.Sprintf("Verifying connectivity to an implicitly denied host %s is not permitted as defined "+
					"by the external firewall policy", getExternalContainerIP(externalContainer2)))
				checkConnectivity(srcPodName, getExternalContainerIP(externalContainer2), externalContainer2.GetPortStr(), false)
			})

			ginkgo.It("Should validate the egress firewall policy functionality for allowed CIDR and port", func() {
				srcPodName := "e2e-egress-fw-src-pod"
				createSrcPod(srcPodName, serverNodeInfo.name, retryInterval, retryTimeout, f)
				// egress firewall crd yaml configuration
				var egressFirewallConfig = fmt.Sprintf(`kind: EgressFirewall
apiVersion: k8s.ovn.org/v1
metadata:
  name: default
  namespace: %s
spec:
  egress:
  - type: Allow
    to:
      cidrSelector: %s/%s
    ports:
      - protocol: TCP
        port: %s
  - type: Deny
    to:
      cidrSelector: %s
`, f.Namespace.Name, getExternalContainerIP(externalContainer1), subnetMask, externalContainer1.GetPortStr(), denyAllCIDR)
				applyEF(egressFirewallConfig, f.Namespace.Name)

				// Verify the remote host/port as explicitly allowed by the firewall policy is reachable
				ginkgo.By(fmt.Sprintf("Verifying connectivity to an explicitly allowed port on host %s is permitted as "+
					"defined by the external firewall policy", getExternalContainerIP(externalContainer1)))
				checkConnectivity(srcPodName, getExternalContainerIP(externalContainer1), externalContainer1.GetPortStr(), true)

				// Verify the remote host/port as implicitly denied by the firewall policy is not reachable
				ginkgo.By(fmt.Sprintf("Verifying connectivity to an implicitly denied port on host %s is not permitted as "+
					"defined by the external firewall policy", getExternalContainerIP(externalContainer2)))
				checkConnectivity(srcPodName, getExternalContainerIP(externalContainer2), externalContainer2.GetPortStr(), false)
			})

			ginkgo.It("Should validate the egress firewall allows inbound connections", func() {
				// 1. Create nodePort service and external container
				// 2. Check connectivity works both ways
				// 3. Apply deny-all egress firewall
				// 4. Check only inbound traffic is allowed

				efPodName := "e2e-egress-fw-pod"
				var efPodPort uint16 = 1234
				serviceName := "service-for-pods"
				var servicePort uint16 = 31234

				ginkgo.By("Creating the egress firewall pod")
				// 1. create nodePort service and external container
				endpointsSelector := map[string]string{"servicebackend": "true"}
				_, err := createPod(f, efPodName, serverNodeInfo.name, f.Namespace.Name,
					[]string{"/agnhost", "netexec", fmt.Sprintf("--http-port=%d", efPodPort)}, endpointsSelector)
				if err != nil {
					framework.Failf("Failed to create pod %s: %v", efPodName, err)
				}

				ginkgo.By("Creating the nodePort service")
				_, err = createServiceForPodsWithLabel(f, f.Namespace.Name, servicePort, efPodPort, "NodePort", endpointsSelector)
				framework.ExpectNoError(err, fmt.Sprintf("unable to create nodePort service, err: %v", err))

				ginkgo.By("Waiting for the endpoints to pop up")
				err = e2eendpointslice.WaitForEndpointCount(context.TODO(), f.ClientSet, f.Namespace.Name, serviceName, 1)
				framework.ExpectNoError(err, "failed to validate endpoints for service %s in namespace: %s", serviceName, f.Namespace.Name)

				// 2. Check connectivity works both ways
				// pod -> external container should work
				ginkgo.By(fmt.Sprintf("Verifying connectivity from pod %s to external container [%s]:%s",
					efPodName, getExternalContainerIP(externalContainer1), externalContainer1.GetPortStr()))
				checkConnectivity(efPodName, getExternalContainerIP(externalContainer1), externalContainer1.GetPortStr(), true)

				// external container -> nodePort svc should work
				svc, err := f.ClientSet.CoreV1().Services(f.Namespace.Name).Get(context.TODO(), serviceName, metav1.GetOptions{})
				framework.ExpectNoError(err, "failed to fetch service: %s in namespace %s", serviceName, f.Namespace.Name)

				nodeIP := serverNodeInfo.nodeIP
				ginkgo.By(fmt.Sprintf("Verifying connectivity from external container %s to nodePort svc [%s]:%d",
					getExternalContainerIP(externalContainer1), nodeIP, svc.Spec.Ports[0].NodePort))
				checkExternalContainerConnectivity(externalContainer1, nodeIP, int(svc.Spec.Ports[0].NodePort))

				// 3. Apply deny-all egress firewall and wait for it to be applied
				var egressFirewallConfig = fmt.Sprintf(`kind: EgressFirewall
apiVersion: k8s.ovn.org/v1
metadata:
  name: default
  namespace: %s
spec:
  egress:
  - type: Deny
    to:
      cidrSelector: %s
`, f.Namespace.Name, denyAllCIDR)
				applyEF(egressFirewallConfig, f.Namespace.Name)

				// 4. Check that only inbound traffic is allowed
				// pod -> external container should be blocked
				ginkgo.By(fmt.Sprintf("Verifying connectivity from pod %s to external container [%s]:%s is blocked",
					efPodName, getExternalContainerIP(externalContainer1), externalContainer1.GetPortStr()))
				checkConnectivity(efPodName, getExternalContainerIP(externalContainer1), externalContainer1.GetPortStr(), false)

				// external container -> nodePort svc should work
				ginkgo.By(fmt.Sprintf("Verifying connectivity from external container %s to nodePort svc [%s]:%d",
					getExternalContainerIP(externalContainer1), nodeIP, svc.Spec.Ports[0].NodePort))
				checkExternalContainerConnectivity(externalContainer1, nodeIP, int(svc.Spec.Ports[0].NodePort))
			})

			ginkgo.It("Should validate the egress firewall doesn't affect internal connections", func() {
				srcPodName := "e2e-egress-fw-src-pod"
				dstPodName := "e2e-egress-fw-dst-pod"
				dstPort := "1234"
				createSrcPod(srcPodName, serverNodeInfo.name, retryInterval, retryTimeout, f)
				// egress firewall crd yaml configuration
				var egressFirewallConfig = fmt.Sprintf(`kind: EgressFirewall
apiVersion: k8s.ovn.org/v1
metadata:
  name: default
  namespace: %s
spec:
  egress:
  - type: Deny
    to:
      cidrSelector: %s
`, f.Namespace.Name, denyAllCIDR)
				applyEF(egressFirewallConfig, f.Namespace.Name)

				// create dst pod
				dstPod, err := createPod(f, dstPodName, serverNodeInfo.name, f.Namespace.Name,
					[]string{"/agnhost", "netexec", fmt.Sprintf("--http-port=%s", dstPort)}, nil)
				if err != nil {
					framework.Failf("Failed to create dst pod %s: %v", dstPodName, err)
				}
				dstPodIP := dstPod.Status.PodIP
				if strings.HasPrefix(testLabel, "Network Segmentation") {
					dstPodIP, err = getPodAnnotationIPsForAttachmentByIndex(
						f.ClientSet,
						f.Namespace.Name,
						dstPod.Name,
						namespacedName(f.Namespace.Name, netConfig.name),
						0,
					)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
				}

				ginkgo.By(fmt.Sprintf("Verifying connectivity to an internal pod %s is permitted", dstPodName))
				checkConnectivity(srcPodName, dstPodIP, dstPort, true)

				ginkgo.By(fmt.Sprintf("Verifying connectivity to an external host %s is not permitted as defined "+
					"by the external firewall policy", getExternalContainerIP(externalContainer1)))
				checkConnectivity(srcPodName, getExternalContainerIP(externalContainer1), externalContainer1.GetPortStr(), false)
			})

			ginkgo.It("Should validate that egressfirewall supports DNS name in caps", func() {
				srcPodName := "e2e-egress-fw-src-pod"
				createSrcPod(srcPodName, serverNodeInfo.name, retryInterval, retryTimeout, f)

				// egress firewall crd yaml configuration
				var egressFirewallConfig = fmt.Sprintf(`kind: EgressFirewall
apiVersion: k8s.ovn.org/v1
metadata:
  name: default
  namespace: %s
spec:
  egress:
  - type: Allow
    to:
      dnsName: WWW.TEST.COM
  - type: Deny
    to:
      cidrSelector: %s
`, f.Namespace.Name, denyAllCIDR)
				applyEF(egressFirewallConfig, f.Namespace.Name)
				framework.Logf("Deleting EgressFirewall in namespace %s", f.Namespace.Name)
				e2ekubectl.RunKubectlOrDie(f.Namespace.Name, "delete", "egressfirewall", "default")
			})
		})

		table.DescribeTable("Should validate the egress firewall policy functionality against cluster nodes by using node selector",
			func(chaosTesting bool) {
				if chaosTesting {
					// apply egressfirewall with many dns names, then delete and check that the next node-selector egress firewall
					// is handled correctly.
					// Using node selector is the best way to check internal egress firewall locking, as node event handler
					// iterates over all existing egress firewalls.
					var egressFirewallConfig = fmt.Sprintf(`kind: EgressFirewall
apiVersion: k8s.ovn.org/v1
metadata:
  name: default
  namespace: %s
spec:
  egress:
  - type: Allow
    to:
      dnsName: www.test1.com
  - type: Allow
    to:
      dnsName: www.test2.com
  - type: Allow
    to:
      dnsName: www.test3.com
  - type: Allow
    to:
      dnsName: www.test4.com
  - type: Allow
    to:
      dnsName: www.test5.com
  - type: Allow
    to:
      dnsName: www.test6.com
  - type: Allow
    to:
      dnsName: www.test7.com
  - type: Allow
    to:
      dnsName: www.test8.com
  - type: Allow
    to:
      dnsName: www.test9.com
  - type: Allow
    to:
      dnsName: www.test10.com
  - type: Allow
    to:
      dnsName: www.test11.com
  - type: Allow
    to:
      dnsName: www.test12.com
    ports:
      - protocol: TCP
        port: 80
  - type: Deny
    to:
      cidrSelector: %s
`, f.Namespace.Name, denyAllCIDR)
					applyEF(egressFirewallConfig, f.Namespace.Name)
					framework.Logf("Deleting EgressFirewall in namespace %s", f.Namespace.Name)
					e2ekubectl.RunKubectlOrDie(f.Namespace.Name, "delete", "egressfirewall", "default")
				}

				srcPodName := "e2e-egress-fw-src-pod"
				testContainer := fmt.Sprintf("%s-container", srcPodName)
				testContainerFlag := fmt.Sprintf("--container=%s", testContainer)
				// use random labels in case test runs again since it's a pain to remove the label from the node
				labelMatch := randStr(5)
				// egress firewall crd yaml configuration
				var egressFirewallConfig = fmt.Sprintf(`kind: EgressFirewall
apiVersion: k8s.ovn.org/v1
metadata:
  name: default
  namespace: %s
spec:
  egress:
  - type: Allow
    to:
      nodeSelector:
        matchLabels:
          %s: %s
  - type: Deny
    to:
      cidrSelector: %s
`, f.Namespace.Name, f.Namespace.Name, labelMatch, denyAllCIDR)
				framework.Logf("Egress Firewall CR generated: %s", egressFirewallConfig)

				createSrcPod(srcPodName, serverNodeInfo.name, retryInterval, retryTimeout, f)
				applyEF(egressFirewallConfig, f.Namespace.Name)
				// create host networked pod
				nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.TODO(), f.ClientSet, 3)
				framework.ExpectNoError(err)

				if len(nodes.Items) < 3 {
					framework.Failf(
						"Test requires >= 3 Ready nodes, but there are only %v nodes",
						len(nodes.Items))
				}
				ginkgo.By("Creating host network pods on each node")
				// get random port in case the test retries and port is already in use on host node
				minPort := 9900
				maxPort := 9999
				hostNetworkPort := rand.Intn(maxPort-minPort+1) + minPort
				framework.Logf("Random host networked port chosen: %d", hostNetworkPort)
				for _, node := range nodes.Items {
					// this creates a udp / http netexec listener which is able to receive the "hostname"
					// command. We use this to validate that each endpoint is received at least once
					args := []string{
						"netexec",
						fmt.Sprintf("--http-port=%d", hostNetworkPort),
						fmt.Sprintf("--udp-port=%d", hostNetworkPort),
					}

					// create host networked Pods
					_, err := createPod(f, node.Name+"-hostnet-ep", node.Name, f.Namespace.Name, []string{}, map[string]string{}, func(p *v1.Pod) {
						p.Spec.Containers[0].Args = args
						p.Spec.HostNetwork = true
					})

					framework.ExpectNoError(err)
				}

				ginkgo.By("Selecting additional IP addresses for serverNode on which source pod lives (networking routing to secondaryIP address on other nodes is harder to achieve)")
				// add new secondary IP from node subnet to the node where the source pod lives on,
				// if the cluster is v6 add an ipv6 address
				toCurlSecondaryNodeIPAddresses := sets.New[string]()
				// node2ndaryIPs holds the nodeName as the key and the value is
				// a map with ipFamily(v4 or v6) as the key and the secondaryIP as the value
				node2ndaryIPs := make(map[string]map[int]string)
				var newIP string
				if node2ndaryIPs[serverNodeInfo.name] == nil {
					node2ndaryIPs[serverNodeInfo.name] = make(map[int]string)
				}
				if utilnet.IsIPv6String(e2enode.GetAddresses(&nodes.Items[1], v1.NodeInternalIP)[0]) {
					newIP = "fc00:f853:ccd:e794::" + strconv.Itoa(12)
					framework.Logf("Secondary nodeIP %s for node %s", newIP, serverNodeInfo.name)
					node2ndaryIPs[serverNodeInfo.name][6] = newIP
				} else {
					newIP = "172.18.1." + strconv.Itoa(13)
					framework.Logf("Secondary nodeIP %s for node %s", newIP, serverNodeInfo.name)
					node2ndaryIPs[serverNodeInfo.name][4] = newIP
				}

				ginkgo.By("Adding additional IP addresses to node on which source pod lives")
				for nodeName, ipFamilies := range node2ndaryIPs {
					for _, ip := range ipFamilies {
						// manually add the a secondary IP to each node
						framework.Logf("Adding IP %s to node %s", ip, nodeName)
						_, err = infraprovider.Get().ExecK8NodeCommand(nodeName, []string{
							"ip", "addr", "add", ip, "dev", deploymentconfig.Get().PrimaryInterfaceName(),
						})
						if err != nil && !strings.Contains(err.Error(), "Address already assigned") {
							framework.Failf("failed to add new IP address %s to node %s: %v", ip, nodeName, err)
						}
						ginkgo.DeferCleanup(func() error {
							_, err = infraprovider.Get().ExecK8NodeCommand(nodeName, []string{"ip", "addr", "delete", ip, "dev", deploymentconfig.Get().PrimaryInterfaceName()})
							return err
						})
						toCurlSecondaryNodeIPAddresses.Insert(ip)
					}
				}

				ginkgo.By("Should NOT be able to reach each host networked pod via node selector")
				hostNetworkPortStr := fmt.Sprint(hostNetworkPort)
				for _, node := range nodes.Items {
					nodeIPs := e2enode.GetAddresses(&node, v1.NodeInternalIP)
					if len(nodeIPs) == 0 {
						framework.Failf("node %q has no InternalIP", node.Name)
					}
					path := fmt.Sprintf("http://%s/hostname", net.JoinHostPort(nodeIPs[0], hostNetworkPortStr))
					_, err = e2ekubectl.RunKubectl(f.Namespace.Name, "exec", srcPodName, testContainerFlag, "--", "curl", "-g", "--max-time", "2", path)
					if err == nil {
						framework.Failf("Was able to curl node %s from container %s on node %s with no allow rule for egress firewall", node.Name, srcPodName, serverNodeInfo.name)
					}
				}

				ginkgo.By("Should NOT be able to reach each secondary hostIP via node selector")
				for _, address := range sets.List(toCurlSecondaryNodeIPAddresses) {
					if !IsIPv6Cluster(f.ClientSet) && utilnet.IsIPv6String(address) || IsIPv6Cluster(f.ClientSet) && !utilnet.IsIPv6String(address) {
						continue
					}
					path := fmt.Sprintf("http://%s/hostname", net.JoinHostPort(address, hostNetworkPortStr))
					_, err = e2ekubectl.RunKubectl(f.Namespace.Name, "exec", srcPodName, testContainerFlag, "--", "curl", "-g", "--max-time", "2", path)
					if err == nil {
						framework.Failf("Was able to curl node %s from container %s on nodeIP %s with no allow rule for egress firewall", address, srcPodName, serverNodeInfo.name)
					}
				}

				ginkgo.By("Apply label to nodes " + f.Namespace.Name + ":" + labelMatch)
				patch := struct {
					Metadata map[string]interface{} `json:"metadata"`
				}{
					Metadata: map[string]interface{}{
						"labels": map[string]string{f.Namespace.Name: labelMatch},
					},
				}
				for _, node := range nodes.Items {
					patchData, err := json.Marshal(&patch)
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
					_, err = f.ClientSet.CoreV1().Nodes().Patch(context.TODO(), node.Name, types.MergePatchType, patchData, metav1.PatchOptions{})
					gomega.Expect(err).NotTo(gomega.HaveOccurred())
				}

				ginkgo.By("Should be able to reach each host networked pod via node selector")
				for _, node := range nodes.Items {
					nodeIPs := e2enode.GetAddresses(&node, v1.NodeInternalIP)
					if len(nodeIPs) == 0 {
						framework.Failf("node %q has no InternalIP", node.Name)
					}
					path := fmt.Sprintf("http://%s/hostname", net.JoinHostPort(nodeIPs[0], hostNetworkPortStr))
					_, err = e2ekubectl.RunKubectl(f.Namespace.Name, "exec", srcPodName, testContainerFlag, "--", "curl", "-g", "--max-time", "2", path)
					if err != nil {
						framework.Failf("Failed to curl node %s from container %s on node %s: %v", node.Name, srcPodName, serverNodeInfo.name, err)
					}
				}

				ginkgo.By("Should be able to reach secondary hostIP via node selector")
				for _, address := range sets.List(toCurlSecondaryNodeIPAddresses) {
					if !IsIPv6Cluster(f.ClientSet) && utilnet.IsIPv6String(address) || IsIPv6Cluster(f.ClientSet) && !utilnet.IsIPv6String(address) {
						continue
					}
					path := fmt.Sprintf("http://%s/hostname", net.JoinHostPort(address, hostNetworkPortStr))
					_, err = e2ekubectl.RunKubectl(f.Namespace.Name, "exec", srcPodName, testContainerFlag, "--", "curl", "-g", "--max-time", "2", path)
					if err != nil {
						framework.Failf("Failed to curl node %s from container %s on nodeIP %s", address, srcPodName, serverNodeInfo.name)
					}
				}
			},
			table.Entry("", false),
			table.Entry("with chaos testing using many dnsNames", true),
		)

		ginkgo.Context("with DNS name resolver", func() {
			ginkgo.BeforeEach(func() {
				// DNS resolution for external DNS names does not work on IPv6 clusters. Skip
				// the test if DNS name resolver is not enabled or IPv4 is not supported.
				if !isDNSNameResolverEnabled() {
					e2eskipper.Skipf("DNS name resolver is not enabled")
					return
				}
				if !isIPv4Supported(f.ClientSet) {
					e2eskipper.Skipf("IPv4 is not supported")
					return
				}
			})

			ginkgo.It("Should validate that egressfirewall policy functionality for allowed DNS name", func() {
				dnsName := "www.google.com"
				srcPodName := "e2e-egress-fw-src-pod"
				createSrcPod(srcPodName, serverNodeInfo.name, retryInterval, retryTimeout, f)

				// egress firewall crd yaml configuration
				var egressFirewallConfig = fmt.Sprintf(`kind: EgressFirewall
apiVersion: k8s.ovn.org/v1
metadata:
  name: default
  namespace: %s
spec:
  egress:
  - type: Allow
    to:
      dnsName: %s
  - type: Deny
    to:
      cidrSelector: %s
`, f.Namespace.Name, dnsName, denyAllCIDR)
				applyEF(egressFirewallConfig, f.Namespace.Name)

				ginkgo.By(fmt.Sprintf("Verifying connectivity to DNS name %s is permitted", dnsName))
				url := fmt.Sprintf("https://%s", dnsName)
				_, err := e2ekubectl.RunKubectl(f.Namespace.Name, "exec", srcPodName, "--", "curl", "-g", "--max-time", "5", url)
				framework.ExpectNoError(err, "failed to curl DNS name %s", dnsName)

				ginkgo.By(fmt.Sprintf("Getting the minimum TTL for DNS name %s", dnsName))
				minTTL := getMinTTLForDNSName(f, dnsName, srcPodName)
				gomega.Expect(minTTL).NotTo(gomega.Equal(-1), "failed to parse nslookup output for DNS name %s", dnsName)
				framework.Logf("Minimum TTL for DNS name %s is %d", dnsName, minTTL)

				ginkgo.By(fmt.Sprintf("Waiting for the minimum TTL + 5 seconds for IP addresses of DNS name %s to be refreshed", dnsName))
				time.Sleep(time.Duration(minTTL+5) * time.Second)

				ginkgo.By(fmt.Sprintf("Verifying connectivity to DNS name %s is still permitted", dnsName))
				_, err = e2ekubectl.RunKubectl(f.Namespace.Name, "exec", srcPodName, "--", "curl", "-g", "--max-time", "5", url)
				framework.ExpectNoError(err, "failed to curl DNS name %s after TTL refresh", dnsName)

				framework.Logf("Deleting EgressFirewall in namespace %s", f.Namespace.Name)
				e2ekubectl.RunKubectlOrDie(f.Namespace.Name, "delete", "egressfirewall", "default")
			})
		})

		ginkgo.Context("with legacy DNS name resolver", func() {
			ginkgo.BeforeEach(func() {
				// This recovery path belongs to the legacy resolver. The feature flag
				// selects the separate resolver implementation at controller startup.
				if isDNSNameResolverEnabled() {
					e2eskipper.Skipf("legacy DNS name resolver is not enabled")
					return
				}
				if !isIPv4Supported(f.ClientSet) {
					e2eskipper.Skipf("IPv4 is not supported")
				}
			})

			ginkgo.It("recovers an EgressFirewall DNS address set after it is deleted", func() {
				dnsName := "www.google.com"
				srcPodName := "e2e-egress-fw-src-pod"
				createSrcPod(srcPodName, serverNodeInfo.name, retryInterval, retryTimeout, f)
				ovnNamespace := deploymentconfig.Get().OVNKubernetesNamespace()
				nbDBPod, err := findOVNNBDBPodForNode(f.ClientSet, ovnNamespace, serverNodeInfo.name)
				framework.ExpectNoError(err, "failed to find the NB database pod for source node %s", serverNodeInfo.name)
				// Read the DNS TTL before applying the policy. DNS lookups from a
				// UDN pod may be denied by the EgressFirewall; the connectivity
				// checks below use an address from the OVN AddressSet directly.
				minTTL := getMinTTLForDNSName(f, dnsName, srcPodName)
				// The EgressDNS controller uses the node's DNS resolver, which can
				// return a longer TTL than the test pod's resolver observed here.
				// Keep enough time for that controller-side TTL before declaring
				// address-set recovery broken.
				refreshTimeout := 10 * time.Minute
				if minTTL >= 0 {
					observedRefreshTimeout := time.Duration(minTTL+30) * time.Second
					if observedRefreshTimeout > refreshTimeout {
						refreshTimeout = observedRefreshTimeout
					}
					framework.Logf("Minimum observed TTL for DNS name %s is %d", dnsName, minTTL)
				} else {
					framework.Logf("Could not read TTL for %s before applying the EgressFirewall; using a %s refresh timeout",
						dnsName, refreshTimeout)
				}

				egressFirewallConfig := fmt.Sprintf(`kind: EgressFirewall
apiVersion: k8s.ovn.org/v1
metadata:
  name: default
  namespace: %s
spec:
  egress:
  - type: Allow
    to:
      dnsName: %s
  - type: Deny
    to:
      cidrSelector: %s
`, f.Namespace.Name, dnsName, denyAllCIDR)
				applyEF(egressFirewallConfig, f.Namespace.Name)

				url := fmt.Sprintf("https://%s", dnsName)
				curlToAddressSetIPs := func(ips []string) error {
					if len(ips) == 0 {
						return fmt.Errorf("DNS address set for %s has no IP addresses yet", dnsName)
					}
					var lastErr error
					for _, ip := range ips {
						resolveIP := ip
						if parsedIP := net.ParseIP(ip); parsedIP != nil && parsedIP.To4() == nil {
							resolveIP = "[" + parsedIP.String() + "]"
						}
						_, curlErr := e2ekubectl.RunKubectl(f.Namespace.Name, "exec", srcPodName, "--",
							"curl", "-g", "--max-time", "5", "--resolve",
							fmt.Sprintf("%s:443:%s", dnsName, resolveIP), url)
						if curlErr == nil {
							return nil
						}
						lastErr = curlErr
					}
					return fmt.Errorf("curl to %s using AddressSet IP addresses %v failed: %w", url, ips, lastErr)
				}

				ginkgo.By(fmt.Sprintf("Waiting for the legacy resolver to populate the address set for %s", dnsName))
				gomega.Eventually(func() error {
					addressSetIPs, lookupErr := getEgressFirewallDNSAddressSetIPs(f, nbDBPod.Name, dnsName)
					if lookupErr != nil {
						return fmt.Errorf("failed to find DNS address set addresses for %s: %w", dnsName, lookupErr)
					}
					if len(addressSetIPs) == 0 {
						return fmt.Errorf("DNS address set for %s has no IP addresses yet", dnsName)
					}
					curlErr := curlToAddressSetIPs(addressSetIPs)
					if curlErr != nil {
						return fmt.Errorf("curl to %s failed while waiting for AddressSet connectivity: %w", url, curlErr)
					}
					return nil
				}, retryTimeout, retryInterval).Should(gomega.Succeed(),
					"connectivity to DNS name %s was not permitted after the EgressFirewall was applied", dnsName)

				ginkgo.By(fmt.Sprintf("Removing the OVN address set for DNS name %s", dnsName))
				addressSetUUIDs, err := getEgressFirewallDNSAddressSetUUIDs(f, nbDBPod.Name, dnsName)
				framework.ExpectNoError(err, "failed to find DNS address sets for %s", dnsName)
				gomega.Expect(addressSetUUIDs).NotTo(gomega.BeEmpty(),
					"expected at least one DNS address set for %s", dnsName)
				for _, uuid := range addressSetUUIDs {
					_, err = runOVNNBCTLInPod(f, ovnNamespace, nbDBPod.Name, "--if-exists", "destroy", "address_set", uuid)
					framework.ExpectNoError(err, "failed to delete DNS address set %s for %s", uuid, dnsName)
				}
				gomega.Eventually(func() error {
					addressSetUUIDs, err := getEgressFirewallDNSAddressSetUUIDs(f, nbDBPod.Name, dnsName)
					if err != nil {
						return fmt.Errorf("failed to verify DNS address sets were deleted: %w", err)
					}
					if len(addressSetUUIDs) != 0 {
						return fmt.Errorf("DNS address sets still exist after deletion: %v", addressSetUUIDs)
					}
					return nil
				}, 30*time.Second, 5*time.Second).Should(gomega.Succeed(),
					"DNS address sets for %s were not deleted", dnsName)

				ginkgo.By(fmt.Sprintf("Waiting for the legacy DNS refresh to recreate the address set for %s", dnsName))
				gomega.Eventually(func() error {
					addressSetIPs, err := getEgressFirewallDNSAddressSetIPs(f, nbDBPod.Name, dnsName)
					if err != nil {
						return fmt.Errorf("failed to find recreated DNS address set addresses: %w", err)
					}
					if len(addressSetIPs) == 0 {
						return fmt.Errorf("DNS address set for %s has no IP addresses after refresh", dnsName)
					}
					return curlToAddressSetIPs(addressSetIPs)
				}, refreshTimeout, 5*time.Second).Should(gomega.Succeed(),
					"DNS address set for %s was not recreated after refresh", dnsName)

				ginkgo.By(fmt.Sprintf("Verifying connectivity to DNS name %s is restored", dnsName))
				gomega.Eventually(func() error {
					addressSetIPs, lookupErr := getEgressFirewallDNSAddressSetIPs(f, nbDBPod.Name, dnsName)
					if lookupErr != nil {
						return fmt.Errorf("failed to find DNS address set addresses: %w", lookupErr)
					}
					return curlToAddressSetIPs(addressSetIPs)
				}, refreshTimeout, 5*time.Second).Should(gomega.Succeed(),
					"connectivity to DNS name %s was not restored after address set recovery", dnsName)

				framework.Logf("Deleting EgressFirewall in namespace %s", f.Namespace.Name)
				e2ekubectl.RunKubectlOrDie(f.Namespace.Name, "delete", "egressfirewall", "default")
			})
		})
	})
}

// getEgressFirewallDNSAddressSetUUIDs returns the legacy resolver's address
// set UUIDs for dnsName from the NB pod serving the source node's zone.
func getEgressFirewallDNSAddressSetUUIDs(f *framework.Framework, nbDBPodName, dnsName string) ([]string, error) {
	output, err := runOVNNBCTLInPod(f, deploymentconfig.Get().OVNKubernetesNamespace(), nbDBPodName,
		"--data=bare", "--no-heading", "--columns=_uuid",
		"find", "address_set",
		"external_ids:k8s.ovn.org/owner-type=EgressFirewallDNS",
		fmt.Sprintf("external_ids:k8s.ovn.org/name=%s", dnsName),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to query DNS address sets for %s: %w", dnsName, err)
	}
	return strings.Fields(output), nil
}

// getEgressFirewallDNSAddressSetIPs returns the legacy resolver's current
// addresses for dnsName from the NB pod serving the source node's zone.
func getEgressFirewallDNSAddressSetIPs(f *framework.Framework, nbDBPodName, dnsName string) ([]string, error) {
	output, err := runOVNNBCTLInPod(f, deploymentconfig.Get().OVNKubernetesNamespace(), nbDBPodName,
		"--data=bare", "--no-heading", "--columns=addresses",
		"find", "address_set",
		"external_ids:k8s.ovn.org/owner-type=EgressFirewallDNS",
		fmt.Sprintf("external_ids:k8s.ovn.org/name=%s", dnsName),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to query DNS address-set addresses for %s: %w", dnsName, err)
	}

	var ips []string
	seen := make(map[string]struct{})
	for _, token := range strings.FieldsFunc(output, func(r rune) bool {
		return strings.ContainsRune("{}[],\"' \t\r\n", r)
	}) {
		ip := net.ParseIP(token)
		if ip == nil {
			continue
		}
		canonicalIP := ip.String()
		if ipv4 := ip.To4(); ipv4 != nil {
			canonicalIP = ipv4.String()
		}
		if _, ok := seen[canonicalIP]; ok {
			continue
		}
		seen[canonicalIP] = struct{}{}
		ips = append(ips, canonicalIP)
	}
	return ips, nil
}

// getMinTTLForDNSName returns the minimum DNS answer TTL observed from the
// source pod.
func getMinTTLForDNSName(f *framework.Framework, dnsName, srcPodName string) int {
	nslookupOutput, _ := e2ekubectl.RunKubectl(f.Namespace.Name, "exec", srcPodName, "--", "nslookup", "-debug", "-timeout=2", dnsName)
	lines := strings.Split(nslookupOutput, "\n")
	minTTL := -1
	for i := 0; i < len(lines); i++ {
		answerLine := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(answerLine, fmt.Sprintf("->  %s", dnsName)) {
			continue
		}
		for i++; i < len(lines); i++ {
			ttlLine := strings.TrimSpace(lines[i])
			if !strings.HasPrefix(ttlLine, "ttl =") {
				continue
			}
			ttlParts := strings.Split(ttlLine, "ttl =")
			if len(ttlParts) != 2 {
				break
			}
			ttl, err := strconv.Atoi(strings.TrimSpace(ttlParts[1]))
			gomega.Expect(err).NotTo(gomega.HaveOccurred(), "failed to parse DNS TTL: %v", err)
			if minTTL == -1 || ttl < minTTL {
				minTTL = ttl
			}
			break
		}
	}
	return minTTL
}

func init() {
	// Always register the standard EgressFirewall test suite
	egressFirewallPolicyValidationTests(false, "")

	// Conditionally register Network Segmentation variants for CI
	if isNetworkSegmentationEnabled() {
		ginkgo.Describe("Network Segmentation: Egress Firewall", feature.NetworkSegmentation, func() {
			// Run both L3 and L2 UDN variants
			egressFirewallPolicyValidationTests(true, "layer3")
			egressFirewallPolicyValidationTests(true, "layer2")
		})
	}
}
