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
	"time"

	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/deploymentconfig"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/feature"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/images"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"golang.org/x/sync/errgroup"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/kubernetes/test/e2e/framework"
	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	utilnet "k8s.io/utils/net"
	"k8s.io/utils/ptr"
)

var _ = ginkgo.Describe("e2e NetworkQoS validation", feature.NetworkQos, func() {
	const (
		networkQoSYaml = "networkqos.yaml"
		nqosSpecName   = "nqos-test-spec"
		srcPodName     = "src-nqos-pod"
		tcpdumpIPv4    = "(ip and (ip[1] & 0xfc) >> 2 == %d)"
		tcpdumpIPv6    = "(ip6 and (ip6[0:2] & 0xfc0) >> 6 == %d)"
		dstPod1Name    = "nqos-dst-pod1"
		dstPod2Name    = "nqos-dst-pod2"
		dstPod3Name    = "nqos-dst-pod3"
		dstPod4Name    = "nqos-dst-pod4"

		bandwidthFluctuation = 1.5
	)

	var (
		skipIpv4        bool
		skipIpv6        bool
		dstPodNamespace string
		srcNode         string
		dstNode         string
		dstPod1IPv4     string
		dstPod1IPv6     string
		dstPod2IPv4     string
		dstPod2IPv6     string
		dstPod3IPv4     string
		dstPod3IPv6     string
		dstPod4IPv4     string
		dstPod4IPv6     string
		nodeIPv4Range   string
		nodeIPv6Range   string
	)

	f := wrappedTestFramework("networkqos")

	waitForNetworkQoSApplied := func(namespace string, rules int) {
		waitForNetworkQoSAppliedOnNode(namespace, nqosSpecName, srcNode, rules)
	}

	ginkgo.BeforeEach(func() {
		nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.TODO(), f.ClientSet, 2)
		framework.ExpectNoError(err)
		if len(nodes.Items) < 2 {
			framework.Failf("Test requires >= 2 Ready nodes, but there are only %v nodes", len(nodes.Items))
		}
		nodeAddresses := map[string]string{}
		err = json.Unmarshal([]byte(nodes.Items[0].Annotations["k8s.ovn.org/node-primary-ifaddr"]), &nodeAddresses)
		framework.ExpectNoError(err)
		if nodeIP, ok := nodeAddresses["ipv4"]; ok {
			_, ipnet, _ := net.ParseCIDR(nodeIP)
			nodeIPv4Range = ipnet.String()
			skipIpv4 = false
		} else {
			ginkgo.By("Node IPv4 address not found: Will be skipping IPv4 checks in the Networking QoS test")
			nodeIPv4Range = "0.0.0.0/0"
			skipIpv4 = true
		}
		if nodeIP, ok := nodeAddresses["ipv6"]; ok {
			_, ipnet, _ := net.ParseCIDR(nodeIP)
			nodeIPv6Range = ipnet.String()
			skipIpv6 = false
		} else {
			ginkgo.By("Node IPv6 address not found: Will be skipping IPv6 checks in the Networking QoS test")
			nodeIPv6Range = "::/0"
			skipIpv6 = true
		}
		if skipIpv4 && skipIpv6 {
			framework.Fail("Neither IPv4 nor IPv6 is configured on the node")
		}
		dstPodNamespace = f.Namespace.Name + "-dest"
		// set up dest namespace
		dstNs := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: dstPodNamespace,
				Labels: map[string]string{
					"app": "nqos-test",
				},
			},
		}
		_, err = f.ClientSet.CoreV1().Namespaces().Create(context.Background(), dstNs, metav1.CreateOptions{})
		framework.ExpectNoError(err, "Error creating Namespace %v: %v", dstPodNamespace, err)

		srcNode = nodes.Items[0].Name
		_, err = createPod(f, srcPodName, srcNode, f.Namespace.Name, []string{"bash", "-c", "sleep infinity"}, map[string]string{"component": "nqos-test-src"}, func(p *corev1.Pod) {
			p.Spec.Containers[0].Image = images.Netshoot()
		})
		framework.ExpectNoError(err)
		dstNode = nodes.Items[1].Name
	})

	ginkgo.DescribeTable("Should have correct DSCP value for overlay traffic when NetworkQoS is applied",
		func(skipThisTableEntry *bool, tcpDumpTpl string, dst1IP, dst2IP, dst3IP, dst4IP *string) {
			if *skipThisTableEntry {
				return
			}
			dscpValue := 50
			// dest pod without protocol and port
			dstPod1, err := createPod(f, dstPod1Name, dstNode, dstPodNamespace, []string{"bash", "-c", "sleep infinity"}, map[string]string{"component": "nqos-test-dst"}, func(p *corev1.Pod) {
				p.Spec.Containers[0].Image = images.Netshoot()
			})
			framework.ExpectNoError(err)
			gomega.Eventually(func() error {
				_, err := e2ekubectl.RunKubectl(dstPodNamespace, "exec", dstPod1Name, "--", "which", "tcpdump")
				return err

			}, 60*time.Second, 1*time.Second).ShouldNot(gomega.HaveOccurred())
			dstPod1IPv4, dstPod1IPv6 = getPodAddresses(dstPod1)

			// dest pod covered by tcp without port rule
			dstPod2, err := createPod(f, dstPod2Name, dstNode, dstPodNamespace, []string{"bash", "-c", "nc -l -p 9090; sleep infinity"}, map[string]string{"component": "nqos-test-tcp"}, func(p *corev1.Pod) {
				p.Spec.Containers[0].Image = images.Netshoot()
			})
			framework.ExpectNoError(err)
			gomega.Eventually(func() error {
				_, err := e2ekubectl.RunKubectl(dstPodNamespace, "exec", dstPod1Name, "--", "which", "nc")
				return err

			}, 60*time.Second, 1*time.Second).ShouldNot(gomega.HaveOccurred())
			dstPod2IPv4, dstPod2IPv6 = getPodAddresses(dstPod2)

			// dest pod covered by tcp with port rule
			dstPod3, err := createPod(f, dstPod3Name, dstNode, dstPodNamespace, []string{"bash", "-c", "python3 -m http.server 80; sleep infinity"}, map[string]string{"component": "nqos-test-web"}, func(p *corev1.Pod) {
				p.Spec.Containers[0].Image = images.Netshoot()
			})
			framework.ExpectNoError(err)
			gomega.Eventually(func() error {
				_, err := e2ekubectl.RunKubectl(dstPodNamespace, "exec", dstPod1Name, "--", "which", "python3")
				return err

			}, 60*time.Second, 1*time.Second).ShouldNot(gomega.HaveOccurred())
			dstPod3IPv4, dstPod3IPv6 = getPodAddresses(dstPod3)

			// dest pod not covered by networkqos
			dstPod4, err := createPod(f, dstPod4Name, dstNode, dstPodNamespace, []string{"bash", "-c", "sleep infinity"}, nil, func(p *corev1.Pod) {
				p.Spec.Containers[0].Image = images.Netshoot()
			})
			framework.ExpectNoError(err)
			gomega.Eventually(func() error {
				_, err := e2ekubectl.RunKubectl(dstPodNamespace, "exec", dstPod1Name, "--", "which", "tcpdump")
				return err

			}, 60*time.Second, 1*time.Second).ShouldNot(gomega.HaveOccurred())
			dstPod4IPv4, dstPod4IPv6 = getPodAddresses(dstPod4)

			// no dscp (dscp == 0) should be deteced before networkqos is applied
			pingExpectDscp(f, srcPodName, dstPodNamespace, dstPod1Name, *dst1IP, tcpDumpTpl, 0)
			netcatExpectDscp(f, srcPodName, dstPodNamespace, dstPod2Name, *dst2IP, tcpDumpTpl, 9090, 0)
			netcatExpectDscp(f, srcPodName, dstPodNamespace, dstPod3Name, *dst3IP, tcpDumpTpl, 80, 0)
			pingExpectDscp(f, srcPodName, dstPodNamespace, dstPod4Name, *dst4IP, tcpDumpTpl, 0)

			// apply networkqos spec
			networkQoSSpec := fmt.Sprintf(`
apiVersion: k8s.ovn.org/v1alpha1
kind: NetworkQoS
metadata:
  namespace: %s
  name: %s
spec:
  podSelector:
    matchLabels:
      component: nqos-test-src
  priority: 50
  egress:
  - dscp: %d
    classifier:
      to:
      - podSelector:
          matchLabels:
            component: nqos-test-dst
        namespaceSelector:
          matchLabels:
            app: nqos-test
  - dscp: %d
    classifier:
      ports:
      - protocol: TCP
      to:
      - podSelector:
          matchLabels:
            component: nqos-test-tcp
        namespaceSelector:
          matchLabels:
           app: nqos-test
  - dscp: %d
    classifier:
      ports:
      - protocol: TCP
        port: 80
      to:
      - podSelector:
          matchLabels:
            component: nqos-test-web
        namespaceSelector:
          matchLabels:
           app: nqos-test
`, f.Namespace.Name, nqosSpecName, dscpValue, dscpValue+1, dscpValue+2)
			if err := os.WriteFile(networkQoSYaml, []byte(networkQoSSpec), 0644); err != nil {
				framework.Failf("Unable to write CRD to disk: %v", err)
			}
			defer func() {
				if err := os.Remove(networkQoSYaml); err != nil {
					framework.Logf("Unable to remove the CRD file from disk: %v", err)
				}
			}()
			e2ekubectl.RunKubectlOrDie(f.Namespace.Name, "create", "-f", networkQoSYaml)
			framework.Logf("NetworkQoS applied")
			waitForNetworkQoSApplied(f.Namespace.Name, 3)
			// verify dscp
			pingExpectDscp(f, srcPodName, dstPodNamespace, dstPod1Name, *dst1IP, tcpDumpTpl, dscpValue)
			netcatExpectDscp(f, srcPodName, dstPodNamespace, dstPod2Name, *dst2IP, tcpDumpTpl, 9090, dscpValue+1)
			netcatExpectDscp(f, srcPodName, dstPodNamespace, dstPod3Name, *dst3IP, tcpDumpTpl, 80, dscpValue+2)
			pingExpectDscp(f, srcPodName, dstPodNamespace, dstPod4Name, *dst4IP, tcpDumpTpl, 0)
		},
		ginkgo.Entry("ipv4", &skipIpv4, tcpdumpIPv4, &dstPod1IPv4, &dstPod2IPv4, &dstPod3IPv4, &dstPod4IPv4),
		ginkgo.Entry("ipv6", &skipIpv6, tcpdumpIPv6, &dstPod1IPv6, &dstPod2IPv6, &dstPod3IPv6, &dstPod4IPv6),
	)

	ginkgo.DescribeTable("Should have correct DSCP value for host network traffic when NetworkQoS is applied",
		func(skipThisTableEntry *bool, tcpDumpTpl string, dst1IP, dst2IP, dst3IP, dst4IP *string) {
			if *skipThisTableEntry {
				return
			}
			dscpValue := 32
			// dest pod to test traffic without protocol and port
			dstPod1, err := createPod(f, dstPod1Name, dstNode, dstPodNamespace, []string{"bash", "-c", "sleep infinity"}, nil, func(p *corev1.Pod) {
				p.Spec.HostNetwork = true
				p.Spec.Containers[0].Image = images.Netshoot()
			})
			framework.ExpectNoError(err)
			gomega.Eventually(func() error {
				_, err := e2ekubectl.RunKubectl(dstPodNamespace, "exec", dstPod1Name, "--", "which", "tcpdump")
				return err

			}, 60*time.Second, 1*time.Second).ShouldNot(gomega.HaveOccurred())
			dstPod1IPv4, dstPod1IPv6 = getPodAddresses(dstPod1)

			// dest pod to test traffic with tcp protocol but no port
			dstPod2, err := createPod(f, dstPod2Name, dstNode, dstPodNamespace, []string{"bash", "-c", "nc -l -p 9090; sleep infinity"}, nil, func(p *corev1.Pod) {
				p.Spec.HostNetwork = true
				p.Spec.Containers[0].Image = images.Netshoot()
			})
			framework.ExpectNoError(err)
			gomega.Eventually(func() error {
				_, err := e2ekubectl.RunKubectl(dstPodNamespace, "exec", dstPod1Name, "--", "which", "nc")
				return err

			}, 60*time.Second, 1*time.Second).ShouldNot(gomega.HaveOccurred())
			dstPod2IPv4, dstPod2IPv6 = getPodAddresses(dstPod2)

			// dest pod to test traffic with tcp protocol and port
			dstPod3, err := createPod(f, dstPod3Name, dstNode, dstPodNamespace, []string{"bash", "-c", "python3 -m http.server 80; sleep infinity"}, nil, func(p *corev1.Pod) {
				p.Spec.HostNetwork = true
				p.Spec.Containers[0].Image = images.Netshoot()
			})
			framework.ExpectNoError(err)
			gomega.Eventually(func() error {
				_, err := e2ekubectl.RunKubectl(dstPodNamespace, "exec", dstPod1Name, "--", "which", "python3")
				return err

			}, 60*time.Second, 1*time.Second).ShouldNot(gomega.HaveOccurred())
			dstPod3IPv4, dstPod3IPv6 = getPodAddresses(dstPod3)

			// dest pod not covered by networkqos
			dstPod4, err := createPod(f, dstPod4Name, dstNode, dstPodNamespace, []string{"bash", "-c", "sleep infinity"}, nil, func(p *corev1.Pod) {
				p.Spec.Containers[0].Image = images.Netshoot()
			})
			framework.ExpectNoError(err)
			gomega.Eventually(func() error {
				_, err := e2ekubectl.RunKubectl(dstPodNamespace, "exec", dstPod1Name, "--", "which", "tcpdump")
				return err

			}, 60*time.Second, 1*time.Second).ShouldNot(gomega.HaveOccurred())
			dstPod4IPv4, dstPod4IPv6 = getPodAddresses(dstPod4)

			// no dscp (dscp == 0) should be deteced before networkqos is applied
			pingExpectDscp(f, srcPodName, dstPodNamespace, dstPod1Name, *dst1IP, tcpDumpTpl, 0)
			netcatExpectDscp(f, srcPodName, dstPodNamespace, dstPod2Name, *dst2IP, tcpDumpTpl, 9090, 0)
			netcatExpectDscp(f, srcPodName, dstPodNamespace, dstPod3Name, *dst3IP, tcpDumpTpl, 80, 0)
			pingExpectDscp(f, srcPodName, dstPodNamespace, dstPod4Name, *dst4IP, tcpDumpTpl, 0)

			// apply networkqos spec
			networkQoSSpec := fmt.Sprintf(`
apiVersion: k8s.ovn.org/v1alpha1
kind: NetworkQoS
metadata:
  namespace: %s
  name: %s
spec:
  podSelector:
    matchLabels:
      component: nqos-test-src
  priority: 51
  egress:
  - dscp: %d
    classifier:
      to:
      - ipBlock:
          cidr: %s
      - ipBlock:
          cidr: %s
  - dscp: %d
    classifier:
      ports:
      - protocol: TCP
      to:
      - ipBlock:
          cidr: %s
      - ipBlock:
          cidr: %s
  - dscp: %d
    classifier:
      ports:
      - protocol: TCP
        port: 80
      to:
      - ipBlock:
          cidr: %s
      - ipBlock:
          cidr: %s
`, f.Namespace.Name, nqosSpecName, dscpValue, nodeIPv4Range, nodeIPv6Range, dscpValue+1, nodeIPv4Range, nodeIPv6Range, dscpValue+2, nodeIPv4Range, nodeIPv6Range)
			if err := os.WriteFile(networkQoSYaml, []byte(networkQoSSpec), 0644); err != nil {
				framework.Failf("Unable to write CRD to disk: %v", err)
			}
			defer func() {
				if err := os.Remove(networkQoSYaml); err != nil {
					framework.Logf("Unable to remove the CRD file from disk: %v", err)
				}
			}()
			e2ekubectl.RunKubectlOrDie(f.Namespace.Name, "create", "-f", networkQoSYaml)
			framework.Logf("NetworkQoS applied")
			waitForNetworkQoSApplied(f.Namespace.Name, 3)
			// verify dscp
			pingExpectDscp(f, srcPodName, dstPodNamespace, dstPod1Name, *dst1IP, tcpDumpTpl, dscpValue)
			netcatExpectDscp(f, srcPodName, dstPodNamespace, dstPod2Name, *dst2IP, tcpDumpTpl, 9090, dscpValue+1)
			netcatExpectDscp(f, srcPodName, dstPodNamespace, dstPod3Name, *dst3IP, tcpDumpTpl, 80, dscpValue+2)
			pingExpectDscp(f, srcPodName, dstPodNamespace, dstPod4Name, *dst4IP, tcpDumpTpl, 0)
		},
		ginkgo.Entry("ipv4", &skipIpv4, tcpdumpIPv4, &dstPod1IPv4, &dstPod2IPv4, &dstPod3IPv4, &dstPod4IPv4),
		ginkgo.Entry("ipv6", &skipIpv6, tcpdumpIPv6, &dstPod1IPv6, &dstPod2IPv6, &dstPod3IPv6, &dstPod4IPv6),
	)

	ginkgo.DescribeTable("Limits egress traffic to all target pods below the specified rate in NetworkQoS spec",
		func(skipThisTableEntry *bool, dst1IP, dst2IP *string) {
			if *skipThisTableEntry {
				return
			}
			rate := 10000
			// dest pod 1 for test without protocol & port
			dstPod1, err := createPod(f, dstPod1Name, dstNode, dstPodNamespace, []string{"bash", "-c", "iperf3 -s"}, map[string]string{"component": "nqos-test-dst"}, func(p *corev1.Pod) {
				p.Spec.Containers[0].Image = images.Netshoot()
			})
			framework.ExpectNoError(err)
			gomega.Eventually(func() error {
				_, err := e2ekubectl.RunKubectl(dstPodNamespace, "exec", dstPod1Name, "--", "which", "iperf3")
				return err

			}, 60*time.Second, 1*time.Second).ShouldNot(gomega.HaveOccurred())
			dstPod1IPv4, dstPod1IPv6 = getPodAddresses(dstPod1)
			// dest pod 2 for test without protocol & port
			dstPod2, err := createPod(f, dstPod2Name, dstNode, dstPodNamespace, []string{"bash", "-c", "iperf3 -s"}, map[string]string{"component": "nqos-test-dst"}, func(p *corev1.Pod) {
				p.Spec.Containers[0].Image = images.Netshoot()
			})
			framework.ExpectNoError(err)
			gomega.Eventually(func() error {
				_, err := e2ekubectl.RunKubectl(dstPodNamespace, "exec", dstPod1Name, "--", "which", "iperf3")
				return err

			}, 60*time.Second, 1*time.Second).ShouldNot(gomega.HaveOccurred())
			dstPod2IPv4, dstPod2IPv6 = getPodAddresses(dstPod2)

			bps := twoStreamIperf3Tests(f, srcPodName, *dst1IP, *dst2IP, 5201)
			gomega.Expect(bps/1000 > float64(rate)*bandwidthFluctuation).To(gomega.BeTrue())

			// apply networkqos spec
			networkQoSSpec := fmt.Sprintf(`
apiVersion: k8s.ovn.org/v1alpha1
kind: NetworkQoS
metadata:
  namespace: %s
  name: %s
spec:
  podSelector:
    matchLabels:
      component: nqos-test-src
  priority: 52
  egress:
  - dscp: 1
    bandwidth:
      rate: %d
    classifier:
      to:
      - podSelector:
          matchLabels:
            component: nqos-test-dst
        namespaceSelector:
          matchLabels:
            app: nqos-test
`, f.Namespace.Name, nqosSpecName, rate)
			if err := os.WriteFile(networkQoSYaml, []byte(networkQoSSpec), 0644); err != nil {
				framework.Failf("Unable to write CRD to disk: %v", err)
			}
			defer func() {
				if err := os.Remove(networkQoSYaml); err != nil {
					framework.Logf("Unable to remove the CRD file from disk: %v", err)
				}
			}()
			e2ekubectl.RunKubectlOrDie(f.Namespace.Name, "create", "-f", networkQoSYaml)
			framework.Logf("NetworkQoS applied")
			waitForNetworkQoSApplied(f.Namespace.Name, 1)
			bps = twoStreamIperf3Tests(f, srcPodName, *dst1IP, *dst2IP, 5201)
			gomega.Expect(bps/1000 <= float64(rate)*bandwidthFluctuation).To(gomega.BeTrue())
		},
		ginkgo.Entry("ipv4", &skipIpv4, &dstPod1IPv4, &dstPod2IPv4),
		ginkgo.Entry("ipv6", &skipIpv6, &dstPod1IPv6, &dstPod2IPv6),
	)

	ginkgo.DescribeTable("Limits egress traffic targeting an individual pod by protocol through a NetworkQoS spec",
		func(skipThisTableEntry *bool, dst1IP *string) {
			if *skipThisTableEntry {
				return
			}
			rate := 5000
			// dest pod for test with protocol
			dstPod1, err := createPod(f, dstPod1Name, dstNode, dstPodNamespace, []string{"bash", "-c", "iperf3 -s"}, map[string]string{"component": "nqos-test-tcp"}, func(p *corev1.Pod) {
				p.Spec.Containers[0].Image = images.Netshoot()
			})
			framework.ExpectNoError(err)
			gomega.Eventually(func() error {
				_, err := e2ekubectl.RunKubectl(dstPodNamespace, "exec", dstPod1Name, "--", "which", "iperf3")
				return err

			}, 60*time.Second, 1*time.Second).ShouldNot(gomega.HaveOccurred())
			dstPod1IPv4, dstPod1IPv6 = getPodAddresses(dstPod1)
			bps := iperf3Test(f, srcPodName, *dst1IP, 5201)
			gomega.Expect(bps/1000 > float64(rate)*bandwidthFluctuation).To(gomega.BeTrue())
			// apply networkqos spec
			networkQoSSpec := fmt.Sprintf(`
apiVersion: k8s.ovn.org/v1alpha1
kind: NetworkQoS
metadata:
  namespace: %s
  name: %s
spec:
  podSelector:
    matchLabels:
      component: nqos-test-src
  priority: 53
  egress:
  - dscp: 2
    bandwidth:
      rate: %d
    classifier:
      ports:
      - protocol: TCP
      to:
      - podSelector:
          matchLabels:
            component: nqos-test-tcp
        namespaceSelector:
          matchLabels:
           app: nqos-test
`, f.Namespace.Name, nqosSpecName, rate)
			if err := os.WriteFile(networkQoSYaml, []byte(networkQoSSpec), 0644); err != nil {
				framework.Failf("Unable to write CRD to disk: %v", err)
			}
			defer func() {
				if err := os.Remove(networkQoSYaml); err != nil {
					framework.Logf("Unable to remove the CRD file from disk: %v", err)
				}
			}()
			e2ekubectl.RunKubectlOrDie(f.Namespace.Name, "create", "-f", networkQoSYaml)
			framework.Logf("NetworkQoS applied")
			waitForNetworkQoSApplied(f.Namespace.Name, 1)
			bps = iperf3Test(f, srcPodName, *dst1IP, 5201)
			gomega.Expect(bps/1000 <= float64(rate)*bandwidthFluctuation).To(gomega.BeTrue())
		},
		ginkgo.Entry("ipv4", &skipIpv4, &dstPod1IPv4),
		ginkgo.Entry("ipv6", &skipIpv6, &dstPod1IPv6),
	)

	ginkgo.DescribeTable("Limits egress traffic targeting a pod by protocol and port through a NetworkQoS spec",
		func(skipThisTableEntry *bool, dst1IP *string) {
			if *skipThisTableEntry {
				return
			}
			rate := 5000
			// dest pod for test with protocol and port
			dstPod1, err := createPod(f, dstPod1Name, dstNode, dstPodNamespace, []string{"bash", "-c", "iperf3 -s -p 80"}, map[string]string{"component": "nqos-test-proto-and-port"}, func(p *corev1.Pod) {
				p.Spec.Containers[0].Image = images.Netshoot()
			})
			framework.ExpectNoError(err)
			gomega.Eventually(func() error {
				_, err := e2ekubectl.RunKubectl(dstPodNamespace, "exec", dstPod1Name, "--", "which", "iperf3")
				return err

			}, 60*time.Second, 1*time.Second).ShouldNot(gomega.HaveOccurred())
			dstPod1IPv4, dstPod1IPv6 = getPodAddresses(dstPod1)
			bps := iperf3Test(f, srcPodName, *dst1IP, 80)
			gomega.Expect(bps/1000 > float64(rate)*bandwidthFluctuation).To(gomega.BeTrue())
			// apply networkqos spec
			networkQoSSpec := fmt.Sprintf(`
apiVersion: k8s.ovn.org/v1alpha1
kind: NetworkQoS
metadata:
  namespace: %s
  name: %s
spec:
  podSelector:
    matchLabels:
      component: nqos-test-src
  priority: 54
  egress:
  - dscp: 3
    bandwidth:
      rate: %d
    classifier:
      ports:
      - protocol: TCP
        port: 80
      to:
      - podSelector:
          matchLabels:
            component: nqos-test-proto-and-port
        namespaceSelector:
          matchLabels:
           app: nqos-test
`, f.Namespace.Name, nqosSpecName, rate)
			if err := os.WriteFile(networkQoSYaml, []byte(networkQoSSpec), 0644); err != nil {
				framework.Failf("Unable to write CRD to disk: %v", err)
			}
			defer func() {
				if err := os.Remove(networkQoSYaml); err != nil {
					framework.Logf("Unable to remove the CRD file from disk: %v", err)
				}
			}()
			e2ekubectl.RunKubectlOrDie(f.Namespace.Name, "create", "-f", networkQoSYaml)
			framework.Logf("NetworkQoS applied")
			waitForNetworkQoSApplied(f.Namespace.Name, 1)
			bps = iperf3Test(f, srcPodName, *dst1IP, 80)
			gomega.Expect(bps/1000 <= float64(rate)*bandwidthFluctuation).To(gomega.BeTrue())
		},
		ginkgo.Entry("ipv4", &skipIpv4, &dstPod1IPv4),
		ginkgo.Entry("ipv6", &skipIpv6, &dstPod1IPv6),
	)

	// Traffic through a Service to a pod on the node of the sender goes through the router and
	// passes the logical switch of the node twice.
	ginkgo.DescribeTable("Applies a rule once to traffic through a Service to a pod on the same node",
		func(skipThisTableEntry *bool, tcpDumpTpl string, family utilnet.IPFamily) {
			if *skipThisTableEntry {
				return
			}
			dstPod, err := createPod(f, dstPod1Name, srcNode, dstPodNamespace, []string{"bash", "-c", "iperf3 -s"}, map[string]string{"component": "nqos-test-dst"}, func(p *corev1.Pod) {
				p.Spec.Containers[0].Image = images.Netshoot()
			})
			framework.ExpectNoError(err)
			dstPodIPv4, dstPodIPv6 := getPodAddresses(dstPod)
			dstPodIP := dstPodIPv4
			if family == utilnet.IPv6 {
				dstPodIP = dstPodIPv6
			}
			service := createNetworkQoSService(f, dstPodNamespace, map[string]string{"component": "nqos-test-dst"})
			serviceIP := getFirstIPStringOfFamily(family, service.Spec.ClusterIPs)
			gomega.Expect(serviceIP).NotTo(gomega.BeEmpty(), "expected a cluster IP of the family of %s", dstPodIP)

			applyOverlappingNetworkQoS(f.Namespace.Name, srcNode, "", `      - podSelector:
          matchLabels:
            component: nqos-test-dst
        namespaceSelector:
          matchLabels:
            app: nqos-test
`)
			// EgressQoS marks the same traffic with a lower priority, on both passes.
			applyEgressQoS(f.Namespace.Name, egressQoSDSCP)
			ginkgo.By("Checking that the rule with the highest priority marks the traffic")
			waitForNetworkQoSService(f.Namespace.Name, srcPodName, serviceIP)
			netcatExpectDscp(f, srcPodName, dstPodNamespace, dstPod1Name, serviceIP, tcpDumpTpl, networkQoSServicePort, networkQoSHighDSCP)
			ginkgo.By("Checking that its bandwidth limit applies once")
			expectServiceTrafficLimitedOnce(f.Namespace.Name, srcPodName, dstPodIP, serviceIP, float64(networkQoSLimitedRate)*bandwidthFluctuation)
		},
		ginkgo.Entry("ipv4", &skipIpv4, tcpdumpIPv4, utilnet.IPv4),
		ginkgo.Entry("ipv6", &skipIpv6, tcpdumpIPv6, utilnet.IPv6),
	)

	ginkgo.It("Rejects a port with an unknown protocol or without a protocol", func() {
		for _, tc := range []struct {
			ports   string
			message string
		}{
			{ports: "[{protocol: TCPX, port: 80}]", message: `Unsupported value: "TCPX"`},
			{ports: "[{port: 80}]", message: "protocol is required when port is set"},
		} {
			manifest := fmt.Sprintf(`apiVersion: k8s.ovn.org/v1alpha1
kind: NetworkQoS
metadata:
  name: %s
spec:
  priority: 50
  egress:
  - dscp: 10
    classifier:
      ports: %s
`, nqosSpecName, tc.ports)
			_, err := e2ekubectl.RunKubectlInput(f.Namespace.Name, manifest, "create", "-f", "-")
			gomega.Expect(err).To(gomega.HaveOccurred(), "expected the API server to reject ports %s", tc.ports)
			gomega.Expect(err.Error()).To(gomega.ContainSubstring(tc.message))
		}
	})

	ginkgo.AfterEach(func() {
		err := f.ClientSet.CoreV1().Namespaces().Delete(context.Background(), dstPodNamespace, metav1.DeleteOptions{})
		framework.ExpectNoError(err, "Error deleting Namespace %v: %v", dstPodNamespace, err)
	})
})

// A layer2 network has one logical switch spanning the nodes, so its rules apply as packets enter
// the switch: a rule applied as they leave it would mark traffic to another node only there.
var _ = ginkgo.Describe("e2e NetworkQoS validation on user defined networks", feature.NetworkQos, feature.NetworkSegmentation, func() {
	const (
		udnName      = "nqos-udn"
		srcPod       = "nqos-src"
		localDstPod  = "nqos-dst-local"
		remoteDstPod = "nqos-dst-remote"
		capturePod   = "nqos-capture"
		tcpdumpIPv4  = "(ip and (ip[1] & 0xfc) >> 2 == %d)"
		tcpdumpIPv6  = "(ip6 and (ip6[0:2] & 0xfc0) >> 6 == %d)"

		bandwidthFluctuation = 1.5
	)

	var (
		srcNode          string
		remoteNode       string
		captureNamespace string
	)

	f := wrappedTestFramework("nqos-udn")
	f.SkipNamespaceCreation = true

	ginkgo.BeforeEach(func() {
		nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.TODO(), f.ClientSet, 2)
		framework.ExpectNoError(err)
		if len(nodes.Items) < 2 {
			framework.Failf("Test requires >= 2 Ready nodes, but there are only %v nodes", len(nodes.Items))
		}
		srcNode = nodes.Items[0].Name
		remoteNode = nodes.Items[1].Name
		namespace, err := f.CreateNamespace(context.TODO(), f.BaseName, map[string]string{
			"e2e-framework":           f.BaseName,
			RequiredUDNNamespaceLabel: "",
		})
		framework.ExpectNoError(err)
		f.Namespace = namespace
		// The capture pod uses the host network, so keep it out of the namespace of the network.
		capture, err := f.CreateNamespace(context.TODO(), f.BaseName+"-capture", nil)
		framework.ExpectNoError(err)
		captureNamespace = capture.Name
	})

	ginkgo.DescribeTable("Marks traffic before it leaves the node and applies a rule once to traffic through a Service",
		func(topology, cidr string) {
			namespace := f.Namespace.Name
			params := networkAttachmentConfigParams{
				name:      udnName,
				namespace: namespace,
				topology:  topology,
				cidr:      cidr,
				role:      "primary",
			}
			cleanup, err := createManifest(namespace, generateUserDefinedNetworkManifest(&params, f.ClientSet))
			framework.ExpectNoError(err)
			ginkgo.DeferCleanup(cleanup)
			gomega.Eventually(userDefinedNetworkReadyFunc(context.TODO(), f.DynamicClient, namespace, udnName), 30*time.Second, time.Second).Should(gomega.Succeed())

			netshoot := func(p *corev1.Pod) {
				p.Spec.Containers[0].Image = images.Netshoot()
			}
			_, err = createPod(f, srcPod, srcNode, namespace, []string{"bash", "-c", "sleep infinity"},
				map[string]string{"component": "nqos-test-src"}, netshoot)
			framework.ExpectNoError(err)
			_, err = createPod(f, localDstPod, srcNode, namespace, []string{"bash", "-c", "iperf3 -s"},
				map[string]string{"component": "nqos-test-dst", "nqos-service": "true"}, netshoot)
			framework.ExpectNoError(err)
			_, err = createPod(f, remoteDstPod, remoteNode, namespace, []string{"bash", "-c", "sleep infinity"},
				map[string]string{"component": "nqos-test-dst"}, netshoot)
			framework.ExpectNoError(err)
			_, err = createPod(f, capturePod, srcNode, captureNamespace, []string{"bash", "-c", "sleep infinity"}, nil, func(p *corev1.Pod) {
				netshoot(p)
				p.Spec.HostNetwork = true
				p.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{Privileged: ptr.To(true)}
			})
			framework.ExpectNoError(err)
			service := createNetworkQoSService(f, namespace, map[string]string{"nqos-service": "true"})

			applyOverlappingNetworkQoS(namespace, srcNode, fmt.Sprintf(`  networkSelectors:
  - networkSelectionType: NetworkAttachmentDefinitions
    networkAttachmentDefinitionSelector:
      namespaceSelector:
        matchLabels:
          kubernetes.io/metadata.name: %s
      networkSelector: {}
`, namespace), `      - podSelector:
          matchLabels:
            component: nqos-test-dst
`)

			podIP := func(pod string, family utilnet.IPFamily) string {
				ip, err := getPodAnnotationIPsForPrimaryNetworkByIPFamily(f.ClientSet, namespace, pod, udnName, family)
				framework.ExpectNoError(err)
				gomega.Expect(ip).NotTo(gomega.BeEmpty(), "expected pod %s/%s to have an IPv%s address on %s", namespace, pod, family, udnName)
				return ip
			}
			var families []utilnet.IPFamily
			if isIPv4Supported(f.ClientSet) {
				families = append(families, utilnet.IPv4)
			}
			if isIPv6Supported(f.ClientSet) {
				families = append(families, utilnet.IPv6)
			}
			for _, family := range families {
				srcIP, remoteIP := podIP(srcPod, family), podIP(remoteDstPod, family)
				ginkgo.By(fmt.Sprintf("Checking that traffic from %s to %s on another node is marked before it leaves the node", srcIP, remoteIP))
				gomega.Eventually(func() ([]int, error) {
					return dscpOfTunneledPackets(captureNamespace, capturePod, namespace, srcPod, srcIP, remoteIP)
				}, 90*time.Second, time.Second).Should(gomega.HaveEach(networkQoSHighDSCP))

				serviceIP := getFirstIPStringOfFamily(family, service.Spec.ClusterIPs)
				gomega.Expect(serviceIP).NotTo(gomega.BeEmpty(), "expected a cluster IP of family IPv%s", family)
				tcpDumpTpl := tcpdumpIPv4
				if family == utilnet.IPv6 {
					tcpDumpTpl = tcpdumpIPv6
				}
				ginkgo.By(fmt.Sprintf("Checking that the rule with the higher priority marks the traffic through %s", serviceIP))
				waitForNetworkQoSService(namespace, srcPod, serviceIP)
				netcatExpectDscp(f, srcPod, namespace, localDstPod, serviceIP, tcpDumpTpl, networkQoSServicePort, networkQoSHighDSCP)
			}

			ginkgo.By("Checking that its bandwidth limit applies once to traffic through the Service")
			expectServiceTrafficLimitedOnce(namespace, srcPod, podIP(localDstPod, families[0]),
				getFirstIPStringOfFamily(families[0], service.Spec.ClusterIPs), float64(networkQoSLimitedRate)*bandwidthFluctuation)
		},
		ginkgo.Entry("layer3", "layer3", primaryLayer3MultiCIDRs()),
		ginkgo.Entry("layer2", "layer2", joinStrings("172.16.0.0/16", "2014:100:200::0/60")),
	)
})

// The timeout command of the image exits with the status of the command it stops, and tcpdump
// exits with zero when stopped, so a capture succeeds only if it printed a packet.
func captureDscpOnPod(dstPodNamespace, pod, tcpDumpTpl string, dscp int) error {
	output, err := e2ekubectl.RunKubectl(dstPodNamespace, "exec", pod, "--", "timeout", "10",
		"tcpdump", "-i", "any", "-c", "1", "-v", fmt.Sprintf(tcpDumpTpl, dscp))
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(output)) == 0 {
		return fmt.Errorf("no packet with DSCP %d captured on pod %s/%s", dscp, dstPodNamespace, pod)
	}
	return nil
}

func pingExpectDscp(f *framework.Framework, srcPod, dstPodNamespace, dstPod, dstPodIP, tcpDumpTpl string, dscp int) {
	tcpDumpSync := errgroup.Group{}
	pingSync := errgroup.Group{}

	checkDSCPOnPod := func(pod string, dscp int) error {
		return captureDscpOnPod(dstPodNamespace, pod, tcpDumpTpl, dscp)
	}

	pingFromSrcPod := func(pod, dst string) error {
		_, err := e2ekubectl.RunKubectl(f.Namespace.Name, "exec", pod, "--", "ping", "-c", "5", dst)
		return err
	}

	tcpDumpSync.Go(func() error {
		return checkDSCPOnPod(dstPod, dscp)
	})
	pingSync.Go(func() error {
		return pingFromSrcPod(srcPod, dstPodIP)
	})
	err := pingSync.Wait()
	framework.ExpectNoError(err, "Failed to ping dst pod")
	err = tcpDumpSync.Wait()
	framework.ExpectNoError(err, "Failed to detect ping with correct DSCP on pod")
}

func netcatExpectDscp(f *framework.Framework, srcPod, dstPodNamespace, dstPod, dstPodIP, tcpDumpTpl string, port, dscp int) {
	tcpDumpSync := errgroup.Group{}
	netcatSync := errgroup.Group{}

	checkDSCPOnPod := func(pod string, dscp int) error {
		return captureDscpOnPod(dstPodNamespace, pod, tcpDumpTpl, dscp)
	}

	netcatFromSrcPod := func(pod, dst string) error {
		_, err := e2ekubectl.RunKubectl(f.Namespace.Name, "exec", pod, "--", "bash", "-c", fmt.Sprintf("for i in {1..5}; do nc -vz -w 1 %s %d; sleep 1; done", dst, port))
		return err
	}

	tcpDumpSync.Go(func() error {
		return checkDSCPOnPod(dstPod, dscp)
	})
	netcatSync.Go(func() error {
		return netcatFromSrcPod(srcPod, dstPodIP)
	})
	err := netcatSync.Wait()
	framework.ExpectNoError(err, "Failed to connect to dst pod")
	err = tcpDumpSync.Wait()
	framework.ExpectNoError(err, "Failed to detect packets with correct DSCP on pod")
}

func iperf3Test(f *framework.Framework, srcPod, dstIP string, port int, protocol ...string) float64 {
	iperf3Sync := errgroup.Group{}

	iperfTest := func(pod, destIP string, port int, bps *float64) error {
		args := []string{"exec", pod, "--", "iperf3", "-c", destIP, "-p", strconv.Itoa(port), "-J"}
		if len(protocol) > 0 && protocol[0] == "udp" {
			args = append(args, "-u", "-b", "0")
		}
		output, err := e2ekubectl.RunKubectl(f.Namespace.Name, args...)
		if err != nil {
			return err
		}
		var data map[string]interface{}
		err = json.Unmarshal([]byte(output), &data)
		if err != nil {
			return err
		}
		end := data["end"].(map[string]interface{})
		if sum_sent, ok := end["sum_sent"]; ok {
			*bps = sum_sent.(map[string]interface{})["bits_per_second"].(float64)
		} else if sum, ok := end["sum"]; ok {
			*bps = sum.(map[string]interface{})["bits_per_second"].(float64)
		}
		return nil
	}
	bps := 0.0
	iperf3Sync.Go(func() error {
		return iperfTest(srcPod, dstIP, port, &bps)
	})
	err := iperf3Sync.Wait()
	framework.ExpectNoError(err, fmt.Sprintf("Failed to run iperf3 test for IP %s", dstIP))
	return bps
}

// An error-free status does not mean the rules are ready. Check the zone of the node too.
func waitForNetworkQoSAppliedOnNode(namespace, name, nodeName string, rules int) {
	gomega.Eventually(func(g gomega.Gomega) {
		output, err := e2ekubectl.RunKubectl(namespace, "get", "networkqos", name)
		g.Expect(err).NotTo(gomega.HaveOccurred(), "could not get the networkqos %s in namespace: %s", name, namespace)
		g.Expect(output).To(gomega.ContainSubstring("No NetworkQoS errors reported"))
	}, 10*time.Second).Should(gomega.Succeed(), fmt.Sprintf("expected networkqos %s/%s to be successfully applied", namespace, name))
	gomega.Eventually(func() error {
		return networkQoSBoundOnNode(namespace, name, nodeName, rules)
	}, 60*time.Second, time.Second).Should(gomega.Succeed(),
		fmt.Sprintf("expected the zone of node %s to apply networkqos %s/%s", nodeName, namespace, name))
}

// Check the rule count in the NB database of the zone of the node, and that a logical switch
// applies each of their rows. The switch depends on the network.
func networkQoSBoundOnNode(namespace, name, nodeName string, rules int) error {
	ovnNamespace := deploymentconfig.Get().OVNKubernetesNamespace()
	pods, err := e2ekubectl.RunKubectl(ovnNamespace, "get", "pods", "-l", "app=ovnkube-node",
		"--field-selector", "spec.nodeName="+nodeName, "-o=jsonpath={.items[0].metadata.name}")
	if err != nil {
		return fmt.Errorf("failed to find the ovnkube-node pod of node %s: %w", nodeName, err)
	}
	nbctl := func(args ...string) (string, error) {
		return e2ekubectl.RunKubectl(ovnNamespace, append([]string{"exec", strings.TrimSpace(pods), "-c", "nb-ovsdb", "--",
			"ovn-nbctl", "--data=bare", "--no-heading"}, args...)...)
	}
	qoses, err := nbctl("--columns=_uuid", "find", "qos",
		"external_ids:k8s.ovn.org/owner-type=NetworkQoS",
		fmt.Sprintf("external_ids:k8s.ovn.org/name=\"%s:%s\"", namespace, name))
	if err != nil {
		return fmt.Errorf("failed to find the QoS rules of networkqos %s/%s: %w", namespace, name, err)
	}
	uuids := strings.Fields(qoses)
	// On the default network, each rule also has a row for the packets routers send into the switch.
	ruleRows := 0
	for _, uuid := range uuids {
		ruleIndex, err := nbctl("get", "qos", uuid, "external_ids:rule-index")
		if err != nil {
			return fmt.Errorf("failed to get the rule index of QoS rule %s: %w", uuid, err)
		}
		// get quotes strings that start with a digit.
		if !strings.HasSuffix(strings.Trim(strings.TrimSpace(ruleIndex), "\""), "-return") {
			ruleRows++
		}
	}
	if ruleRows != rules {
		return fmt.Errorf("found %d QoS rules for networkqos %s/%s, expected %d", ruleRows, namespace, name, rules)
	}
	for _, uuid := range uuids {
		switches, err := nbctl("--columns=name", "find", "logical_switch", "qos_rules{>=}"+uuid)
		if err != nil {
			return fmt.Errorf("failed to find the logical switch of QoS rule %s: %w", uuid, err)
		}
		if strings.TrimSpace(switches) == "" {
			return fmt.Errorf("QoS rule %s of networkqos %s/%s is not bound to a logical switch", uuid, namespace, name)
		}
	}
	return nil
}

const (
	// iperf3 tests UDP through a Service whose TCP and UDP ports are the same.
	networkQoSServicePort = 5201
	networkQoSHighDSCP    = 46
	networkQoSLowDSCP     = 10
	// EgressQoS rules have lower priorities than NetworkQoS rules.
	egressQoSDSCP = 20
	// kbps and kilobits
	networkQoSLimitedRate  = 10000
	networkQoSLimitedBurst = 1000
)

// Create a NetworkQoS and wait until the zone of the node applies its rules.
func applyNetworkQoS(namespace, name, spec, nodeName string, rules int) {
	manifest := fmt.Sprintf("apiVersion: k8s.ovn.org/v1alpha1\nkind: NetworkQoS\nmetadata:\n  name: %s\nspec:\n%s", name, spec)
	_, err := e2ekubectl.RunKubectlInput(namespace, manifest, "create", "-f", "-")
	framework.ExpectNoError(err, "failed to create networkqos %s/%s", namespace, name)
	waitForNetworkQoSAppliedOnNode(namespace, name, nodeName, rules)
}

// Two NetworkQoS for the source pods: one marks and limits their traffic to the destinations,
// and one of lower priority marks all their traffic. networkSelectors is YAML at the level of the
// spec fields, and destinations YAML items of the destination list of the classifier.
func applyOverlappingNetworkQoS(namespace, nodeName, networkSelectors, destinations string) {
	applyNetworkQoS(namespace, "nqos-high", fmt.Sprintf(`%s  podSelector:
    matchLabels:
      component: nqos-test-src
  priority: 60
  egress:
  - dscp: %d
    bandwidth:
      rate: %d
      burst: %d
    classifier:
      to:
%s`, networkSelectors, networkQoSHighDSCP, networkQoSLimitedRate, networkQoSLimitedBurst, destinations), nodeName, 1)
	applyNetworkQoS(namespace, "nqos-low", fmt.Sprintf(`%s  podSelector:
    matchLabels:
      component: nqos-test-src
  priority: 10
  egress:
  - dscp: %d
`, networkSelectors, networkQoSLowDSCP), nodeName, 1)
}

// An EgressQoS that marks all traffic of the pods of the namespace. Wait until every zone applies it.
func applyEgressQoS(namespace string, dscp int) {
	manifest := fmt.Sprintf("apiVersion: k8s.ovn.org/v1\nkind: EgressQoS\nmetadata:\n  name: default\nspec:\n  egress:\n  - dscp: %d\n", dscp)
	_, err := e2ekubectl.RunKubectlInput(namespace, manifest, "create", "-f", "-")
	framework.ExpectNoError(err, "failed to create the egressqos of namespace %s", namespace)
	gomega.Eventually(func() (string, error) {
		return e2ekubectl.RunKubectl(namespace, "get", "egressqos", "default")
	}, 30*time.Second, time.Second).Should(gomega.ContainSubstring("EgressQoS Rules applied"),
		fmt.Sprintf("expected the egressqos of namespace %s to be applied", namespace))
}

// A Service for the iperf3 servers among the selected pods.
func createNetworkQoSService(f *framework.Framework, namespace string, selector map[string]string) *corev1.Service {
	policy := corev1.IPFamilyPolicyPreferDualStack
	service, err := f.ClientSet.CoreV1().Services(namespace).Create(context.TODO(), &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "nqos-service"},
		Spec: corev1.ServiceSpec{
			Selector:       selector,
			IPFamilyPolicy: &policy,
			Ports: []corev1.ServicePort{
				{Name: "tcp", Protocol: corev1.ProtocolTCP, Port: networkQoSServicePort, TargetPort: intstr.FromInt32(networkQoSServicePort)},
				{Name: "udp", Protocol: corev1.ProtocolUDP, Port: networkQoSServicePort, TargetPort: intstr.FromInt32(networkQoSServicePort)},
			},
		},
	}, metav1.CreateOptions{})
	framework.ExpectNoError(err, "failed to create a Service in namespace %s", namespace)
	return service
}

func waitForNetworkQoSService(namespace, srcPod, serviceIP string) {
	gomega.Eventually(func() error {
		_, err := e2ekubectl.RunKubectl(namespace, "exec", srcPod, "--", "nc", "-z", "-w", "2", serviceIP, strconv.Itoa(networkQoSServicePort))
		return err
	}, 60*time.Second, time.Second).Should(gomega.Succeed(), fmt.Sprintf("expected pod %s/%s to reach %s", namespace, srcPod, serviceIP))
}

// The rate in kbps at which the iperf3 server at dstIP received the UDP that the pod sent it
// for five seconds at three times the limit.
func iperf3UDPReceiveRate(namespace, srcPod, dstIP string) (float64, error) {
	output, err := e2ekubectl.RunKubectl(namespace, "exec", srcPod, "--", "iperf3", "-c", dstIP, "-p", strconv.Itoa(networkQoSServicePort),
		"-u", "-b", fmt.Sprintf("%dK", 3*networkQoSLimitedRate), "-t", "5", "-J")
	if err != nil {
		return 0, err
	}
	var result struct {
		End struct {
			SumReceived struct {
				BitsPerSecond float64 `json:"bits_per_second"`
			} `json:"sum_received"`
		} `json:"end"`
	}
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		return 0, fmt.Errorf("failed to parse the output of iperf3 %q: %w", output, err)
	}
	return result.End.SumReceived.BitsPerSecond / 1000, nil
}

// Through the Service, the limit reduces UDP as much as UDP sent straight to the pod. A rule
// applied on both passes of the packets through the switch reduces it to a fraction of that.
// UDP shows it better than TCP, whose rate under a policer depends on the burst.
func expectServiceTrafficLimitedOnce(namespace, srcPod, podIP, serviceIP string, maxRate float64) {
	direct, err := iperf3UDPReceiveRate(namespace, srcPod, podIP)
	framework.ExpectNoError(err, "failed to send UDP from %s/%s to %s", namespace, srcPod, podIP)
	framework.Logf("Received %.0f kbps of UDP sent straight to %s", direct, podIP)
	gomega.Expect(direct).To(gomega.BeNumerically("<=", maxRate), "expected the rule to limit UDP sent straight to the pod")
	gomega.Expect(direct).To(gomega.BeNumerically(">=", float64(networkQoSLimitedRate)/2), "expected UDP sent straight to the pod to reach the limit")
	gomega.Eventually(func() (float64, error) {
		rate, err := iperf3UDPReceiveRate(namespace, srcPod, serviceIP)
		framework.Logf("Received %.0f kbps of UDP sent through %s", rate, serviceIP)
		return rate, err
	}, 60*time.Second, time.Second).Should(gomega.BeNumerically(">=", 0.7*direct),
		"expected UDP sent through the Service to reach the rate of UDP sent straight to the pod")
}

// The inner DSCP of the Geneve packets from srcIP to dstIP that a host network pod captures on the
// node of the sender while the sender pings dstIP. The outer header does not take the DSCP.
func dscpOfTunneledPackets(captureNamespace, capturePod, srcNamespace, srcPod, srcIP, dstIP string) ([]int, error) {
	ipLayer, dscpField := "ip", "ip.dsfield.dscp"
	if utilnet.IsIPv6String(dstIP) {
		ipLayer, dscpField = "ipv6", "ipv6.tclass.dscp"
	}
	var output string
	capture := errgroup.Group{}
	capture.Go(func() error {
		var err error
		output, err = e2ekubectl.RunKubectl(captureNamespace, "exec", capturePod, "--", "tshark", "-i", "any", "-n",
			"-f", "udp port 6081", "-Y", fmt.Sprintf("%[1]s.src == %[2]s && %[1]s.dst == %[3]s", ipLayer, srcIP, dstIP),
			"-T", "fields", "-e", dscpField, "-a", "duration:10")
		return err
	})
	// Let tshark start capturing.
	time.Sleep(3 * time.Second)
	_, pingErr := e2ekubectl.RunKubectl(srcNamespace, "exec", srcPod, "--", "ping", "-c", "5", "-i", "0.5", dstIP)
	if err := capture.Wait(); err != nil {
		return nil, fmt.Errorf("failed to capture the Geneve packets from %s to %s: %w", srcIP, dstIP, err)
	}
	if pingErr != nil {
		return nil, fmt.Errorf("failed to ping %s: %w", dstIP, pingErr)
	}
	var dscps []int
	for _, line := range strings.Fields(output) {
		// The field of the outer header comes first, if it is of the same family.
		values := strings.Split(line, ",")
		dscp, err := strconv.Atoi(values[len(values)-1])
		if err != nil {
			return nil, fmt.Errorf("failed to parse the output of tshark %q: %w", output, err)
		}
		dscps = append(dscps, dscp)
	}
	return dscps, nil
}

func twoStreamIperf3Tests(f *framework.Framework, srcPod, dstPod1IP, dstPod2IP string, port int) float64 {
	iperf3Sync1 := errgroup.Group{}
	iperf3Sync2 := errgroup.Group{}

	iperfTest := func(pod, destIP string, port int, bps *float64) error {
		output, err := e2ekubectl.RunKubectl(f.Namespace.Name, "exec", pod, "--", "iperf3", "-c", destIP, "-p", strconv.Itoa(port), "-J")
		if err != nil {
			return err
		}
		var data map[string]interface{}
		err = json.Unmarshal([]byte(output), &data)
		if err != nil {
			return err
		}
		end := data["end"].(map[string]interface{})
		sum_sent := end["sum_sent"].(map[string]interface{})
		*bps = sum_sent["bits_per_second"].(float64)
		return nil
	}

	bps1 := 0.0
	bps2 := 0.0

	iperf3Sync1.Go(func() error {
		return iperfTest(srcPod, dstPod1IP, port, &bps1)
	})
	iperf3Sync2.Go(func() error {
		return iperfTest(srcPod, dstPod2IP, port, &bps2)
	})
	err := iperf3Sync1.Wait()
	framework.ExpectNoError(err, fmt.Sprintf("Failed to run iperf3 test for IP %s", dstPod1IP))
	err = iperf3Sync2.Wait()
	framework.ExpectNoError(err, fmt.Sprintf("Failed to run iperf3 test for IP %s", dstPod2IP))
	return bps1 + bps2
}

func pingExpectNoDscp(f *framework.Framework, srcPod, dstPodNamespace, dstPod, dstPodIP, tcpDumpTpl string, dscp int) {
	tcpDumpSync := errgroup.Group{}
	pingSync := errgroup.Group{}

	checkDSCPOnPod := func(pod string, dscp int) error {
		output, err := e2ekubectl.RunKubectl(dstPodNamespace, "exec", pod, "--", "timeout", "10",
			"tcpdump", "-i", "any", "-c", "1", "-v", fmt.Sprintf(tcpDumpTpl, dscp))
		if err != nil {
			return err
		}
		if len(strings.TrimSpace(output)) == 0 {
			return fmt.Errorf("no packets captured")
		}
		return nil
	}

	pingFromSrcPod := func(pod, dst string) error {
		_, err := e2ekubectl.RunKubectl(f.Namespace.Name, "exec", pod, "--", "ping", "-c", "5", dst)
		return err
	}

	tcpDumpSync.Go(func() error {
		return checkDSCPOnPod(dstPod, dscp)
	})
	pingSync.Go(func() error {
		return pingFromSrcPod(srcPod, dstPodIP)
	})
	err := pingSync.Wait()
	gomega.Expect(err).To(gomega.BeNil())
	err = tcpDumpSync.Wait()
	gomega.Expect(err).To(gomega.HaveOccurred())
}

func netcatExpectNoDscp(f *framework.Framework, srcPod, dstPodNamespace, dstPod, dstPodIP, tcpDumpTpl string, port, dscp int) {
	tcpDumpSync := errgroup.Group{}
	netcatSync := errgroup.Group{}

	checkDSCPOnPod := func(pod string, dscp int) error {
		output, err := e2ekubectl.RunKubectl(dstPodNamespace, "exec", pod, "--", "timeout", "10",
			"tcpdump", "-i", "any", "-c", "1", "-v", fmt.Sprintf(tcpDumpTpl, dscp))
		if err != nil {
			return err
		}
		if len(strings.TrimSpace(output)) == 0 {
			return fmt.Errorf("no packets captured")
		}
		return nil
	}

	netcatFromSrcPod := func(pod, dst string) error {
		_, err := e2ekubectl.RunKubectl(f.Namespace.Name, "exec", pod, "--", "bash", "-c", fmt.Sprintf("for i in {1..5}; do nc -vz -w 1 %s %d; sleep 1; done", dst, port))
		return err
	}

	tcpDumpSync.Go(func() error {
		return checkDSCPOnPod(dstPod, dscp)
	})
	netcatSync.Go(func() error {
		return netcatFromSrcPod(srcPod, dstPodIP)
	})
	err := netcatSync.Wait()
	framework.ExpectNoError(err, "Failed to connect to dst pod")
	err = tcpDumpSync.Wait()
	gomega.Expect(err).To(gomega.HaveOccurred())
}
